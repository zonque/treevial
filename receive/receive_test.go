package receive_test

import (
	"bytes"
	"sort"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/holoplot/gats/internal/demo"
	"github.com/holoplot/gats/objects"
	"github.com/holoplot/gats/receive"
)

// recorder captures the callbacks in the order they fire, so a test can assert
// that objects are handed over one at a time during the read rather than in a
// batch at the end.
type recorder struct {
	count  uint32
	order  []string
	blobs  map[plumbing.Hash][]byte
	trees  map[plumbing.Hash][]object.TreeEntry
	footer plumbing.Hash
}

func newRecorder() *recorder {
	return &recorder{
		blobs: map[plumbing.Hash][]byte{},
		trees: map[plumbing.Hash][]object.TreeEntry{},
	}
}

func (r *recorder) OnPackHeader(count uint32) error {
	r.count = count
	r.order = append(r.order, "header")

	return nil
}

func (r *recorder) OnBlob(h plumbing.Hash, content []byte) error {
	r.blobs[h] = append([]byte(nil), content...)
	r.order = append(r.order, "blob")

	return nil
}

func (r *recorder) OnTree(h plumbing.Hash, entries []object.TreeEntry) error {
	r.trees[h] = entries
	r.order = append(r.order, "tree")

	return nil
}

func (r *recorder) OnPackFooter(checksum plumbing.Hash) error {
	r.footer = checksum
	r.order = append(r.order, "footer")

	return nil
}

// demoPack builds the ten-leaf tree and encodes the whole graph as a packfile.
func demoPack(t *testing.T) (*objects.Store, plumbing.Hash, []byte) {
	t.Helper()

	s := objects.NewStore()

	root, err := demo.BuildTree(s, "v1")
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	hashes, err := s.SelectSince(plumbing.ZeroHash, root)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	var buf bytes.Buffer
	if _, err := s.EncodePack(&buf, hashes); err != nil {
		t.Fatalf("EncodePack: %v", err)
	}

	return s, root, buf.Bytes()
}

func TestInterpretDecodesEveryObjectInTheStream(t *testing.T) {
	_, _, pack := demoPack(t)

	rec := newRecorder()
	if err := receive.Interpret(bytes.NewReader(pack), rec); err != nil {
		t.Fatalf("Interpret: %v", err)
	}

	if want := uint32(14); rec.count != want {
		t.Errorf("pack header announced %d objects, want %d", rec.count, want)
	}
	if want := 10; len(rec.blobs) != want {
		t.Errorf("got %d blobs, want %d", len(rec.blobs), want)
	}
	if want := 4; len(rec.trees) != want {
		t.Errorf("got %d trees, want %d", len(rec.trees), want)
	}
	if rec.footer.IsZero() {
		t.Error("pack footer checksum missing")
	}
}

func TestInterpretReportsObjectOrderEndingWithTheFooter(t *testing.T) {
	_, _, pack := demoPack(t)

	rec := newRecorder()
	if err := receive.Interpret(bytes.NewReader(pack), rec); err != nil {
		t.Fatalf("Interpret: %v", err)
	}

	if got := rec.order[0]; got != "header" {
		t.Errorf("first callback was %q, want \"header\"", got)
	}
	if got := rec.order[len(rec.order)-1]; got != "footer" {
		t.Errorf("last callback was %q, want \"footer\"", got)
	}
	t.Logf("callback order: %v", rec.order)
}

func TestInterpretHandsOverContentMatchingTheSource(t *testing.T) {
	s, root, pack := demoPack(t)

	rec := newRecorder()
	if err := receive.Interpret(bytes.NewReader(pack), rec); err != nil {
		t.Fatalf("Interpret: %v", err)
	}

	tree, err := s.Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}

	var leaf09 plumbing.Hash
	for _, e := range tree.Entries {
		if e.Name == "leaf-09" {
			leaf09 = e.Hash
		}
	}

	got, ok := rec.blobs[leaf09]
	if !ok {
		t.Fatalf("blob %s never delivered", leaf09)
	}
	if want := "leaf-09 v1\n"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestInterpretFailsOnATruncatedStream(t *testing.T) {
	_, _, pack := demoPack(t)

	err := receive.Interpret(bytes.NewReader(pack[:len(pack)/2]), newRecorder())
	if err == nil {
		t.Fatal("Interpret accepted a truncated pack")
	}
}

func TestGraphRebuildsTheHierarchyFromCallbacksAlone(t *testing.T) {
	_, root, pack := demoPack(t)

	g := receive.NewGraph()
	if err := receive.Interpret(bytes.NewReader(pack), g); err != nil {
		t.Fatalf("Interpret: %v", err)
	}

	leaves, err := g.Leaves(root)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}

	got := make([]string, 0, len(leaves))
	for p := range leaves {
		got = append(got, p)
	}
	sort.Strings(got)

	want := []string{
		"a/leaf-00", "a/leaf-01", "a/leaf-02", "a/leaf-03",
		"b/c/leaf-04", "b/c/leaf-05", "b/c/leaf-06", "b/c/leaf-07",
		"leaf-08", "leaf-09",
	}

	if len(got) != len(want) {
		t.Fatalf("got %d leaves %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("leaf %d: got %q, want %q", i, got[i], want[i])
		}
	}
	if w := "leaf-04 v1\n"; string(leaves["b/c/leaf-04"]) != w {
		t.Errorf("content of b/c/leaf-04: got %q, want %q", leaves["b/c/leaf-04"], w)
	}
}
