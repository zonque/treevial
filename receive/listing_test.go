package receive_test

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/receive"
)

// line matches one git ls-tree style row: mode, type, hash, tab, path.
var line = regexp.MustCompile(`^(\d{6}) (tree|blob) ([0-9a-f]{40})\t(\S.*)$`)

type listed struct {
	mode string
	kind string
	hash string
	path string
}

func parseListing(t *testing.T, out string) []listed {
	t.Helper()

	var rows []listed

	for _, raw := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		m := line.FindStringSubmatch(raw)
		if m == nil {
			t.Fatalf("line %q does not parse", raw)
		}

		rows = append(rows, listed{mode: m[1], kind: m[2], hash: m[3], path: m[4]})
	}

	return rows
}

func listingOf(t *testing.T, root plumbing.Hash, pack []byte) []listed {
	t.Helper()

	g := receive.NewGraph()
	if err := receive.Interpret(bytes.NewReader(pack), g); err != nil {
		t.Fatalf("Interpret: %v", err)
	}

	out, err := g.Listing(root)
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}

	return parseListing(t, out)
}

func TestListingShowsEveryObjectWithItsHash(t *testing.T) {
	_, root, pack := demoPack(t)

	rows := listingOf(t, root, pack)

	// Eleven leaves and the five subtrees beneath the root.
	if want := 16; len(rows) != want {
		t.Errorf("listed %d objects, want %d", len(rows), want)
	}

	var trees, blobs int
	for _, r := range rows {
		switch r.kind {
		case "tree":
			trees++
		case "blob":
			blobs++
		}
	}

	if trees != 5 {
		t.Errorf("listed %d trees, want 5", trees)
	}
	if blobs != 11 {
		t.Errorf("listed %d blobs, want 11", blobs)
	}
}

func TestListingMarksStructsAsTreesAndValuesAsBlobs(t *testing.T) {
	_, root, pack := demoPack(t)

	kinds := map[string]string{}
	for _, r := range listingOf(t, root, pack) {
		kinds[r.path] = r.kind
	}

	for path, want := range map[string]string{
		"Device":               "tree",
		"Device/Location":      "tree",
		"Device/Location/Room": "blob",
		"Network/Primary":      "tree",
		"Network/Primary/MTU":  "blob",
		"Network/DNS":          "blob", // a slice is one leaf
		"Device/Installed":     "blob", // a struct, tagged as a leaf
		"Audio":                "tree",
		"Audio/Delay":          "blob", // a proto.Message is one leaf
	} {
		if kinds[path] != want {
			t.Errorf("%s: listed as %q, want %q", path, kinds[path], want)
		}
	}
}

func TestListingUsesGitsModesForEachKind(t *testing.T) {
	_, root, pack := demoPack(t)

	for _, r := range listingOf(t, root, pack) {
		want := "100644"
		if r.kind == "tree" {
			want = "040000"
		}

		if r.mode != want {
			t.Errorf("%s (%s): mode %q, want %q", r.path, r.kind, r.mode, want)
		}
	}
}

func TestListingPlacesASubtreeBeforeItsContents(t *testing.T) {
	_, root, pack := demoPack(t)

	rows := listingOf(t, root, pack)

	at := func(path string) int {
		for i, r := range rows {
			if r.path == path {
				return i
			}
		}

		return -1
	}

	parent, child := at("Device/Location"), at("Device/Location/Room")
	if parent < 0 || child < 0 {
		t.Fatalf("expected paths missing: %d %d", parent, child)
	}
	if parent > child {
		t.Errorf("subtree listed at %d, after its contents at %d", parent, child)
	}
}

func TestListingReportsATreeItDoesNotHave(t *testing.T) {
	g := receive.NewGraph()

	if _, err := g.Listing(plumbing.NewHash("1111111111111111111111111111111111111111")); err == nil {
		t.Error("Listing accepted a tree the graph does not have")
	}
}
