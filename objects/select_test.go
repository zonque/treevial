package objects_test

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

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
