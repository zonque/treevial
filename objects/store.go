// Package objects holds the git object model side of treevial: an in-memory
// object store, the object-set arithmetic that turns a client's "have" set into
// the objects it still needs, and packfile encoding and decoding. Nothing here
// touches the filesystem.
//
// A store moves a whole graph out through [Store.EncodePack] and back in
// through [Store.LoadPack], which is what taking a snapshot of one and
// restoring it from another comes to. It may be read while it is being
// appended to, because adding objects cannot change what an existing root
// reaches; nothing can be removed, so reclaiming one means a new store rather
// than a pruned one.
package objects

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/zonque/treevial/internal/packread"
)

// Store keeps git objects in memory. It is the server's view of the object
// graph; the client never uses one, since it interprets objects as they arrive
// rather than storing them.
//
// A Store may be read while it is being appended to, which is what a server
// behind a consensus layer needs: a push encoding, a snapshot walking and an
// entry adding all at once. Adding cannot change what an existing root
// reaches, so the three do not have to be kept apart.
type Store struct {
	storage *memory.Storage

	// objects guards storage. go-git reads it on its own account — both the
	// packfile encoder and the tree decoder are handed a storer — so what
	// they are given is a [view] that takes this lock per access rather
	// than the storage itself.
	objects sync.RWMutex

	// mu guards decoded, which remembers what a tree object parses to.
	// Several subscribers to one ref walk the same store at the same time,
	// so the memo is read from more goroutines than it is written by.
	mu      sync.RWMutex
	decoded map[plumbing.Hash][]object.TreeEntry
}

// NewStore returns an empty in-memory object store.
func NewStore() *Store {
	return &Store{
		storage: memory.NewStorage(),
		decoded: map[plumbing.Hash][]object.TreeEntry{},
	}
}

// view is the storage as go-git sees it: every access takes the store's lock.
// The lock is held inside a call into go-git, never across one.
type view struct {
	storage *memory.Storage
	mu      *sync.RWMutex
}

// storer returns the view to hand go-git.
func (s *Store) storer() view {
	return view{storage: s.storage, mu: &s.objects}
}

// NewEncodedObject returns an object that is not in the storage yet, so it
// needs no lock until it is set.
func (v view) NewEncodedObject() plumbing.EncodedObject {
	return v.storage.NewEncodedObject()
}

func (v view) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.storage.SetEncodedObject(obj)
}

func (v view) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.storage.EncodedObject(t, h)
}

func (v view) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.storage.IterEncodedObjects(t)
}

func (v view) HasEncodedObject(h plumbing.Hash) error {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.storage.HasEncodedObject(h)
}

func (v view) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.storage.EncodedObjectSize(h)
}

// AddAlternate is part of the storer interface and is not something an
// in-memory store can do; the storage it wraps refuses it too.
func (v view) AddAlternate(remote string) error {
	return v.storage.AddAlternate(remote)
}

// entries returns the entries of the tree at h, parsing the object only the
// first time it is asked for.
//
// Walking dominates what a push costs, and parsing dominates a walk:
// SelectSince walks the whole of the tree a client already holds before it
// can prune anything, and does that again for every subscriber. Since a store
// is append-only and content addressed, a hash always parses to the same
// entries, so what is remembered here can never go stale.
//
// The slice is shared with every later caller and must not be modified.
func (s *Store) entries(h plumbing.Hash) ([]object.TreeEntry, error) {
	s.mu.RLock()
	cached, ok := s.decoded[h]
	s.mu.RUnlock()

	if ok {
		return cached, nil
	}

	tree, err := s.TreeObject(h)
	if err != nil {
		return nil, err
	}

	s.remember(h, tree.Entries)

	return tree.Entries, nil
}

// remember records what a tree parses to.
func (s *Store) remember(h plumbing.Hash, entries []object.TreeEntry) {
	s.mu.Lock()
	s.decoded[h] = entries
	s.mu.Unlock()
}

// AddBlob stores content as a blob and returns its hash.
func (s *Store) AddBlob(content []byte) (plumbing.Hash, error) {
	obj := s.storer().NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))

	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(content); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}

	return s.storer().SetEncodedObject(obj)
}

// AddTree stores entries as a tree object and returns its hash. The entries
// are sorted into git's canonical order first — which compares directories as
// though their names ended in a slash — so callers may pass them in whatever
// order suits them.
func (s *Store) AddTree(entries []object.TreeEntry) (plumbing.Hash, error) {
	sorted := make([]object.TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Sort(object.TreeEntrySorter(sorted))

	tree := &object.Tree{Entries: sorted}

	obj := s.storer().NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}

	hash, err := s.storer().SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	// Writing a tree is the one moment its entries are already in hand, so
	// the walk that follows never has to parse it back.
	s.remember(hash, sorted)

	return hash, nil
}

// TreeObject decodes the tree object at h.
//
// This parses the object every time. It hands back go-git's own *object.Tree,
// which carries a storer and builds an index of its own as it is used, so one
// instance cannot be shared between callers; [Store.Tree] is the cached way in
// for the callers that only want the entries.
func (s *Store) TreeObject(h plumbing.Hash) (*object.Tree, error) {
	obj, err := s.storer().EncodedObject(plumbing.TreeObject, h)
	if err != nil {
		return nil, err
	}

	return object.DecodeTree(s.storer(), obj)
}

// EncodePack writes the given objects to w as a packfile and returns the pack's
// checksum. Delta compression is switched off (a pack window of zero), so every
// object arrives self-contained and a receiver can inflate it without holding
// any base object — which is what lets the client interpret the stream without
// a store of its own.
func (s *Store) EncodePack(w io.Writer, hashes []plumbing.Hash) (plumbing.Hash, error) {
	return packfile.NewEncoder(w, s.storer(), false).Encode(hashes, 0)
}

// Tree returns the entries of the tree at h, and whether this store has it.
//
// This is the shape a structtree source asks for, so a whole value can be
// decoded straight out of a store — which is what a restored snapshot needs
// before it can be built on again.
func (s *Store) Tree(h plumbing.Hash) ([]object.TreeEntry, bool) {
	entries, err := s.entries(h)
	if err != nil {
		return nil, false
	}

	return entries, true
}

// Blob returns the content of the blob at h, and whether this store has it.
// The slice is the caller's; nothing else holds it.
func (s *Store) Blob(h plumbing.Hash) ([]byte, bool) {
	obj, err := s.storer().EncodedObject(plumbing.BlobObject, h)
	if err != nil {
		return nil, false
	}

	r, err := obj.Reader()
	if err != nil {
		return nil, false
	}
	defer r.Close()

	content, err := io.ReadAll(r)
	if err != nil {
		return nil, false
	}

	return content, true
}

// Has reports whether the store holds the object at h.
//
// A store is only ever added to or compacted, and both leave every object a
// tree reaches in place, so holding a tree means holding everything beneath
// it: this answers for a whole graph as well as for one object.
func (s *Store) Has(h plumbing.Hash) bool {
	if h.IsZero() {
		return false
	}

	return s.storer().HasEncodedObject(h) == nil
}

// Compact returns a new store holding what the given roots reach, and how many
// objects it left behind. The zero hash among the roots is ignored, so a ref
// that points nowhere can be passed in with the rest.
//
// Nothing is deleted. The store it reads is untouched, which is what lets a
// compaction run while pushes are encoding from it: each holds the store it
// read through its own pointer and finishes undisturbed, and once the last of
// them is done nothing references it and everything the new store did not take
// is freed.
//
// Moving an object is a map insert. The stored object is shared with the new
// store rather than copied and its hash is already known, so what a compaction
// costs is in the number of objects retained, not in their size.
//
// A root the store cannot walk in full is refused and no store comes back,
// since a compaction that could not see all of what it was keeping would take
// the graph apart.
func (s *Store) Compact(roots ...plumbing.Hash) (*Store, int, error) {
	out := NewStore()

	live := map[plumbing.Hash]bool{}

	for _, root := range roots {
		if root.IsZero() {
			continue
		}

		err := s.walk(root, func(h plumbing.Hash) (bool, error) {
			// Already taken, along with everything beneath it.
			if live[h] {
				return false, nil
			}
			live[h] = true

			obj, err := s.storer().EncodedObject(plumbing.AnyObject, h)
			if err != nil {
				return false, err
			}

			if _, err := out.storer().SetEncodedObject(obj); err != nil {
				return false, err
			}

			return true, nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("compact from %s: %w", root, err)
		}
	}

	// The memo goes with what it describes, so the walks after a compaction
	// are as quick as the ones before it.
	s.mu.RLock()
	for h, entries := range s.decoded {
		if live[h] {
			out.decoded[h] = entries
		}
	}
	s.mu.RUnlock()

	s.objects.RLock()
	held := len(s.storage.Objects)
	s.objects.RUnlock()

	return out, held - len(live), nil
}

// LoadPack adds every object a packfile carries to the store, and returns how
// many it added.
//
// This is the receiving half of [Store.EncodePack]: together they move a whole
// graph from one store to another, which is what a snapshot and its restore
// are. Nothing is assumed about the order the objects arrive in — a tree may
// be stored before anything beneath it, since storing one only records its
// entries.
//
// Each object is checked against the hash it arrived under. The pack's own
// checksum is what catches corruption; this catches the one thing that
// checksum cannot, which is a tree whose entries are not in git's canonical
// order. [Store.AddTree] sorts before hashing, so such a tree would be stored
// under a hash its sender never used, and a store that disagreed with the
// sender about its own hashes would be worse than a refusal.
func (s *Store) LoadPack(r io.Reader) (int, error) {
	l := &loader{store: s}

	if err := packread.Scan(r, l); err != nil {
		return l.loaded, err
	}

	return l.loaded, nil
}

// loader puts what a pack carries into a store.
type loader struct {
	store  *Store
	loaded int
}

func (l *loader) OnPackHeader(uint32) error { return nil }

func (l *loader) OnBlob(h plumbing.Hash, content []byte) error {
	// The handler's content is reused once this returns, and AddBlob copies
	// it into the object it stores.
	got, err := l.store.AddBlob(content)
	if err != nil {
		return err
	}

	return l.check(h, got)
}

func (l *loader) OnTree(h plumbing.Hash, entries []object.TreeEntry) error {
	got, err := l.store.AddTree(entries)
	if err != nil {
		return err
	}

	return l.check(h, got)
}

func (l *loader) OnPackFooter(plumbing.Hash) error { return nil }

// check confirms an object was stored under the hash it arrived under.
func (l *loader) check(arrived, stored plumbing.Hash) error {
	if stored != arrived {
		return fmt.Errorf("pack carries %s but this store puts it at %s", arrived, stored)
	}

	l.loaded++

	return nil
}

// ReplaceBlob rewrites the blob at the given slash-separated path under the
// tree at root, rebuilding every tree on the path, and returns the new root
// hash. Only the objects along that path are new, so an update after such a
// change costs a handful of objects.
//
// The path must already exist; ReplaceBlob does not create entries.
func (s *Store) ReplaceBlob(root plumbing.Hash, path string, content []byte) (plumbing.Hash, error) {
	parts := strings.Split(path, "/")

	var rebuild func(h plumbing.Hash, parts []string) (plumbing.Hash, error)
	rebuild = func(h plumbing.Hash, parts []string) (plumbing.Hash, error) {
		current, err := s.entries(h)
		if err != nil {
			return plumbing.ZeroHash, err
		}

		// Cloned before anything is rewritten: the memo hands out the
		// slice it holds, and this is about to change entries in place.
		entries := slices.Clone(current)

		for i, e := range entries {
			if e.Name != parts[0] {
				continue
			}

			if len(parts) == 1 {
				if entries[i].Hash, err = s.AddBlob(content); err != nil {
					return plumbing.ZeroHash, err
				}
			} else if entries[i].Hash, err = rebuild(e.Hash, parts[1:]); err != nil {
				return plumbing.ZeroHash, err
			}

			return s.AddTree(entries)
		}

		return plumbing.ZeroHash, fmt.Errorf("path element %q not found in tree %s", parts[0], h)
	}

	return rebuild(root, parts)
}
