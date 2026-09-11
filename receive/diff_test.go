package receive_test

import (
	"bytes"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/holoplot/treevial/internal/demo"
	"github.com/holoplot/treevial/objects"
	"github.com/holoplot/treevial/receive"
)

// feed pushes the whole graph rooted at root through a pack and into g, the way
// a client would receive it.
func feed(t *testing.T, g *receive.Graph, s *objects.Store, root plumbing.Hash) {
	t.Helper()

	hashes, err := s.SelectSince(plumbing.ZeroHash, root)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	var buf bytes.Buffer
	if _, err := s.EncodePack(&buf, hashes); err != nil {
		t.Fatalf("EncodePack: %v", err)
	}

	if err := receive.Interpret(bytes.NewReader(buf.Bytes()), g); err != nil {
		t.Fatalf("Interpret: %v", err)
	}
}

func blobEntry(t *testing.T, s *objects.Store, name, content string) object.TreeEntry {
	t.Helper()

	h, err := s.AddBlob([]byte(content))
	if err != nil {
		t.Fatalf("AddBlob: %v", err)
	}

	return object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h}
}

func tree(t *testing.T, s *objects.Store, entries ...object.TreeEntry) plumbing.Hash {
	t.Helper()

	h, err := s.AddTree(entries)
	if err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	return h
}

func TestDiffOfAnUnchangedTreeIsEmpty(t *testing.T) {
	s := objects.NewStore()
	g := receive.NewGraph()

	root, err := demo.BuildTree(s, "v1")
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	feed(t, g, s, root)

	changes, err := g.Diff(root, root)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if len(changes) != 0 {
		t.Errorf("got %d changes, want none: %v", len(changes), changes)
	}
}

func TestDiffReportsOnlyTheRewrittenLeaf(t *testing.T) {
	s := objects.NewStore()
	g := receive.NewGraph()

	v1, err := demo.BuildTree(s, "v1")
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	feed(t, g, s, v1)

	v2, err := s.ReplaceBlob(v1, "Network/Primary/MTU", []byte("9000"))
	if err != nil {
		t.Fatalf("ReplaceBlob: %v", err)
	}
	feed(t, g, s, v2)

	changes, err := g.Diff(v1, v2)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %v", len(changes), changes)
	}
	if got, want := changes[0].Path, "Network/Primary/MTU"; got != want {
		t.Errorf("path: got %q, want %q", got, want)
	}
	if changes[0].Kind != receive.Modified {
		t.Errorf("kind: got %v, want Modified", changes[0].Kind)
	}
	if got, want := string(changes[0].Content), "9000"; got != want {
		t.Errorf("content: got %q, want %q", got, want)
	}
}

func TestDiffReportsAddedAndDeletedLeaves(t *testing.T) {
	s := objects.NewStore()
	g := receive.NewGraph()

	keep := blobEntry(t, s, "keep", "same\n")

	old := tree(t, s, keep, blobEntry(t, s, "gone", "bye\n"))
	feed(t, g, s, old)

	added := blobEntry(t, s, "fresh", "hello\n")
	new := tree(t, s, added, keep)
	feed(t, g, s, new)

	changes, err := g.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2: %v", len(changes), changes)
	}

	byPath := map[string]receive.Change{}
	for _, c := range changes {
		byPath[c.Path] = c
	}

	if c := byPath["fresh"]; c.Kind != receive.Added || string(c.Content) != "hello\n" {
		t.Errorf("fresh: got kind %v content %q, want Added \"hello\\n\"", c.Kind, c.Content)
	}
	if c := byPath["gone"]; c.Kind != receive.Deleted {
		t.Errorf("gone: got kind %v, want Deleted", c.Kind)
	}
	if _, ok := byPath["keep"]; ok {
		t.Error("unchanged entry reported as a change")
	}
}

func TestDiffDescendsIntoAddedAndDeletedSubtrees(t *testing.T) {
	s := objects.NewStore()
	g := receive.NewGraph()

	oldSub := tree(t, s, blobEntry(t, s, "one", "1\n"), blobEntry(t, s, "two", "2\n"))
	old := tree(t, s, object.TreeEntry{Name: "sub", Mode: filemode.Dir, Hash: oldSub})
	feed(t, g, s, old)

	newSub := tree(t, s, blobEntry(t, s, "three", "3\n"))
	new := tree(t, s, object.TreeEntry{Name: "other", Mode: filemode.Dir, Hash: newSub})
	feed(t, g, s, new)

	changes, err := g.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := map[string]receive.ChangeKind{
		"other/three": receive.Added,
		"sub/one":     receive.Deleted,
		"sub/two":     receive.Deleted,
	}

	if len(changes) != len(want) {
		t.Fatalf("got %d changes, want %d: %v", len(changes), len(want), changes)
	}
	for _, c := range changes {
		kind, ok := want[c.Path]
		if !ok {
			t.Errorf("unexpected change %v", c)

			continue
		}
		if c.Kind != kind {
			t.Errorf("%s: got kind %v, want %v", c.Path, c.Kind, kind)
		}
	}
}

func TestDiffReturnsChangesSortedByPath(t *testing.T) {
	s := objects.NewStore()
	g := receive.NewGraph()

	old := tree(t, s, blobEntry(t, s, "a", "1\n"), blobEntry(t, s, "b", "1\n"), blobEntry(t, s, "c", "1\n"))
	feed(t, g, s, old)

	new := tree(t, s, blobEntry(t, s, "a", "2\n"), blobEntry(t, s, "b", "2\n"), blobEntry(t, s, "c", "2\n"))
	feed(t, g, s, new)

	changes, err := g.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if len(changes) != 3 {
		t.Fatalf("got %d changes, want 3", len(changes))
	}
	for i, want := range []string{"a", "b", "c"} {
		if changes[i].Path != want {
			t.Errorf("change %d: got %q, want %q", i, changes[i].Path, want)
		}
	}
}
