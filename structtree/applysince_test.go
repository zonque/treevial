package structtree_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/holoplot/treevial/objects"
	"github.com/holoplot/treevial/receive"
	"github.com/holoplot/treevial/structtree"
)

// history builds a sequence of values into one store and feeds every object to
// a single graph, the way a client accumulates what it has been pushed.
type history struct {
	store *objects.Store
	graph *receive.Graph
	roots []plumbing.Hash
}

func newHistory(t *testing.T, values ...any) *history {
	t.Helper()

	h := &history{store: objects.NewStore(), graph: receive.NewGraph()}

	var sent plumbing.Hash

	for _, v := range values {
		root, err := structtree.Build(h.store, v)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		// Only the objects the server would have had to send.
		hashes, err := h.store.SelectSince(sent, root)
		if err != nil {
			t.Fatalf("SelectSince: %v", err)
		}

		if len(hashes) > 0 {
			var buf bytes.Buffer
			if _, err := h.store.EncodePack(&buf, hashes); err != nil {
				t.Fatalf("EncodePack: %v", err)
			}
			if err := receive.Interpret(&buf, h.graph); err != nil {
				t.Fatalf("Interpret: %v", err)
			}
		}

		h.roots = append(h.roots, root)
		sent = root
	}

	return h
}

// countingDecoder records how many leaves were actually decoded, which is how
// a test can see what ApplySince skipped.
func countingDecoder(n *int) structtree.Decoder {
	return func(data []byte, v reflect.Value) error {
		*n++

		return structtree.DefaultDecoder(data, v)
	}
}

func changedConfig() *config {
	c := sampleConfig()
	c.Device.Location.Row = 4

	return c
}

func TestApplySinceFromAZeroBaselineFillsEverything(t *testing.T) {
	h := newHistory(t, sampleConfig())

	var got config
	if err := structtree.ApplySince(&got, h.graph, plumbing.ZeroHash, h.roots[0]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	if rebuilt(t, &got) != h.roots[0] {
		t.Error("a zero baseline did not produce the whole value")
	}
}

func TestApplySinceDecodesOnlyTheLeavesThatChanged(t *testing.T) {
	h := newHistory(t, sampleConfig(), changedConfig())

	var got config
	if err := structtree.ApplySince(&got, h.graph, plumbing.ZeroHash, h.roots[0]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	decoded := 0
	if err := structtree.ApplySinceWith(&got, h.graph, h.roots[0], h.roots[1], countingDecoder(&decoded)); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	// One field moved, so exactly one leaf is worth decoding; the other
	// nine sit under subtrees whose hashes did not change.
	if decoded != 1 {
		t.Errorf("decoded %d leaves, want 1", decoded)
	}
	if got.Device.Location.Row != 4 {
		t.Errorf("Row: got %d, want 4", got.Device.Location.Row)
	}
}

func TestApplySinceDecodesNothingWhenTheTreeDidNotMove(t *testing.T) {
	h := newHistory(t, sampleConfig())

	var got config
	if err := structtree.ApplySince(&got, h.graph, plumbing.ZeroHash, h.roots[0]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	decoded := 0
	if err := structtree.ApplySinceWith(&got, h.graph, h.roots[0], h.roots[0], countingDecoder(&decoded)); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	if decoded != 0 {
		t.Errorf("decoded %d leaves for an unchanged tree, want 0", decoded)
	}
}

func TestApplySinceAgreesWithApply(t *testing.T) {
	h := newHistory(t, sampleConfig(), changedConfig())

	incremental := &config{}
	if err := structtree.ApplySince(incremental, h.graph, plumbing.ZeroHash, h.roots[0]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}
	if err := structtree.ApplySince(incremental, h.graph, h.roots[0], h.roots[1]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	full := &config{}

	leaves, err := h.graph.Leaves(h.roots[1])
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}
	if err := structtree.Apply(full, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if rebuilt(t, incremental) != rebuilt(t, full) {
		t.Errorf("incremental and full application disagree:\n got %+v\nwant %+v", incremental, full)
	}
}

func TestApplySinceZeroesLeavesRemovedSinceTheBaseline(t *testing.T) {
	before := sampleConfig()

	after := sampleConfig()
	after.Tags = nil
	after.Primary = nil

	h := newHistory(t, before, after)

	got := &config{}
	if err := structtree.ApplySince(got, h.graph, plumbing.ZeroHash, h.roots[0]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	if got.Primary == nil || got.Tags == nil {
		t.Fatal("the baseline was not applied")
	}

	if err := structtree.ApplySince(got, h.graph, h.roots[0], h.roots[1]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	if got.Tags != nil {
		t.Errorf("Tags: got %v, want nil", got.Tags)
	}
	if got.Primary != nil {
		t.Errorf("Primary: got %+v, want nil", got.Primary)
	}
	if got.Device.Name != "panel" {
		t.Errorf("an untouched field changed: %q", got.Device.Name)
	}
}

func TestApplySinceTreatsAnUnknownBaselineAsEverythingChanged(t *testing.T) {
	h := newHistory(t, sampleConfig())

	unknown := plumbing.NewHash("1111111111111111111111111111111111111111")

	var got config

	decoded := 0
	if err := structtree.ApplySinceWith(&got, h.graph, unknown, h.roots[0], countingDecoder(&decoded)); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	// A baseline the client cannot resolve must degrade to a full
	// application rather than silently skipping work.
	if decoded != len(paths(sampleConfig())) {
		t.Errorf("decoded %d leaves, want all %d", decoded, len(paths(sampleConfig())))
	}
	if rebuilt(t, &got) != h.roots[0] {
		t.Error("an unknown baseline did not produce the whole value")
	}
}

func TestApplySinceFailsWhenTheTargetTreeIsMissing(t *testing.T) {
	h := newHistory(t, sampleConfig())

	missing := plumbing.NewHash("2222222222222222222222222222222222222222")

	var got config
	if err := structtree.ApplySince(&got, h.graph, h.roots[0], missing); err == nil {
		t.Error("ApplySince accepted a tree the source does not have")
	}
}

func TestApplySinceRejectsDestinationsItCannotWriteTo(t *testing.T) {
	h := newHistory(t, sampleConfig())

	if err := structtree.ApplySince(config{}, h.graph, plumbing.ZeroHash, h.roots[0]); err == nil {
		t.Error("ApplySince accepted a non-pointer destination")
	}

	n := 3
	if err := structtree.ApplySince(&n, h.graph, plumbing.ZeroHash, h.roots[0]); err == nil {
		t.Error("ApplySince accepted a pointer to a non-struct")
	}
}
