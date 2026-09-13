package main

import (
	"testing"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

const testRef = "refs/heads/printer-7/config"

// TestRetuneMatchesAFullRebuild is the invariant the incremental path rests on,
// applied where this program actually uses it: taking the shortcut has to
// produce the tree the long way round would.
func TestRetuneMatchesAFullRebuild(t *testing.T) {
	p := newDemoProvider()

	if _, _, err := p.Prepare(testRef); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	got, err := p.Retune(testRef)
	if err != nil {
		t.Fatalf("Retune: %v", err)
	}

	// The same configuration, built from scratch in a store of its own.
	want, err := structtree.Build(objects.NewStore(), p.held[testRef].config)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got != want {
		t.Errorf("retuned tree %s, want %s", got, want)
	}
}

func TestRetuneWritesOnlyThePathsObjects(t *testing.T) {
	p := newDemoProvider()

	store, before, err := p.Prepare(testRef)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	after, err := p.Retune(testRef)
	if err != nil {
		t.Fatalf("Retune: %v", err)
	}

	fresh, err := store.SelectSince(before, after)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	// The rewritten blob and the Primary, Network and root trees above it.
	if want := 4; len(fresh) != want {
		t.Errorf("got %d new objects, want %d", len(fresh), want)
	}
}

func TestRetuneOnAReleasedRefFails(t *testing.T) {
	p := newDemoProvider()

	if _, _, err := p.Prepare(testRef); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	p.Release(testRef)

	if _, err := p.Retune(testRef); err == nil {
		t.Error("Retune worked on a ref whose data was released")
	}
}
