package objects_test

import (
	"io"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/zonque/treevial/objects"
)

// Adding to a store while it is being read is what a server behind a consensus
// layer does continuously: an entry appends, a push encodes, and a snapshot
// walks. Adding cannot change what an existing root reaches, so all three may
// overlap — provided the store says so rather than leaving it to luck.
func TestAStoreMayBeReadWhileItIsAppendedTo(t *testing.T) {
	store := objects.NewStore()

	root := treeOf(t, store, "settled", 8)

	var wg sync.WaitGroup

	// The appending side: new objects, none of them under root.
	wg.Add(1)
	go func() {
		defer wg.Done()

		for i := range 200 {
			treeOf(t, store, "moving", i)
		}
	}()

	// The reading side: select and encode the settled root over and over.
	wg.Add(1)
	go func() {
		defer wg.Done()

		for range 200 {
			hashes, err := store.SelectSince(plumbing.ZeroHash, root)
			if err != nil {
				t.Errorf("SelectSince: %v", err)

				return
			}

			if _, err := store.EncodePack(io.Discard, hashes); err != nil {
				t.Errorf("EncodePack: %v", err)

				return
			}
		}
	}()

	wg.Wait()
}

// treeOf stores one blob under a one-entry tree and returns the tree's hash.
func treeOf(t *testing.T, store *objects.Store, name string, n int) plumbing.Hash {
	t.Helper()

	blob, err := store.AddBlob([]byte{byte(n), byte(n >> 8)})
	if err != nil {
		t.Errorf("AddBlob: %v", err)

		return plumbing.ZeroHash
	}

	tree, err := store.AddTree([]object.TreeEntry{
		{Name: name, Mode: filemode.Regular, Hash: blob},
	})
	if err != nil {
		t.Errorf("AddTree: %v", err)
	}

	return tree
}
