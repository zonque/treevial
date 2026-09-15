package structtree_test

import (
	"reflect"
	"testing"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

// visiting counts how many fields the leaf rule is asked about, which is how a
// test can see how much of the value a build actually traversed.
func visiting(n *int) structtree.Mapper {
	return structtree.Mapper{
		IsLeaf: func(f reflect.StructField) bool {
			*n++

			return structtree.DefaultIsLeaf(f)
		},
	}
}

func TestATargetedBuildTraversesOnlyWhatWasDeclared(t *testing.T) {
	v := sampleDeepMaps()

	visited := 0

	b, err := visiting(&visited).NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	whole := visited
	if whole < 8 {
		t.Fatalf("a full build visited only %d fields; the test value is too small to tell anything", whole)
	}

	visited = 0
	v.Ports["eth0"].Location.Row = 9

	if _, err := b.Build(&v.Ports["eth0"].Location.Row); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The declared leaf and nothing else: not the sibling entry, not the
	// other map, not the rest of the entry it sits in.
	if visited >= whole {
		t.Errorf("a targeted build visited %d fields, a full one %d; it is still walking everything", visited, whole)
	}
	if visited > 3 {
		t.Errorf("a targeted build visited %d fields, want a handful", visited)
	}
}

func TestATargetedBuildStillMatchesAFullRebuild(t *testing.T) {
	v := sampleDeepMaps()

	b, err := structtree.NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	for i, change := range []struct {
		apply   func()
		declare func() any
	}{
		{func() { v.Ports["eth0"].Location.Row = 9 }, func() any { return &v.Ports["eth0"].Location.Row }},
		{func() { v.Ports["eth1"].Name = "moved" }, func() any { return &v.Ports["eth1"].Name }},
		{func() { v.Ports["eth0"].Location.Room = "hall-z" }, func() any { return v.Ports["eth0"] }},
		{func() { v.Zones["north"] = device{Name: "n", Location: location{Room: "r", Row: 1}} }, func() any { return &v.Zones }},
	} {
		change.apply()

		got, err := b.Build(change.declare())
		if err != nil {
			t.Fatalf("build %d: %v", i, err)
		}

		if want := rebuilt(t, v); got != want {
			t.Errorf("build %d: got %s, want %s", i, got, want)
		}
	}
}

func TestAKeyAddedWithoutBeingDeclaredIsNotPickedUp(t *testing.T) {
	v := sampleDeepMaps()

	b, err := structtree.NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	before, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Adding an entry changes the shape, and a declaration that names
	// something else cannot know about it. This is the bargain: a member
	// coming or going wants the map declared, or a full build.
	v.Ports["eth2"] = &device{Name: "spare"}
	v.Ports["eth1"].Name = "moved"

	got, err := b.Build(&v.Ports["eth1"].Name)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got == rebuilt(t, v) {
		t.Error("the added key reached the tree; the shortcut is not doing what it says")
	}
	if got == before {
		t.Error("the declared change did not reach the tree either")
	}
}

func TestDeclaringTheMapPicksUpAnAddedKey(t *testing.T) {
	v := sampleDeepMaps()

	b, err := structtree.NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	v.Ports["eth2"] = &device{Name: "spare"}

	got, err := b.Build(&v.Ports)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if want := rebuilt(t, v); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestAFullBuildPicksUpAnythingAtAll(t *testing.T) {
	v := sampleDeepMaps()

	b, err := structtree.NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	v.Ports["eth2"] = &device{Name: "spare"}
	delete(v.Ports, "eth1")
	v.Zones["south"] = device{Name: "s"}

	got, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if want := rebuilt(t, v); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestAReplacedEntryPointerIsRefusedRatherThanMisattributed(t *testing.T) {
	v := sampleDeepMaps()

	b, err := structtree.NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A fresh entry the last build never saw. Attributing it to whatever
	// used to live at that address would be worse than refusing it.
	v.Ports["eth0"] = &device{Name: "replaced"}

	if _, err := b.Build(v.Ports["eth0"]); err == nil {
		t.Error("Build accepted a pointer no build has ever seen")
	}
}
