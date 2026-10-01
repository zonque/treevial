package objects_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
)

func TestSelectSinceNothingReturnsWholeGraph(t *testing.T) {
	s := objects.NewStore()

	root, err := shared.BuildTree(s, "v1")
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	got, err := s.SelectSince(plumbing.ZeroHash, root)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	// 11 blobs plus the root, Device, Location, Network, Primary and Audio
	// trees.
	if want := 17; len(got) != want {
		t.Errorf("got %d objects, want %d", len(got), want)
	}
}

func TestSelectSinceTheSameTreeReturnsNothing(t *testing.T) {
	s := objects.NewStore()

	root, err := shared.BuildTree(s, "v1")
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	got, err := s.SelectSince(root, root)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("got %d objects, want none for an already-synced client", len(got))
	}
}

func TestSelectSinceReturnsOnlyThePathToAChangedLeaf(t *testing.T) {
	s := objects.NewStore()

	v1, err := shared.BuildTree(s, "v1")
	if err != nil {
		t.Fatalf("BuildTree v1: %v", err)
	}

	v2, err := s.ReplaceBlob(v1, "Network/Primary/MTU", []byte("9000"))
	if err != nil {
		t.Fatalf("ReplaceBlob: %v", err)
	}

	got, err := s.SelectSince(v1, v2)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	// The rewritten blob plus the c, b and root trees on its path; every
	// subtree the change did not touch is pruned.
	if want := 4; len(got) != want {
		t.Errorf("got %d objects, want %d", len(got), want)
	}

	for _, h := range got {
		if h == v1 {
			t.Error("SelectSince returned the tree the client already had")
		}
	}
}

func TestSelectSinceFailsWhenTheClientClaimsAnUnknownState(t *testing.T) {
	s := objects.NewStore()

	root, err := shared.BuildTree(s, "v1")
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	unknown := plumbing.NewHash("1111111111111111111111111111111111111111")

	if _, err := s.SelectSince(unknown, root); err == nil {
		t.Error("SelectSince accepted a synced hash the store does not have")
	}
}

// pathMove builds a store holding one subtree reached by two different names,
// padded with n blobs of its own so the store is larger than the move. It
// returns the two roots: the subtree sits at "a" in the first and at "b" in
// the second, so nothing about it has changed but the name leading to it.
func pathMove(t *testing.T, n int) (*objects.Store, plumbing.Hash, plumbing.Hash) {
	t.Helper()

	s := objects.NewStore()

	blob, err := s.AddBlob([]byte("payload"))
	if err != nil {
		t.Fatalf("AddBlob: %v", err)
	}

	sub, err := s.AddTree([]object.TreeEntry{{Name: "leaf", Mode: filemode.Regular, Hash: blob}})
	if err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	padding := make([]object.TreeEntry, 0, n)

	for i := range n {
		h, err := s.AddBlob(fmt.Appendf(nil, "padding-%d", i))
		if err != nil {
			t.Fatalf("AddBlob: %v", err)
		}

		padding = append(padding, object.TreeEntry{
			Name: fmt.Sprintf("pad-%d", i),
			Mode: filemode.Regular,
			Hash: h,
		})
	}

	roots := make([]plumbing.Hash, 0, 2)

	for _, name := range []string{"a", "b"} {
		entries := append(slices.Clone(padding),
			object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: sub})

		root, err := s.AddTree(entries)
		if err != nil {
			t.Fatalf("AddTree: %v", err)
		}

		roots = append(roots, root)
	}

	return s, roots[0], roots[1]
}

// TestSelectSinceResendsASubtreeThatMovedPath pins the price of comparing the
// two trees position by position: a subtree the client already holds, reached
// by a different name, is selected again. It is a superset of what the client
// needs, which is the safe direction, and the padding here keeps the move small
// enough that the exact selection is not worth reaching for.
func TestSelectSinceResendsASubtreeThatMovedPath(t *testing.T) {
	s, from, to := pathMove(t, 10)

	got, err := s.SelectSince(from, to)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	// The new root, the subtree under its new name, and the blob beneath it.
	if want := 3; len(got) != want {
		t.Errorf("got %d objects, want %d", len(got), want)
	}
}

// TestSelectSinceFallsBackToTheExactSelectionForABulkMove is the other side of
// that bargain. When comparing position by position would select most of what
// the store holds, the objects the client already has are worth establishing in
// full: here every subtree was renamed, and all the client actually needs is
// the new root.
func TestSelectSinceFallsBackToTheExactSelectionForABulkMove(t *testing.T) {
	s := objects.NewStore()

	subs := make([]plumbing.Hash, 0, 6)

	for i := range 6 {
		blob, err := s.AddBlob(fmt.Appendf(nil, "value-%d", i))
		if err != nil {
			t.Fatalf("AddBlob: %v", err)
		}

		sub, err := s.AddTree([]object.TreeEntry{{Name: "leaf", Mode: filemode.Regular, Hash: blob}})
		if err != nil {
			t.Fatalf("AddTree: %v", err)
		}

		subs = append(subs, sub)
	}

	root := func(prefix string) plumbing.Hash {
		entries := make([]object.TreeEntry, 0, len(subs))
		for i, sub := range subs {
			entries = append(entries, object.TreeEntry{
				Name: fmt.Sprintf("%s%d", prefix, i),
				Mode: filemode.Dir,
				Hash: sub,
			})
		}

		h, err := s.AddTree(entries)
		if err != nil {
			t.Fatalf("AddTree: %v", err)
		}

		return h
	}

	from, to := root("a"), root("b")

	got, err := s.SelectSince(from, to)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	if want := 1; len(got) != want {
		t.Errorf("got %d objects, want %d: every subtree is one the client holds", len(got), want)
	}
}

// TestSelectSinceFailsWhenAnUnknownStateNamesTheTargetTree is the unknown
// baseline again, in the one shape a comparison could answer for without ever
// reading it.
func TestSelectSinceFailsWhenAnUnknownStateNamesTheTargetTree(t *testing.T) {
	s := objects.NewStore()

	if _, err := shared.BuildTree(s, "v1"); err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	unknown := plumbing.NewHash("1111111111111111111111111111111111111111")

	if _, err := s.SelectSince(unknown, unknown); err == nil {
		t.Error("SelectSince accepted a synced hash the store does not have")
	}
}
