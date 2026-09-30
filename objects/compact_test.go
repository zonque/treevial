package objects_test

import (
	"io"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/objects"
)

func TestCompactKeepsWhatARootReachesAndDropsTheRest(t *testing.T) {
	store := objects.NewStore()

	kept := treeOf(t, store, "kept", 1)
	dropped := treeOf(t, store, "dropped", 2)

	compacted, gone, err := store.Compact(kept)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// One blob and one tree for the root that went.
	if gone != 2 {
		t.Errorf("dropped %d objects, want 2", gone)
	}

	if !compacted.Has(kept) {
		t.Error("the compacted store does not hold the root it was given")
	}
	if compacted.Has(dropped) {
		t.Error("the compacted store holds a root nothing reached")
	}

	// The store it read is untouched, which is what lets a push go on
	// reading it while this happens.
	if !store.Has(dropped) {
		t.Error("Compact changed the store it read")
	}

	hashes, err := compacted.SelectSince(plumbing.ZeroHash, kept)
	if err != nil {
		t.Fatalf("SelectSince on the compacted store: %v", err)
	}
	if len(hashes) != 2 {
		t.Errorf("the compacted store reaches %d objects from %s, want 2", len(hashes), kept)
	}

	if _, err := compacted.EncodePack(io.Discard, hashes); err != nil {
		t.Errorf("EncodePack on the compacted store: %v", err)
	}
}

func TestCompactFromNothingGivesAnEmptyStore(t *testing.T) {
	store := objects.NewStore()

	root := treeOf(t, store, "everything", 1)

	compacted, gone, err := store.Compact()
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if gone != 2 {
		t.Errorf("dropped %d objects, want 2", gone)
	}
	if compacted.Has(root) {
		t.Error("a store compacted from no roots holds something")
	}
}

// The zero hash among the roots is ignored, so a void ref's head can be passed
// in with the rest rather than filtered out by every caller.
func TestCompactIgnoresTheZeroHash(t *testing.T) {
	store := objects.NewStore()

	root := treeOf(t, store, "kept", 1)

	compacted, _, err := store.Compact(plumbing.ZeroHash, root)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if !compacted.Has(root) {
		t.Error("the compacted store does not hold the root it was given")
	}
}

func TestARetainedTreeReadsBackUnchanged(t *testing.T) {
	store := objects.NewStore()

	root := treeOf(t, store, "kept", 7)

	before, ok := store.Tree(root)
	if !ok {
		t.Fatal("the store does not hold the tree it built")
	}

	compacted, _, err := store.Compact(root)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	after, ok := compacted.Tree(root)
	if !ok {
		t.Fatal("the compacted store does not hold the retained tree")
	}

	if len(after) != len(before) {
		t.Fatalf("the retained tree has %d entries, want %d", len(after), len(before))
	}
	for i := range after {
		if after[i] != before[i] {
			t.Errorf("entry %d = %+v, want %+v", i, after[i], before[i])
		}
	}

	was, ok := store.Blob(before[0].Hash)
	if !ok {
		t.Fatal("the store does not hold the blob it added")
	}

	is, ok := compacted.Blob(before[0].Hash)
	if !ok {
		t.Fatal("the compacted store does not hold the retained blob")
	}
	if string(is) != string(was) {
		t.Errorf("the retained blob reads %q, want %q", is, was)
	}
}

func TestHasAnswersForWhatAStoreHolds(t *testing.T) {
	store := objects.NewStore()

	root := treeOf(t, store, "held", 1)

	if !store.Has(root) {
		t.Error("Has denies a tree the store built")
	}
	if store.Has(plumbing.NewHash("1111111111111111111111111111111111111111")) {
		t.Error("Has claims an object the store never had")
	}
	if store.Has(plumbing.ZeroHash) {
		t.Error("Has claims the zero hash")
	}
}

func TestCompactRefusesARootItCannotWalk(t *testing.T) {
	store := objects.NewStore()

	stranger := plumbing.NewHash("1111111111111111111111111111111111111111")

	if _, _, err := store.Compact(stranger); err == nil {
		t.Error("Compact accepted a root the store does not hold")
	}
}

// Compacting reads the old store and writes a new one, so it may run while a
// push is encoding — which is the whole reason a sweep does not delete.
func TestAStoreMayBeCompactedWhileItIsRead(t *testing.T) {
	store := objects.NewStore()

	root := treeOf(t, store, "settled", 8)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		for range 100 {
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

	wg.Add(1)
	go func() {
		defer wg.Done()

		for range 100 {
			if _, _, err := store.Compact(root); err != nil {
				t.Errorf("Compact: %v", err)

				return
			}
		}
	}()

	wg.Wait()
}
