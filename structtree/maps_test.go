package structtree_test

import (
	"reflect"
	"slices"
	"testing"

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
