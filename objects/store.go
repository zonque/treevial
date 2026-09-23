// Package objects holds the git object model side of treevial: an in-memory
// object store, the object-set arithmetic that turns a client's "have" set into
// the objects it still needs, and packfile encoding. Nothing here touches the
// filesystem.
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
	"github.com/go-git/go-git/v5/storage/memory"
)

// Store keeps git objects in memory. It is the server's view of the object
// graph; the client never uses one, since it interprets objects as they arrive
// rather than storing them.
type Store struct {
	storage *memory.Storage

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

	tree, err := s.Tree(h)
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
	obj := s.storage.NewEncodedObject()
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

	return s.storage.SetEncodedObject(obj)
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

	obj := s.storage.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}

	hash, err := s.storage.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	// Writing a tree is the one moment its entries are already in hand, so
	// the walk that follows never has to parse it back.
	s.remember(hash, sorted)

	return hash, nil
}

// Tree decodes the tree object at h.
//
// This parses the object every time. It hands back go-git's own *object.Tree,
// which carries a storer and builds an index of its own as it is used, so one
// instance cannot be shared between callers; [Store.entries] is the cached way
// in for the walks that only want the entries.
func (s *Store) Tree(h plumbing.Hash) (*object.Tree, error) {
	obj, err := s.storage.EncodedObject(plumbing.TreeObject, h)
	if err != nil {
		return nil, err
	}

	return object.DecodeTree(s.storage, obj)
}

// EncodePack writes the given objects to w as a packfile and returns the pack's
// checksum. Delta compression is switched off (a pack window of zero), so every
// object arrives self-contained and a receiver can inflate it without holding
// any base object — which is what lets the client interpret the stream without
// a store of its own.
func (s *Store) EncodePack(w io.Writer, hashes []plumbing.Hash) (plumbing.Hash, error) {
	return packfile.NewEncoder(w, s.storage, false).Encode(hashes, 0)
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
