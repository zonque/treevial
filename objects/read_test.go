package objects_test

import (
	"slices"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/zonque/treevial/objects"
)

// The read side a structtree source asks for, so a whole value can be decoded
// straight out of a store that has just been loaded from a pack.
func TestTreeAndBlobReadBackWhatWasAdded(t *testing.T) {
	store := objects.NewStore()

	blob, err := store.AddBlob([]byte("1500"))
	if err != nil {
		t.Fatalf("AddBlob: %v", err)
	}

	tree, err := store.AddTree([]object.TreeEntry{
		{Name: "mtu", Mode: filemode.Regular, Hash: blob},
	})
	if err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	got, ok := store.Tree(tree)
	if !ok {
		t.Fatalf("Tree(%s) reported nothing", tree)
	}
	if len(got) != 1 || got[0].Name != "mtu" || got[0].Hash != blob {
		t.Errorf("Tree(%s) = %+v, want the one entry it was given", tree, got)
	}

	content, ok := store.Blob(blob)
	if !ok {
		t.Fatalf("Blob(%s) reported nothing", blob)
	}
	if string(content) != "1500" {
		t.Errorf("Blob(%s) = %q, want %q", blob, content, "1500")
	}

	missing := plumbing.NewHash("1111111111111111111111111111111111111111")

	if _, ok := store.Tree(missing); ok {
		t.Error("Tree reported a tree it never had")
	}
	if _, ok := store.Blob(missing); ok {
		t.Error("Blob reported a blob it never had")
	}

	// A tree asked for as a blob, and the other way round: neither is in
	// the store under that type, whatever else it holds.
	if _, ok := store.Blob(tree); ok {
		t.Error("Blob reported a tree")
	}
}

// TestAddTreeOrdersEntriesAsGitDoes pins the rule the ordering turns on: git
// compares a directory as though its name ended in a slash. The slash is 0x2f,
// so a directory "ab" sorts after a file "ab-" and before a file "ab0" — it
// lands between two names a plain comparison would put it before. Get this
// wrong and every tree holding such a set is stored under a hash git itself
// would not use.
func TestAddTreeOrdersEntriesAsGitDoes(t *testing.T) {
	s := objects.NewStore()

	blob, err := s.AddBlob([]byte("x"))
	if err != nil {
		t.Fatalf("AddBlob: %v", err)
	}

	sub, err := s.AddTree([]object.TreeEntry{{Name: "leaf", Mode: filemode.Regular, Hash: blob}})
	if err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	dir := object.TreeEntry{Name: "ab", Mode: filemode.Dir, Hash: sub}
	file := object.TreeEntry{Name: "ab-", Mode: filemode.Regular, Hash: blob}
	other := object.TreeEntry{Name: "ab0", Mode: filemode.Regular, Hash: blob}

	// Every way round, since the caller may pass them in any order.
	orders := [][]object.TreeEntry{
		{dir, file, other},
		{file, dir, other},
		{other, file, dir},
		{dir, other, file},
	}

	var first plumbing.Hash

	for i, entries := range orders {
		root, err := s.AddTree(entries)
		if err != nil {
			t.Fatalf("AddTree: %v", err)
		}

		if i == 0 {
			first = root

			continue
		}

		if root != first {
			t.Errorf("order %d stored as %s, want %s: the input order must not matter", i, root, first)
		}
	}

	stored, ok := s.Tree(first)
	if !ok {
		t.Fatal("the store does not hold the tree it just wrote")
	}

	want := []string{"ab-", "ab", "ab0"}

	got := make([]string, 0, len(stored))
	for _, e := range stored {
		got = append(got, e.Name)
	}

	if !slices.Equal(got, want) {
		t.Errorf("stored order %v, want %v", got, want)
	}
}
