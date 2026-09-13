package structtree_test

import (
	"reflect"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

// builderOn returns a Builder over cfg with its first, full build done.
func builderOn(t *testing.T, store *objects.Store, cfg *config) (*structtree.Builder, plumbing.Hash) {
	t.Helper()

	b, err := structtree.NewBuilder(store, cfg)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	root, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return b, root
}

func TestABuildersFirstBuildMatchesAPlainBuild(t *testing.T) {
	cfg := sampleConfig()

	_, got := builderOn(t, objects.NewStore(), cfg)

	if want := rebuilt(t, cfg); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// The invariant the whole design rests on: taking the shortcut must produce
// exactly the tree the long way round would.
func TestATargetedRebuildMatchesAFullRebuild(t *testing.T) {
	cfg := sampleConfig()

	b, _ := builderOn(t, objects.NewStore(), cfg)

	cfg.Primary.MTU = 9000

	got, err := b.Build(&cfg.Primary.MTU)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if want := rebuilt(t, cfg); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestATargetedRebuildEncodesOnlyWhatWasDeclared(t *testing.T) {
	cfg := sampleConfig()

	encoded := 0

	m := structtree.Mapper{
		Encode: func(v reflect.Value) ([]byte, error) {
			encoded++

			return structtree.DefaultEncoder(v)
		},
	}

	b, err := m.NewBuilder(objects.NewStore(), cfg)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	encoded = 0
	cfg.Primary.MTU = 9000

	if _, err := b.Build(&cfg.Primary.MTU); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if encoded != 1 {
		t.Errorf("encoded %d leaves, want 1", encoded)
	}
}

func TestATargetedRebuildWritesOnlyThePathsObjects(t *testing.T) {
	cfg := sampleConfig()
	store := objects.NewStore()

	b, before := builderOn(t, store, cfg)

	cfg.Primary.MTU = 9000

	after, err := b.Build(&cfg.Primary.MTU)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	fresh, err := store.SelectSince(before, after)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	// The rewritten blob, the Primary tree and the root.
	if want := 3; len(fresh) != want {
		t.Errorf("got %d new objects, want %d", len(fresh), want)
	}
}

func TestAPointerToASubtreeRecomputesAllOfIt(t *testing.T) {
	cfg := sampleConfig()

	encoded := 0

	m := structtree.Mapper{
		Encode: func(v reflect.Value) ([]byte, error) {
			encoded++

			return structtree.DefaultEncoder(v)
		},
	}

	b, err := m.NewBuilder(objects.NewStore(), cfg)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	encoded = 0
	cfg.Device.Location.Room = "hall-b"

	got, err := b.Build(&cfg.Device)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Device/Name, Device/Location/Room and Device/Location/Row.
	if encoded != 3 {
		t.Errorf("encoded %d leaves, want the 3 under Device", encoded)
	}
	if want := rebuilt(t, cfg); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestThePointerToTheWholeValueIsTheWildcard(t *testing.T) {
	cfg := sampleConfig()

	encoded := 0

	m := structtree.Mapper{
		Encode: func(v reflect.Value) ([]byte, error) {
			encoded++

			return structtree.DefaultEncoder(v)
		},
	}

	b, err := m.NewBuilder(objects.NewStore(), cfg)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	all := len(paths(cfg))

	encoded = 0
	if _, err := b.Build(cfg); err != nil {
		t.Fatalf("Build(cfg): %v", err)
	}
	if encoded != all {
		t.Errorf("Build(cfg) encoded %d leaves, want all %d", encoded, all)
	}

	encoded = 0
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build(): %v", err)
	}
	if encoded != all {
		t.Errorf("Build() encoded %d leaves, want all %d", encoded, all)
	}
}

func TestAPointerFieldCanBeNamedEitherWay(t *testing.T) {
	cfg := sampleConfig()

	b, _ := builderOn(t, objects.NewStore(), cfg)

	cfg.Primary.MTU = 9000

	// The address of the pointer field, and the pointer itself.
	viaField, err := b.Build(&cfg.Primary)
	if err != nil {
		t.Fatalf("Build(&cfg.Primary): %v", err)
	}

	viaPointer, err := b.Build(cfg.Primary)
	if err != nil {
		t.Fatalf("Build(cfg.Primary): %v", err)
	}

	if viaField != viaPointer {
		t.Errorf("the two spellings disagree: %s and %s", viaField, viaPointer)
	}
	if want := rebuilt(t, cfg); viaField != want {
		t.Errorf("got %s, want %s", viaField, want)
	}
}

func TestAPointerThatMatchesNoFieldIsAnError(t *testing.T) {
	cfg := sampleConfig()

	b, _ := builderOn(t, objects.NewStore(), cfg)

	stray := 42

	// Silently ignoring this would publish a stale tree, which is the one
	// way this design can quietly go wrong.
	if _, err := b.Build(&stray); err == nil {
		t.Error("Build accepted a pointer into nothing")
	}
}

func TestAnUndeclaredChangeIsNotPickedUp(t *testing.T) {
	cfg := sampleConfig()

	b, _ := builderOn(t, objects.NewStore(), cfg)

	cfg.Primary.MTU = 9000
	cfg.Device.Name = "undeclared"

	got, err := b.Build(&cfg.Primary.MTU)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The price of the shortcut, stated plainly: the tree carries the
	// declared change and not the other.
	if want := rebuilt(t, cfg); got == want {
		t.Error("the undeclared change reached the tree; the shortcut is not doing what it says")
	}

	cfg.Device.Name = "panel"
	if want := rebuilt(t, cfg); got != want {
		t.Errorf("got %s, want the tree without the undeclared change %s", got, want)
	}
}

func TestAStructuralChangeIsPickedUpWithoutBeingDeclared(t *testing.T) {
	cfg := sampleConfig()

	b, _ := builderOn(t, objects.NewStore(), cfg)

	// Backup was nil, so its paths are new: there is no cached hash to
	// reuse for them, and they cannot be missed.
	cfg.Backup = &netIface{Address: "10.0.0.2", MTU: 9000}

	got, err := b.Build(&cfg.Primary.MTU)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if want := rebuilt(t, cfg); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestSuccessiveTargetedBuildsStayCorrect(t *testing.T) {
	cfg := sampleConfig()

	b, _ := builderOn(t, objects.NewStore(), cfg)

	for i, change := range []func(){
		func() { cfg.Primary.MTU = 9000 },
		func() { cfg.Device.Location.Row = 4 },
		func() { cfg.Tags = []string{"rear"} },
		func() { cfg.Primary.Address = "10.0.0.9" },
	} {
		change()

		var (
			got plumbing.Hash
			err error
		)

		switch i {
		case 0:
			got, err = b.Build(&cfg.Primary.MTU)
		case 1:
			got, err = b.Build(&cfg.Device.Location.Row)
		case 2:
			got, err = b.Build(&cfg.Tags)
		case 3:
			got, err = b.Build(&cfg.Primary.Address)
		}

		if err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		if want := rebuilt(t, cfg); got != want {
			t.Errorf("build %d: got %s, want %s", i, got, want)
		}
	}
}

func TestABuilderNeedsAPointerToAStruct(t *testing.T) {
	store := objects.NewStore()

	if _, err := structtree.NewBuilder(store, *sampleConfig()); err == nil {
		t.Error("NewBuilder accepted a non-pointer")
	}

	n := 3
	if _, err := structtree.NewBuilder(store, &n); err == nil {
		t.Error("NewBuilder accepted a pointer to a non-struct")
	}
}
