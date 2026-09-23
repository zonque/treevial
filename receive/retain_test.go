package receive_test

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/receive"
)

// moved returns a graph holding two successive states of the demo tree, the
// way a client's graph holds them after two pushes: the whole tree, then the
// four objects one changed field moved.
func moved(t *testing.T) (g *receive.Graph, before, after plumbing.Hash) {
	t.Helper()

	store := objects.NewStore()

	before, err := shared.BuildTree(store, "v1")
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	after, err = store.ReplaceBlob(before, "Network/Primary/MTU", []byte("9000"))
	if err != nil {
		t.Fatalf("ReplaceBlob: %v", err)
	}

	g = receive.NewGraph()

	push(t, store, g, plumbing.ZeroHash, before)
	push(t, store, g, before, after)

	return g, before, after
}

func TestRetainDropsWhatNoRootReaches(t *testing.T) {
	g, before, after := moved(t)

	// Two states, sharing all but the changed blob and the trees above it:
	// 17 objects for the first and 4 more for the second.
	dropped, err := g.Retain(after)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}

	if want := 4; dropped != want {
		t.Errorf("dropped %d objects, want the %d only the older state needed", dropped, want)
	}

	// What the older state alone held is gone; the current state is whole.
	if _, err := g.Leaves(before); err == nil {
		t.Error("the dropped state can still be walked")
	}
	if _, err := g.Leaves(after); err != nil {
		t.Errorf("the retained state cannot be walked: %v", err)
	}
}

func TestRetainKeepsEveryRootItIsGiven(t *testing.T) {
	g, before, after := moved(t)

	dropped, err := g.Retain(before, after)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}

	if dropped != 0 {
		t.Errorf("dropped %d objects, want none: both states were named", dropped)
	}

	for _, root := range []plumbing.Hash{before, after} {
		if _, err := g.Leaves(root); err != nil {
			t.Errorf("state %s cannot be walked: %v", root, err)
		}
	}
}

func TestRetainIgnoresTheZeroHash(t *testing.T) {
	g, _, after := moved(t)

	// What a client does on its first update, where Previous is the zero
	// hash: it must mean "nothing to keep", not "a root I cannot find".
	if _, err := g.Retain(plumbing.ZeroHash, after); err != nil {
		t.Fatalf("Retain: %v", err)
	}

	if _, err := g.Leaves(after); err != nil {
		t.Errorf("the retained state cannot be walked: %v", err)
	}
}

func TestRetainRefusesARootItCannotWalkAndDropsNothing(t *testing.T) {
	g, before, after := moved(t)

	stranger := plumbing.NewHash("df0e0e1cd7146ab580338deb67e66bd05d42c1e8")

	dropped, err := g.Retain(after, stranger)
	if err == nil {
		t.Fatal("Retain accepted a root the graph does not hold")
	}
	if dropped != 0 {
		t.Errorf("dropped %d objects while refusing, want none", dropped)
	}

	// Nothing was swept, so both states are still there to try again with.
	for _, root := range []plumbing.Hash{before, after} {
		if _, err := g.Leaves(root); err != nil {
			t.Errorf("state %s was damaged by a refused sweep: %v", root, err)
		}
	}
}

func TestRetainingNothingEmptiesTheGraph(t *testing.T) {
	g, _, after := moved(t)

	dropped, err := g.Retain()
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}

	if want := 21; dropped != want {
		t.Errorf("dropped %d objects, want all %d of them", dropped, want)
	}

	if _, err := g.Leaves(after); err == nil {
		t.Error("the graph still holds a tree after being told to keep nothing")
	}
}

func TestRetainingTwiceDropsNothingTheSecondTime(t *testing.T) {
	g, _, after := moved(t)

	if _, err := g.Retain(after); err != nil {
		t.Fatalf("Retain: %v", err)
	}

	dropped, err := g.Retain(after)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}

	if dropped != 0 {
		t.Errorf("a second sweep dropped %d objects, want none", dropped)
	}
}

func TestASweptGraphStillFollowsThePushesThatComeAfterIt(t *testing.T) {
	store := objects.NewStore()

	first, err := shared.BuildTree(store, "v1")
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	g := receive.NewGraph()
	push(t, store, g, plumbing.ZeroHash, first)

	// A client that sweeps down to the head it holds is still a client
	// that holds that head: the next push carries only what moved, and
	// resolves against what the sweep kept.
	if _, err := g.Retain(first); err != nil {
		t.Fatalf("Retain: %v", err)
	}

	second, err := store.ReplaceBlob(first, "Network/Primary/MTU", []byte("9000"))
	if err != nil {
		t.Fatalf("ReplaceBlob: %v", err)
	}

	push(t, store, g, first, second)

	leaves, err := g.Leaves(second)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}
	if want := shared.LeafCount; len(leaves) != want {
		t.Fatalf("reconstructed %d leaves, want %d", len(leaves), want)
	}
	if got := string(leaves["Network/Primary/MTU"]); got != "9000" {
		t.Errorf("Network/Primary/MTU = %q, want %q", got, "9000")
	}

	// And the pair the update reports is still usable for a changeset.
	changeset, err := g.ListingSince(first, second)
	if err != nil {
		t.Fatalf("ListingSince: %v", err)
	}
	if changeset == "" {
		t.Error("the changeset is empty after a sweep that kept both states")
	}
}

func TestLenCountsEverythingTheGraphHolds(t *testing.T) {
	g, _, after := moved(t)

	// The whole tree, and the four objects the second state moved.
	if want := 21; g.Len() != want {
		t.Errorf("graph holds %d objects, want %d", g.Len(), want)
	}

	if _, err := g.Retain(after); err != nil {
		t.Fatalf("Retain: %v", err)
	}

	if want := 17; g.Len() != want {
		t.Errorf("graph holds %d objects after the sweep, want the %d that are live", g.Len(), want)
	}
}
