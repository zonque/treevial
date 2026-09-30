package member

import (
	"fmt"
	"log"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
	"github.com/zonque/treevial/structtree"
)

// refData is what a member holds for one ref: the Go value the cluster agreed
// on, the builder that turns it into objects, and the root it last produced.
//
// The store is not here. One store is shared by every ref, so that a snapshot
// is one packfile and an object two devices happen to have in common is stored
// once.
type refData struct {
	config  *shared.Config
	builder *structtree.Builder
	root    plumbing.Hash
}

// Member is one node's copy of the cluster's state, and the treevial server
// that serves it.
//
// Its refs are published rather than provided: they exist because this member
// holds them, not because somebody is connected, so they are there before the
// first client arrives and outlive the last one to leave.
type Member struct {
	srv *server.Server
	out *log.Logger

	// mu guards everything below it. Entries are applied on one goroutine,
	// so the lock is not there to order them against each other: it is
	// there because a snapshot may be taken and written out while they
	// keep arriving.
	mu    sync.Mutex
	store *objects.Store
	refs  map[string]*refData
	// index is the log index of the last entry applied, which is what a
	// head's Sequence is set from and what a snapshot carries.
	index uint64
}

// New returns a member that will publish its refs on srv and explain itself to
// out.
//
// It takes the server rather than making one so that a test can hand it one
// that listens nowhere.
func New(srv *server.Server, out *log.Logger) *Member {
	return &Member{
		srv:   srv,
		out:   out,
		store: objects.NewStore(),
		refs:  map[string]*refData{},
	}
}

// Store returns the objects behind every ref this member holds.
func (m *Member) Store() *objects.Store {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.store
}

// Roots reports what each ref points at.
func (m *Member) Roots() map[string]plumbing.Hash {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]plumbing.Hash, len(m.refs))
	for ref, data := range m.refs {
		out[ref] = data.root
	}

	return out
}

// Index reports the log index this member has applied up to.
func (m *Member) Index() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.index
}

// Holds says whether this member has any state at all. A member that holds
// nothing has neither applied a load nor restored a snapshot, which is what
// tells a freshly elected leader whether the database still has to be read.
func (m *Member) Holds() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.refs) > 0
}

// load builds every device's configuration and publishes it. The caller holds
// m.mu.
//
// This is the expensive walk — every leaf encoded and hashed — and it happens
// on every member, from the entry rather than from the database. What it leaves
// behind is a builder per ref that can do the cheap incremental builds the
// entries that follow ask for.
func (m *Member) load(set map[string]*shared.Config, index uint64) error {
	// In a fixed order, because the objects each member produces have to
	// agree byte for byte and a Go map hands out its keys in none.
	for _, ref := range sortedRefs(set) {
		config := set[ref]

		builder, err := structtree.NewBuilder(m.store, config)
		if err != nil {
			return fmt.Errorf("builder for %q: %w", ref, err)
		}

		root, err := builder.Build()
		if err != nil {
			return fmt.Errorf("build %q: %w", ref, err)
		}

		m.refs[ref] = &refData{config: config, builder: builder, root: root}

		if err := m.install(ref, root, index); err != nil {
			return err
		}
	}

	m.out.Printf("entry %d: loaded %d device(s) from the database", index, len(set))

	return nil
}

// setMTU applies one field of one device. The caller holds m.mu.
func (m *Member) setMTU(ref string, mtu int, index uint64) error {
	data, held := m.refs[ref]
	if !held {
		// A member replicates every entry the cluster commits, including
		// entries for refs it holds nothing for. Reported as the server
		// would report it, so one check upstream covers both.
		return fmt.Errorf("entry %d names %q: %w", index, ref, server.ErrUnknownRef)
	}

	data.config.Network.Primary.MTU = mtu

	// The field this declares is the field it changed, so the builder
	// encodes that leaf alone and reuses the hashes it already holds for
	// everything else: one blob, and the trees above it.
	root, err := data.builder.Build(&data.config.Network.Primary.MTU)
	if err != nil {
		return fmt.Errorf("build %q: %w", ref, err)
	}

	data.root = root

	if err := m.install(ref, root, index); err != nil {
		return err
	}

	m.out.Printf("entry %d: %s MTU %d -> %s", index, ref, mtu, root)

	return nil
}

// install points a ref at a root, publishing it if this member does not hold it
// yet. The caller holds m.mu.
//
// The sequence is the log index, never a number this node counted for itself.
// It is the one ordinal every member agrees on for a given state, which is what
// lets a client that has been talking to two of them tell which of the heads it
// was pushed supersedes the other.
func (m *Member) install(ref string, root plumbing.Hash, index uint64) error {
	head := server.Head{Hash: root, Sequence: index}

	if _, held := m.srv.Head(ref); held {
		return m.srv.SetHead(ref, head)
	}

	return m.srv.Publish(ref, m.store, head)
}

// Observe implements [server.Watcher], so that a node says who is connected to
// it. It runs on the goroutine serving that subscriber and takes no lock of the
// member's, so it cannot hold up an entry being applied.
func (m *Member) Observe(e server.Event) {
	who := e.Subscription.ClientID
	if who == "" {
		who = e.Subscription.Addr
	}

	if e.Kind == server.Synced {
		m.out.Printf("[%s] %s synced at %s (seq %d)", e.Ref, who, e.Synced, e.Head.Sequence)

		return
	}

	m.out.Printf("[%s] %s %s", e.Ref, who, e.Kind)
}
