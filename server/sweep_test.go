package server_test

import (
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
)

// tree stores one blob under a one-entry tree and returns the tree's hash, so
// a test has a head its store actually holds.
func tree(t *testing.T, store *objects.Store, name string) plumbing.Hash {
	t.Helper()

	blob, err := store.AddBlob([]byte(name))
	if err != nil {
		t.Fatalf("AddBlob: %v", err)
	}

	root, err := store.AddTree([]object.TreeEntry{
		{Name: name, Mode: filemode.Regular, Hash: blob},
	})
	if err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	return root
}

// published returns a server holding one ref at a head its store has.
func published(t *testing.T) (*server.Server, *objects.Store, server.Head) {
	t.Helper()

	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	store := objects.NewStore()
	head := server.Head{Hash: tree(t, store, "first"), Sequence: 1}

	if err := srv.Publish(someRef, store, head); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	return srv, store, head
}

func TestSweepInstallsTheNewStoreAndHeads(t *testing.T) {
	srv, store, head := published(t)

	next := objects.NewStore()
	moved := server.Head{Hash: tree(t, next, "second"), Sequence: 2}

	reset, err := srv.Sweep(next, map[string]server.Head{someRef: moved})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if reset != 0 {
		t.Errorf("%d subscribers were reset, want none — nobody was connected", reset)
	}

	if got, held := srv.Head(someRef); !held || got != moved {
		t.Errorf("Head() = %+v, %v; want %+v, true", got, held, moved)
	}

	// The old store is untouched; the server simply no longer reads it.
	if !store.Has(head.Hash) {
		t.Error("the sweep changed the store it replaced")
	}
}

// A sweep that names nothing still swaps the store, which is why every head
// has to be reachable in it whether it was named or not.
func TestSweepWithNoNamedHeadsStillChecksTheOnesItKeeps(t *testing.T) {
	srv, _, head := published(t)

	if _, err := srv.Sweep(objects.NewStore(), nil); err == nil {
		t.Fatal("Sweep accepted a store without the head the ref keeps")
	}

	// Nothing changed.
	if got, held := srv.Head(someRef); !held || got != head {
		t.Errorf("Head() = %+v, %v; want %+v, true", got, held, head)
	}
}

func TestSweepRefusesAHeadTheNewStoreDoesNotHold(t *testing.T) {
	srv, _, head := published(t)

	next := objects.NewStore()
	tree(t, next, "something else")

	_, err := srv.Sweep(next, map[string]server.Head{
		someRef: {Hash: otherHash, Sequence: 2},
	})
	if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
		t.Errorf("Sweep = %v (code %s), want %s", err, got, treevial.CodeInvalid)
	}

	if got, _ := srv.Head(someRef); got != head {
		t.Errorf("head moved to %+v, want it left at %+v", got, head)
	}
}

func TestSweepRefusesAHeadThatDoesNotAdvance(t *testing.T) {
	srv, _, head := published(t)

	next := objects.NewStore()
	behind := server.Head{Hash: tree(t, next, "behind"), Sequence: 1}

	if _, err := srv.Sweep(next, map[string]server.Head{someRef: behind}); !errors.Is(err, server.ErrNotAdvancing) {
		t.Errorf("Sweep = %v, want ErrNotAdvancing", err)
	}

	if got, _ := srv.Head(someRef); got != head {
		t.Errorf("head moved to %+v, want it left at %+v", got, head)
	}
}

func TestSweepRefusesARefTheServerDoesNotHold(t *testing.T) {
	srv, _, _ := published(t)

	next := objects.NewStore()
	kept := server.Head{Hash: tree(t, next, "first"), Sequence: 2}

	_, err := srv.Sweep(next, map[string]server.Head{
		someRef:                    kept,
		"refs/heads/nobody/config": {Hash: kept.Hash, Sequence: 2},
	})
	if !errors.Is(err, server.ErrUnknownRef) {
		t.Errorf("Sweep = %v, want ErrUnknownRef", err)
	}
}

func TestSweepRefusesAProviderBackedRef(t *testing.T) {
	srv, err := server.New(server.WithProvider(stubProvider{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	store := objects.NewStore()
	head := server.Head{Hash: tree(t, store, "published"), Sequence: 1}

	if err := srv.Publish(someRef, store, head); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// The provider's own ref is not published, so it is not the caller's to
	// sweep — and it reports that rather than "unknown", since the
	// difference is what the caller has to act on.
	_, err = srv.Sweep(store, map[string]server.Head{"refs/heads/lazy/config": head})
	if !errors.Is(err, server.ErrUnknownRef) {
		t.Errorf("Sweep of a ref nobody holds = %v, want ErrUnknownRef", err)
	}
}

func TestSweepRefusesANilStore(t *testing.T) {
	srv, _, _ := published(t)

	if _, err := srv.Sweep(nil, nil); err == nil {
		t.Error("Sweep accepted a nil store")
	}
}

// A void head needs nothing in the store, so a ref whose state is gone can be
// swept to nothing in the same call that keeps the others.
func TestSweepMayVoidARef(t *testing.T) {
	srv, _, _ := published(t)

	next := objects.NewStore()
	void := server.Head{Sequence: 2}

	if _, err := srv.Sweep(next, map[string]server.Head{someRef: void}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	got, held := srv.Head(someRef)
	if !held {
		t.Fatal("a voided ref is no longer held")
	}
	if got != void {
		t.Errorf("Head() = %+v, want %+v", got, void)
	}
}
