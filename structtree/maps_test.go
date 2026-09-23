package structtree_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

type withMaps struct {
	Limits map[string]int
	Ports  map[string]netIface
	Whole  map[string]int `treevial:"leaf"`
	Empty  map[string]int
	Absent map[string]int
}

func sampleMaps() *withMaps {
	return &withMaps{
		Limits: map[string]int{"gain": 6, "delay": 12},
		Ports: map[string]netIface{
			"eth0": {Address: "10.0.0.1", MTU: 1500},
			"eth1": {Address: "10.0.0.2", MTU: 9000},
		},
		Whole: map[string]int{"a": 1, "b": 2},
		Empty: map[string]int{},
	}
}

func TestAMapBecomesASubtree(t *testing.T) {
	got := paths(sampleMaps())

	want := []string{
		"Limits/delay", "Limits/gain",
		"Ports/eth0/Address", "Ports/eth0/MTU",
		"Ports/eth1/Address", "Ports/eth1/MTU",
		"Whole",
	}

	if !slices.Equal(got, want) {
		t.Errorf("got %v,\nwant %v", got, want)
	}
}

func TestATaggedMapStaysOneBlob(t *testing.T) {
	_, leaves := storedLeaves(t, sampleMaps())

	if _, ok := leaves["Whole"]; !ok {
		t.Error("the tagged map was not stored whole")
	}
	if _, ok := leaves["Whole/a"]; ok {
		t.Error("the tagged map was descended into")
	}
}

func TestMapKeysAreSortedSoTheWalkIsDeterministic(t *testing.T) {
	// Go's map order is random, so without sorting this would flap.
	first := paths(sampleMaps())

	for range 20 {
		if got := paths(sampleMaps()); !slices.Equal(got, first) {
			t.Fatalf("walk order changed:\n%v\n%v", got, first)
		}
	}
}

func TestANilMapContributesNothing(t *testing.T) {
	for _, p := range paths(sampleMaps()) {
		if len(p) >= 6 && p[:6] == "Absent" {
			t.Errorf("a nil map produced %q", p)
		}
	}
}

func TestAnEmptyMapContributesNothing(t *testing.T) {
	for _, p := range paths(sampleMaps()) {
		if len(p) >= 5 && p[:5] == "Empty" {
			t.Errorf("an empty map produced %q", p)
		}
	}
}

func TestAMapOfStructsNestsFurther(t *testing.T) {
	_, leaves := storedLeaves(t, sampleMaps())

	if got, want := string(leaves["Ports/eth1/MTU"]), "9000"; got != want {
		t.Errorf("Ports/eth1/MTU: got %s, want %s", got, want)
	}
}

func TestBuildRejectsAMapItCannotAddress(t *testing.T) {
	type badKeys struct {
		Zones map[int]string
	}

	if _, err := structtree.Build(objects.NewStore(), &badKeys{Zones: map[int]string{1: "a"}}); err == nil {
		t.Error("Build accepted a map whose keys cannot be path elements")
	}
}

func TestBuildRejectsAKeyThatIsNotAPathElement(t *testing.T) {
	for _, key := range []string{"", "with/slash"} {
		v := &withMaps{Limits: map[string]int{key: 1}}

		if _, err := structtree.Build(objects.NewStore(), v); err == nil {
			t.Errorf("Build accepted the key %q", key)
		}
	}
}

func TestApplyRebuildsAMap(t *testing.T) {
	want := sampleMaps()

	_, leaves := storedLeaves(t, want)

	var got withMaps
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Limits["gain"] != 6 || got.Limits["delay"] != 12 {
		t.Errorf("Limits: got %v", got.Limits)
	}
	if got.Ports["eth1"].MTU != 9000 {
		t.Errorf("Ports: got %v", got.Ports)
	}
	if got.Whole["b"] != 2 {
		t.Errorf("Whole: got %v", got.Whole)
	}

	// Rebuilding what was decoded must reproduce the same tree.
	if rebuilt(t, &got) != rebuilt(t, want) {
		t.Error("the round trip changed the value")
	}
}

func TestApplyDropsMapKeysTheTreeNoLongerCarries(t *testing.T) {
	_, leaves := storedLeaves(t, sampleMaps())
	delete(leaves, "Limits/delay")

	got := sampleMaps()

	if err := structtree.Apply(got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if _, ok := got.Limits["delay"]; ok {
		t.Error("a key the tree no longer carries survived")
	}
	if got.Limits["gain"] != 6 {
		t.Errorf("the remaining key was lost: %v", got.Limits)
	}
}

func TestApplyNilsAMapWhoseSubtreeIsGone(t *testing.T) {
	_, leaves := storedLeaves(t, sampleMaps())
	for p := range leaves {
		if len(p) >= 7 && p[:7] == "Limits/" {
			delete(leaves, p)
		}
	}

	got := sampleMaps()

	if err := structtree.Apply(got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Limits != nil {
		t.Errorf("Limits: got %v, want nil", got.Limits)
	}
}

func TestApplyRejectsAMapItCannotAddress(t *testing.T) {
	type badKeys struct {
		Zones map[int]string
	}

	var got badKeys
	if err := structtree.Apply(&got, map[string][]byte{"Zones/1": []byte(`"a"`)}); err == nil {
		t.Error("Apply accepted a map whose keys cannot be path elements")
	}
}

func TestACustomRuleDecidesForMapValuesToo(t *testing.T) {
	// A map value has no struct field of its own, so the rule is asked
	// about a synthesised one carrying the key as its name and the map's
	// element type.
	var asked []string

	m := structtree.Mapper{
		IsLeaf: func(f reflect.StructField) bool {
			asked = append(asked, f.Name+":"+f.Type.String())

			return structtree.DefaultIsLeaf(f)
		},
	}

	for range m.Walk(&withMaps{Ports: map[string]netIface{"eth0": {}}}) {
	}

	if !slices.Contains(asked, "eth0:structtree_test.netIface") {
		t.Errorf("the rule was not asked about the map value; it saw %v", asked)
	}
}

func TestABuilderDeclaresAWholeMap(t *testing.T) {
	v := sampleMaps()

	b, err := structtree.NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A map element has no address, so the map itself is what gets
	// declared.
	v.Limits["gain"] = 9

	got, err := b.Build(&v.Limits)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if want := rebuilt(t, v); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// deepMaps keeps its entries behind pointers, which is what lets a caller both
// mutate a field inside an entry and hand its address to a Builder.
type deepMaps struct {
	Ports map[string]*device
	Zones map[string]device
}

func sampleDeepMaps() *deepMaps {
	return &deepMaps{
		Ports: map[string]*device{
			"eth0": {Name: "front", Location: location{Room: "hall-a", Row: 1}},
			"eth1": {Name: "rear", Location: location{Room: "hall-b", Row: 2}},
		},
		Zones: map[string]device{
			"north": {Name: "north", Location: location{Room: "hall-c", Row: 3}},
		},
	}
}

func countingMapper(n *int) structtree.Mapper {
	return structtree.Mapper{
		Encode: func(v reflect.Value) ([]byte, error) {
			*n++

			return structtree.DefaultEncoder(v)
		},
	}
}

func TestABuilderDeclaresAFieldInsideAMapEntry(t *testing.T) {
	v := sampleDeepMaps()

	encoded := 0

	b, err := countingMapper(&encoded).NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	encoded = 0
	v.Ports["eth0"].Location.Row = 9

	got, err := b.Build(&v.Ports["eth0"].Location.Row)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if encoded != 1 {
		t.Errorf("encoded %d leaves, want 1", encoded)
	}
	if want := rebuilt(t, v); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestABuilderDeclaresAWholeMapEntry(t *testing.T) {
	v := sampleDeepMaps()

	encoded := 0

	b, err := countingMapper(&encoded).NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	encoded = 0
	v.Ports["eth0"].Name = "moved"
	v.Ports["eth0"].Location.Room = "hall-z"

	// The entry pointer stands for everything in that entry.
	got, err := b.Build(v.Ports["eth0"])
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Name, Location/Room and Location/Row — and nothing from eth1.
	if encoded != 3 {
		t.Errorf("encoded %d leaves, want the 3 in eth0", encoded)
	}
	if want := rebuilt(t, v); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestDeclaringOneEntryLeavesItsSiblingsAlone(t *testing.T) {
	v := sampleDeepMaps()
	store := objects.NewStore()

	b, err := structtree.NewBuilder(store, v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	before, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	v.Ports["eth0"].Location.Row = 9

	after, err := b.Build(&v.Ports["eth0"].Location.Row)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	fresh, err := store.SelectSince(before, after)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	// The rewritten blob and the trees above it: eth0/Location, eth0,
	// Ports and the root. Nothing of eth1 or Zones.
	if want := 5; len(fresh) != want {
		t.Errorf("got %d new objects, want %d", len(fresh), want)
	}
}

func TestAValueTypedMapCanOnlyBeDeclaredWhole(t *testing.T) {
	v := sampleDeepMaps()

	encoded := 0

	b, err := countingMapper(&encoded).NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Its entries have no address — Go will not even let one be mutated in
	// place — so the map itself is the finest thing there is to declare.
	encoded = 0
	v.Zones["north"] = device{Name: "north", Location: location{Room: "hall-z", Row: 4}}

	got, err := b.Build(&v.Zones)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if encoded != 3 {
		t.Errorf("encoded %d leaves, want the 3 in Zones", encoded)
	}
	if want := rebuilt(t, v); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// A map of pointers is the shape the Builder made first-class, and Apply never
// had a test for it.
func TestApplyFillsAMapOfPointers(t *testing.T) {
	want := sampleDeepMaps()

	_, leaves := storedLeaves(t, want)

	var got deepMaps
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Ports["eth0"] == nil {
		t.Fatal("Ports/eth0 was left nil")
	}
	if got.Ports["eth0"].Location.Room != "hall-a" {
		t.Errorf("Ports/eth0: got %+v", got.Ports["eth0"])
	}
	if got.Zones["north"].Name != "north" {
		t.Errorf("Zones/north: got %+v", got.Zones["north"])
	}

	if rebuilt(t, &got) != rebuilt(t, want) {
		t.Error("the round trip changed the value")
	}
}

func TestApplySinceFillsAMapOfPointers(t *testing.T) {
	first := sampleDeepMaps()

	second := sampleDeepMaps()
	second.Ports["eth0"].Location.Row = 9

	h := newHistory(t, first, second)

	var got deepMaps
	if err := structtree.ApplySince(&got, h.graph, plumbing.ZeroHash, h.roots[0]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}
	if err := structtree.ApplySince(&got, h.graph, h.roots[0], h.roots[1]); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	if got.Ports["eth0"] == nil {
		t.Fatal("Ports/eth0 was left nil")
	}
	if got.Ports["eth0"].Location.Row != 9 {
		t.Errorf("Ports/eth0/Location/Row: got %d, want 9", got.Ports["eth0"].Location.Row)
	}
	if rebuilt(t, &got) != rebuilt(t, second) {
		t.Error("the incremental decode does not match the value published")
	}
}

func TestApplyReportsANonStructRatherThanPanicking(t *testing.T) {
	// apply reaches NumField, so a caller that hands it anything else used
	// to take the process down. Whatever goes wrong here, it should arrive
	// as an error.
	for _, dst := range []any{
		&map[string]int{},
		&[]string{},
		new(int),
	} {
		err := structtree.Apply(dst, map[string][]byte{"A": []byte("1")})
		if err == nil {
			t.Errorf("Apply(%T) returned no error", dst)
		}
	}
}
