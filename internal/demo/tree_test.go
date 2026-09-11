package demo_test

import (
	"sort"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/holoplot/gats/internal/demo"
	"github.com/holoplot/gats/objects"
)

// walk collects every path->hash mapping reachable from root, so a test can
// assert on the shape of a tree without caring how it was assembled.
func walk(t *testing.T, s *objects.Store, root plumbing.Hash, prefix string, out map[string]plumbing.Hash) {
	t.Helper()

	tree, err := s.Tree(root)
	if err != nil {
		t.Fatalf("Tree(%s): %v", root, err)
	}

	for _, e := range tree.Entries {
		path := prefix + e.Name
		if e.Mode == filemode.Dir {
			walk(t, s, e.Hash, path+"/", out)
			continue
		}
		out[path] = e.Hash
	}
}

func TestBuildTreeHasTenBlobLeaves(t *testing.T) {
	s := objects.NewStore()

	root, err := demo.BuildTree(s, "v1")
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	leaves := map[string]plumbing.Hash{}
	walk(t, s, root, "", leaves)

	want := []string{
		"a/leaf-00", "a/leaf-01", "a/leaf-02", "a/leaf-03",
		"b/c/leaf-04", "b/c/leaf-05", "b/c/leaf-06", "b/c/leaf-07",
		"leaf-08", "leaf-09",
	}

	got := make([]string, 0, len(leaves))
	for p := range leaves {
		got = append(got, p)
	}
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("got %d leaves %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("leaf %d: got %q, want %q", i, got[i], want[i])
		}
	}
}
