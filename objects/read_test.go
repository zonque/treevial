package objects_test

import (
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
