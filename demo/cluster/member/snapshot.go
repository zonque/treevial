package member

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hashicorp/raft"

	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

// header is the first line of a snapshot: where each ref pointed, and the log
// index it pointed there at. Hashes are written as hex so that the head of a
// snapshot is something a person can read.
type header struct {
	Index uint64            `json:"index"`
	Refs  map[string]string `json:"refs"`
}

// snapshot is a point-in-time view of a member.
//
// Taking one is the part that is usually hard and here is not. The store is
// append-only and content addressed, so a root hash already is a view of the
// state as it was: what happens after it was read cannot change what it
// reaches. The whole of a snapshot is therefore a handful of hashes and the
// store to read them out of, and nothing is copied or locked to get it.
type snapshot struct {
	index uint64
	roots map[string]plumbing.Hash
	store *objects.Store
	out   *log.Logger
}

// Snapshot implements [raft.FSM]. It returns quickly because there is nothing
// to do: entries cannot be applied while it runs, so it reads what the member
// points at and gets out of the way.
func (m *Member) Snapshot() (raft.FSMSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	roots := make(map[string]plumbing.Hash, len(m.refs))
	for ref, data := range m.refs {
		roots[ref] = data.root
	}

	return &snapshot{index: m.index, roots: roots, store: m.store, out: m.out}, nil
}

// Persist writes the snapshot out: one header line, then one packfile holding
// everything the roots reach.
//
// Entries keep being applied while this runs, and that is safe rather than
// tolerated. Adding objects to a store cannot change what an already-named root
// reaches, and a store may be read while it is written, so what lands in the
// pack is the state as it was when the snapshot was taken however long the
// write takes.
func (s *snapshot) Persist(sink raft.SnapshotSink) error {
	if err := s.write(sink); err != nil {
		sink.Cancel()

		return err
	}

	return sink.Close()
}

func (s *snapshot) write(w io.Writer) error {
	h := header{Index: s.index, Refs: make(map[string]string, len(s.roots))}
	for ref, root := range s.roots {
		h.Refs[ref] = root.String()
	}

	line, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encode snapshot header: %w", err)
	}

	if _, err := w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write snapshot header: %w", err)
	}

	// The refs share a store and may share objects, so the union is taken
	// before anything is encoded — a pack naming the same object twice is
	// not one git would read back.
	union := map[plumbing.Hash]struct{}{}

	for _, ref := range sortedRefs(s.roots) {
		reachable, err := s.store.SelectSince(plumbing.ZeroHash, s.roots[ref])
		if err != nil {
			return fmt.Errorf("objects behind %q: %w", ref, err)
		}

		for _, h := range reachable {
			union[h] = struct{}{}
		}
	}

	hashes := make([]plumbing.Hash, 0, len(union))
	for h := range union {
		hashes = append(hashes, h)
	}

	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })

	if _, err := s.store.EncodePack(w, hashes); err != nil {
		return fmt.Errorf("encode snapshot pack: %w", err)
	}

	s.out.Printf("snapshot at entry %d: %d ref(s), %d object(s)", s.index, len(s.roots), len(hashes))

	return nil
}

// Release implements [raft.FSMSnapshot]. A snapshot holds nothing of its own —
// the store it read from belongs to the member and outlives it — so there is
// nothing to give back.
func (s *snapshot) Release() {}

// Restore implements [raft.FSM]. It is how a member that was not there for the
// entries gets their result: the objects arrive in a pack, and the state is
// read back out of them.
//
// It is not called concurrently with anything else.
func (m *Member) Restore(r io.ReadCloser) error {
	defer r.Close()

	buffered := bufio.NewReader(r)

	line, err := buffered.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read snapshot header: %w", err)
	}

	var h header
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		return fmt.Errorf("decode snapshot header: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	loaded, err := m.store.LoadPack(buffered)
	if err != nil {
		return fmt.Errorf("load snapshot pack: %w", err)
	}

	refs := sortedRefs(h.Refs)

	restored := map[string]*refData{}

	for _, ref := range refs {
		root := plumbing.NewHash(h.Refs[ref])

		config := &shared.Config{}

		// The value, read back out of the tree that arrived.
		if err := structtree.ApplySince(config, m.store, plumbing.ZeroHash, root); err != nil {
			return fmt.Errorf("decode %q from the snapshot: %w", ref, err)
		}

		builder, err := structtree.NewBuilder(m.store, config)
		if err != nil {
			return fmt.Errorf("builder for %q: %w", ref, err)
		}

		// One full walk, which is what leaves the builder able to do the
		// cheap incremental builds the entries that follow will ask for
		// — and, because the hashing is deterministic, what turns "did I
		// restore this correctly?" into an equality test.
		got, err := builder.Build()
		if err != nil {
			return fmt.Errorf("rebuild %q: %w", ref, err)
		}

		if got != root {
			return fmt.Errorf("%q rebuilt as %s, but the snapshot says %s", ref, got, root)
		}

		restored[ref] = &refData{config: config, builder: builder, root: root}
	}

	// A ref the snapshot does not name is one the cluster no longer holds.
	for ref := range m.refs {
		if _, named := restored[ref]; named {
			continue
		}

		if err := m.srv.Unpublish(ref); err != nil {
			return fmt.Errorf("unpublish %q: %w", ref, err)
		}
	}

	m.refs = restored
	m.index = h.Index

	for _, ref := range refs {
		if err := m.install(ref, restored[ref].root, h.Index); err != nil {
			return fmt.Errorf("install %q: %w", ref, err)
		}
	}

	m.out.Printf("restored %d ref(s) from a snapshot at entry %d, %d object(s) loaded",
		len(refs), h.Index, loaded)

	return nil
}
