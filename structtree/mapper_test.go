package structtree_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

// stamped has a field the default rule descends into and finds nothing in:
// time.Time is a struct that is not a protobuf message, and all its fields are
// unexported.
type stamped struct {
	Name string
	Seen time.Time
}

// tagged marks its own leaves.
type tagged struct {
	Name  string
	Inner inner `treevial:"leaf"`
	Plain inner
}

type inner struct {
	A int
	B int
}

func mapperPaths(m structtree.Mapper, v any) []string {
	var out []string
	for leaf := range m.Walk(v) {
		out = append(out, leaf.Path)
	}

	return out
}

func TestTheZeroMapperBehavesLikeThePackageFunctions(t *testing.T) {
	var m structtree.Mapper

	if got, want := mapperPaths(m, sampleConfig()), paths(sampleConfig()); !slices.Equal(got, want) {
		t.Errorf("walk: got %v, want %v", got, want)
	}

	fromMapper, err := m.Build(objects.NewStore(), sampleConfig())
	if err != nil {
		t.Fatalf("Mapper.Build: %v", err)
	}

	if want := rebuilt(t, sampleConfig()); fromMapper != want {
		t.Errorf("build: got %s, want %s", fromMapper, want)
	}
}

func TestTheDefaultRuleDescendsIntoAStructWithNothingToSync(t *testing.T) {
	// Which is the wart a caller may want to fix: time.Time vanishes.
	got := paths(&stamped{Name: "panel", Seen: time.Unix(1700000000, 0)})

	if want := []string{"Name"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v — time.Time should have vanished", got, want)
	}
}

func TestALeafRuleCanKeepAStructWhole(t *testing.T) {
	m := structtree.Mapper{
		IsLeaf: func(f reflect.StructField) bool {
			return f.Type == reflect.TypeFor[time.Time]() || structtree.DefaultIsLeaf(f)
		},
	}

	got := mapperPaths(m, &stamped{Name: "panel", Seen: time.Unix(1700000000, 0)})

	if want := []string{"Name", "Seen"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestACustomLeafRuleAppliesToDecodingToo(t *testing.T) {
	m := structtree.Mapper{
		IsLeaf: func(f reflect.StructField) bool {
			return f.Type == reflect.TypeFor[time.Time]() || structtree.DefaultIsLeaf(f)
		},
	}

	want := &stamped{Name: "panel", Seen: time.Unix(1700000000, 0).UTC()}

	store := objects.NewStore()

	root, err := m.Build(store, want)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	leaves := leavesOf(t, store, root)

	var got stamped
	if err := m.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Name != want.Name {
		t.Errorf("Name: got %q, want %q", got.Name, want.Name)
	}
	if !got.Seen.Equal(want.Seen) {
		t.Errorf("Seen: got %s, want %s", got.Seen, want.Seen)
	}
}

func TestATaggedFieldIsALeafWithNoRuleOfYourOwn(t *testing.T) {
	// The tag is the primary way to say what a leaf is, so it works out of
	// the box.
	got := paths(&tagged{})

	// Inner is kept whole because it is tagged; Plain is descended into.
	want := []string{"Name", "Inner", "Plain/A", "Plain/B"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestATaggedFieldStaysALeafUnderARuleThatSaysOtherwise(t *testing.T) {
	// Whatever rule is plugged in answers for the fields the tag says
	// nothing about; it does not get to overrule the tag.
	m := structtree.Mapper{
		IsLeaf: func(reflect.StructField) bool { return false },
	}

	got := mapperPaths(m, &tagged{})

	if want := []string{"Name", "Inner", "Plain/A", "Plain/B"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestATaggedLeafRoundTrips(t *testing.T) {
	want := &tagged{Name: "panel", Inner: inner{A: 1, B: 2}, Plain: inner{A: 3, B: 4}}

	store := objects.NewStore()

	root, err := structtree.Build(store, want)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	leaves := leavesOf(t, store, root)

	// One blob for the whole tagged struct, and one per field of the
	// untagged one.
	if _, ok := leaves["Inner"]; !ok {
		t.Error("the tagged struct was not stored as one blob")
	}

	var got tagged
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got != *want {
		t.Errorf("got %+v, want %+v", got, *want)
	}
}

func TestAnUnknownTagValueIsIgnored(t *testing.T) {
	type mistyped struct {
		Inner inner `treevial:"blob"`
	}

	// Only "leaf" is recognised; anything else leaves the structural rule
	// to decide, which for a plain struct means descending into it.
	got := paths(&mistyped{})

	if want := []string{"Inner/A", "Inner/B"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAMapperCarriesItsOwnEncodingAndDecoding(t *testing.T) {
	// A rule and the codec that serves it travel together, so they cannot
	// be paired up wrongly at a call site.
	var encoded, decoded int

	m := structtree.Mapper{
		Encode: func(v reflect.Value) ([]byte, error) {
			encoded++

			return structtree.DefaultEncoder(v)
		},
		Decode: func(data []byte, v reflect.Value) error {
			decoded++

			return structtree.DefaultDecoder(data, v)
		},
	}

	store := objects.NewStore()

	root, err := m.Build(store, sampleConfig())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var got config
	if err := m.Apply(&got, leavesOf(t, store, root)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if want := len(paths(sampleConfig())); encoded != want {
		t.Errorf("encoded %d leaves, want %d", encoded, want)
	}
	if want := len(paths(sampleConfig())); decoded != want {
		t.Errorf("decoded %d leaves, want %d", decoded, want)
	}
}

func TestAMapperAppliesIncrementallyToo(t *testing.T) {
	m := structtree.Mapper{
		IsLeaf: func(f reflect.StructField) bool {
			return f.Type == reflect.TypeFor[time.Time]() || structtree.DefaultIsLeaf(f)
		},
	}

	before := &stamped{Name: "panel", Seen: time.Unix(1700000000, 0).UTC()}
	after := &stamped{Name: "panel", Seen: time.Unix(1800000000, 0).UTC()}

	h := newHistoryWith(t, m, before, after)

	got := &stamped{}
	if err := m.ApplySince(got, h.graph, plumbing.ZeroHash, h.roots[0]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}
	if err := m.ApplySince(got, h.graph, h.roots[0], h.roots[1]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	if !got.Seen.Equal(after.Seen) {
		t.Errorf("Seen: got %s, want %s", got.Seen, after.Seen)
	}
}
