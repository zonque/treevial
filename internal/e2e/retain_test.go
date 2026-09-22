package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/structtree"
)

// moveRef changes one field and pushes, returning the new head. Every state is
// built while the ref is quiet, which is the rule a provider keeps.
func moveRef(t *testing.T, h *harness, ref string, mtu int) plumbing.Hash {
	t.Helper()

	next := h.provider.retune(t, ref, mtu)
	if err := h.server.SetHead(ref, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	return next
}

func TestAGraphKeepsEveryStateItWasEverPushed(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, updates)
	waitForAllSynced(t, h.server, refA, u.Hash, 1)

	// Left to itself a graph accumulates: the whole tree, and four objects
	// for each move after it.
	for i, mtu := range []int{4000, 6000, 9000} {
		next := moveRef(t, h, refA, mtu)

		u = followTo(t, updates, next)
		waitForAllSynced(t, h.server, refA, next, 1)

		if want := 17 + 4*(i+1); u.Graph.Len() != want {
			t.Errorf("after %d moves the graph holds %d objects, want %d", i+1, u.Graph.Len(), want)
		}
	}
}

func TestAClientSweepsWhatItNoLongerNeeds(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// One state behind the arriving one is all ApplySince needs.
	cli, err := client.Dial(ctx, h.addr, client.WithHistory(1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { cli.Close() })

	updates, err := cli.Subscribe(ctx, refA)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	u := nextUpdate(t, updates)
	waitForAllSynced(t, h.server, refA, u.Hash, 1)

	first := u.Hash

	var config shared.Config
	if err := structtree.ApplySince(&config, u.Graph, u.Previous, u.Hash); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	for _, mtu := range []int{4000, 6000, 9000} {
		next := moveRef(t, h, refA, mtu)

		u = followTo(t, updates, next)
		waitForAllSynced(t, h.server, refA, next, 1)

		// The state it moved from is still there, so an update is
		// still decoded by what changed rather than in full.
		if err := structtree.ApplySince(&config, u.Graph, u.Previous, u.Hash); err != nil {
			t.Fatalf("ApplySince: %v", err)
		}

		if got := config.Network.Primary.MTU; got != mtu {
			t.Errorf("MTU = %d, want %d", got, mtu)
		}

		// The current state and the one behind it, and nothing else,
		// however many moves have gone by.
		if want := 21; u.Graph.Len() != want {
			t.Errorf("graph holds %d objects, want %d", u.Graph.Len(), want)
		}
	}

	// The state it started from is long gone.
	if _, err := u.Graph.Leaves(first); err == nil {
		t.Error("the graph can still walk a state three moves old")
	}
}

func TestAClientKeepsAsMuchHistoryAsItAsksFor(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, err := client.Dial(ctx, h.addr, client.WithHistory(2))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { cli.Close() })

	updates, err := cli.Subscribe(ctx, refA)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	u := nextUpdate(t, updates)
	waitForAllSynced(t, h.server, refA, u.Hash, 1)

	var seen []plumbing.Hash
	seen = append(seen, u.Hash)

	for _, mtu := range []int{4000, 6000, 9000} {
		next := moveRef(t, h, refA, mtu)

		u = followTo(t, updates, next)
		waitForAllSynced(t, h.server, refA, next, 1)

		seen = append(seen, next)
	}

	// Two states behind the current one, so the one before those is gone.
	for _, root := range seen[len(seen)-3:] {
		if _, err := u.Graph.Leaves(root); err != nil {
			t.Errorf("state %s should have been kept: %v", root, err)
		}
	}

	for _, root := range seen[:len(seen)-3] {
		if _, err := u.Graph.Leaves(root); err == nil {
			t.Errorf("state %s should have been swept", root)
		}
	}
}

func TestAskingForNoHistoryAtAllIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Nothing is listening: the argument is refused before a connection is
	// attempted.
	_, err := client.Dial(ctx, "127.0.0.1:1", client.WithHistory(0))
	if err == nil {
		t.Fatal("a history of zero states was accepted")
	}

	if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
		t.Errorf("got error %v (code %s), want %s", err, got, treevial.CodeInvalid)
	}
}
