package receive_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/receive"
	"github.com/zonque/treevial/structtree"
)

// marked matches one changeset row: a marker, then exactly what Listing emits.
var marked = regexp.MustCompile(`^([+~-]) (\d{6}) (tree|blob) ([0-9a-f]{40})\t(\S.*)$`)

type change struct {
	mark string
	kind string
	hash string
	path string
}

func parseChangeset(t *testing.T, out string) []change {
	t.Helper()

	if out == "" {
		return nil
	}

	var rows []change

	for _, raw := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		m := marked.FindStringSubmatch(raw)
		if m == nil {
			t.Fatalf("row %q does not parse", raw)
		}

		rows = append(rows, change{mark: m[1], kind: m[3], hash: m[4], path: m[5]})
	}

	return rows
}

// twoStates stores both values and gives a graph holding every object of each.
func twoStates(t *testing.T, before, after any) (*receive.Graph, plumbing.Hash, plumbing.Hash) {
	t.Helper()

	store := objects.NewStore()
	graph := receive.NewGraph()

	return graph, feedState(t, store, graph, before), feedState(t, store, graph, after)
}

func feedState(t *testing.T, store *objects.Store, graph *receive.Graph, v any) plumbing.Hash {
	t.Helper()

	root, err := structtree.Build(store, v)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	push(t, store, graph, plumbing.ZeroHash, root)

	return root
}

func TestAChangesetShowsTheBlobAndTheTreesAboveIt(t *testing.T) {
	before := shared.Example("printer-7")

	after := shared.Example("printer-7")
	after.Network.Primary.MTU = 9000

	graph, old, new := twoStates(t, before, after)

	out, err := graph.ListingSince(old, new)
	if err != nil {
		t.Fatalf("ListingSince: %v", err)
	}

	rows := parseChangeset(t, out)

	var paths []string
	for _, r := range rows {
		paths = append(paths, r.path)

		if r.mark != "~" {
			t.Errorf("%s: marked %q, want %q", r.path, r.mark, "~")
		}
	}

	want := []string{"Network", "Network/Primary", "Network/Primary/MTU"}
	if len(paths) != len(want) {
		t.Fatalf("got %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("row %d: got %q, want %q", i, paths[i], want[i])
		}
	}
}

func TestAChangesetOfNothingIsEmpty(t *testing.T) {
	graph, old, _ := twoStates(t, shared.Example("printer-7"), shared.Example("printer-7"))

	out, err := graph.ListingSince(old, old)
	if err != nil {
		t.Fatalf("ListingSince: %v", err)
	}

	if out != "" {
		t.Errorf("got %q, want nothing", out)
	}
}

func TestAChangesetFromNothingIsTheWholeTreeAdded(t *testing.T) {
	graph, _, new := twoStates(t, shared.Example("printer-7"), shared.Example("printer-7"))

	out, err := graph.ListingSince(plumbing.ZeroHash, new)
	if err != nil {
		t.Fatalf("ListingSince: %v", err)
	}

	rows := parseChangeset(t, out)

	whole, err := graph.Listing(new)
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}

	if got, want := len(rows), len(strings.Split(strings.TrimSuffix(whole, "\n"), "\n")); got != want {
		t.Errorf("got %d rows, want the %d of a whole listing", got, want)
	}
	for _, r := range rows {
		if r.mark != "+" {
			t.Errorf("%s: marked %q, want %q", r.path, r.mark, "+")
		}
	}
}

func TestAChangesetNamesEveryObjectOfAnAddedSubtree(t *testing.T) {
	before := shared.Example("printer-7")

	after := shared.Example("printer-7")
	after.Network.Secondary = &shared.Interface{Address: "10.0.0.2", MTU: 9000}

	graph, old, new := twoStates(t, before, after)

	out, err := graph.ListingSince(old, new)
	if err != nil {
		t.Fatalf("ListingSince: %v", err)
	}

	marks := map[string]string{}
	for _, r := range parseChangeset(t, out) {
		marks[r.path] = r.mark
	}

	for path, want := range map[string]string{
		"Network":                   "~",
		"Network/Secondary":         "+",
		"Network/Secondary/Address": "+",
		"Network/Secondary/MTU":     "+",
	} {
		if marks[path] != want {
			t.Errorf("%s: marked %q, want %q", path, marks[path], want)
		}
	}

	if _, ok := marks["Network/Primary/MTU"]; ok {
		t.Error("an untouched blob was listed")
	}
}

func TestAChangesetNamesEveryObjectOfARemovedSubtree(t *testing.T) {
	before := shared.Example("printer-7")
	before.Network.Secondary = &shared.Interface{Address: "10.0.0.2", MTU: 9000}

	after := shared.Example("printer-7")

	graph, old, new := twoStates(t, before, after)

	out, err := graph.ListingSince(old, new)
	if err != nil {
		t.Fatalf("ListingSince: %v", err)
	}

	marks := map[string]string{}
	for _, r := range parseChangeset(t, out) {
		marks[r.path] = r.mark
	}

	for _, path := range []string{"Network/Secondary", "Network/Secondary/Address", "Network/Secondary/MTU"} {
		if marks[path] != "-" {
			t.Errorf("%s: marked %q, want %q", path, marks[path], "-")
		}
	}
}

func TestAChangesetReportsAPathThatChangedKind(t *testing.T) {
	s := objects.NewStore()

	leaf, err := s.AddBlob([]byte("whole"))
	if err != nil {
		t.Fatal(err)
	}

	inner, err := s.AddTree([]object.TreeEntry{{Name: "part", Mode: filemode.Regular, Hash: leaf}})
	if err != nil {
		t.Fatal(err)
	}

	old, err := s.AddTree([]object.TreeEntry{{Name: "thing", Mode: filemode.Regular, Hash: leaf}})
	if err != nil {
		t.Fatal(err)
	}

	new, err := s.AddTree([]object.TreeEntry{{Name: "thing", Mode: filemode.Dir, Hash: inner}})
	if err != nil {
		t.Fatal(err)
	}

	graph := receive.NewGraph()
	for _, root := range []plumbing.Hash{old, new} {
		push(t, s, graph, plumbing.ZeroHash, root)
	}

	out, err := graph.ListingSince(old, new)
	if err != nil {
		t.Fatalf("ListingSince: %v", err)
	}

	rows := parseChangeset(t, out)

	// A blob where a tree now stands is the old thing going and a new one
	// arriving, not one thing changing.
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %v", len(rows), rows)
	}
	if rows[0].mark != "-" || rows[0].kind != "blob" || rows[0].path != "thing" {
		t.Errorf("first row: %+v", rows[0])
	}
	if rows[1].mark != "+" || rows[1].kind != "tree" || rows[1].path != "thing" {
		t.Errorf("second row: %+v", rows[1])
	}
	if rows[2].mark != "+" || rows[2].path != "thing/part" {
		t.Errorf("third row: %+v", rows[2])
	}
}

func TestAChangesetReportsATreeItDoesNotHave(t *testing.T) {
	graph := receive.NewGraph()

	if _, err := graph.ListingSince(plumbing.ZeroHash, plumbing.NewHash("1111111111111111111111111111111111111111")); err == nil {
		t.Error("ListingSince accepted a tree the graph does not have")
	}
}
