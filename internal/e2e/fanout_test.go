package e2e

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/server"
)

// waitForAllSynced waits until want subscribers are following ref and every
// one of them has confirmed h. With several subscribers on one ref, "the ref
// has been taken up" is a statement about all of them, not about whichever
// answered first.
func waitForAllSynced(t *testing.T, srv *server.Server, ref string, h plumbing.Hash, want int) {
	t.Helper()

	eventually(t, "every subscriber of the ref to sync", func() bool {
		synced := 0

		for _, sub := range srv.Subscribers() {
			if sub.Ref == ref && sub.Synced == h {
				synced++
			}
		}

		return synced == want
	})
}

// followTo reads updates until the subscription reaches head, and returns the
// update that got there. A subscriber may be handed intermediate states on the
// way; what matters is where it ends up.
func followTo(t *testing.T, updates <-chan client.Update, head plumbing.Hash) client.Update {
	t.Helper()

	deadline := time.After(10 * time.Second)

	for {
		select {
		case u, ok := <-updates:
			if !ok {
				t.Fatal("update channel closed before the subscription reached the head")
			}

			if u.Hash == head {
				return u
			}
		case <-deadline:
			t.Fatalf("timed out before the subscription reached %s", head)
		}
	}
}

func TestSeveralSubscribersShareOnePreparedRef(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, first := h.subscribe(t, ctx, refA)
	_, second := h.subscribe(t, ctx, refA)

	a := nextUpdate(t, first)
	b := nextUpdate(t, second)

	if a.Hash != b.Hash {
		t.Errorf("subscribers were served different trees: %s and %s", a.Hash, b.Hash)
	}

	// Each is a client of its own and holds nothing, so each is sent the
	// whole graph — but it is prepared once.
	for _, u := range []client.Update{a, b} {
		leaves, err := u.Graph.Leaves(u.Hash)
		if err != nil {
			t.Fatalf("Leaves: %v", err)
		}
		if want := shared.LeafCount; len(leaves) != want {
			t.Errorf("subscriber reconstructed %d leaves, want %d", len(leaves), want)
		}
	}

	if prepared, _ := h.provider.counts(refA); prepared != 1 {
		t.Errorf("provider prepared %q %d times, want once for any number of subscribers", refA, prepared)
	}
}

func TestEverySubscriberMovesWhenTheRefMoves(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const followers = 3

	streams := make([]<-chan client.Update, followers)
	for i := range streams {
		_, streams[i] = h.subscribe(t, ctx, refA)
	}

	var start plumbing.Hash
	for _, updates := range streams {
		start = nextUpdate(t, updates).Hash
	}

	// Rebuilding while a push is in flight would have the provider writing
	// to a store the server is reading, so the ref moves only once every
	// subscriber has taken up the current one.
	waitForAllSynced(t, h.server, refA, start, followers)

	next := h.provider.retune(t, refA, 9000)
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	for i, updates := range streams {
		u := followTo(t, updates, next)

		if u.Previous != start {
			t.Errorf("subscriber %d moved from %s, want %s", i, u.Previous, start)
		}

		// The move costs each of them the changed blob and the trees
		// above it, not the whole graph again.
		if want := 4; u.ObjectCount != want {
			t.Errorf("subscriber %d was sent %d objects, want %d", i, u.ObjectCount, want)
		}
	}

	waitForAllSynced(t, h.server, refA, next, followers)
}

func TestASubscriberEndsUpAtTheNewestHead(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	start := nextUpdate(t, updates).Hash
	waitForAllSynced(t, h.server, refA, start, 1)

	// Every state is built first: rebuilding a store while a push is
	// reading it is the one thing a provider may not do, and the point
	// here is the moves, not the building.
	var heads []plumbing.Hash
	for _, mtu := range []int{4000, 6000, 9000} {
		heads = append(heads, h.provider.retune(t, refA, mtu))
	}

	// Now move the ref three times in a row, faster than a subscriber can
	// take them up. What it must not do is settle on one of the ones it
	// was told about first.
	for _, head := range heads {
		if err := h.server.SetHead(refA, head); err != nil {
			t.Fatalf("SetHead: %v", err)
		}
	}

	last := heads[len(heads)-1]

	u := followTo(t, updates, last)

	if u.Hash != last {
		t.Fatalf("subscriber settled at %s, want the newest head %s", u.Hash, last)
	}

	waitForAllSynced(t, h.server, refA, last, 1)
}

func TestALateSubscriberGetsTheCurrentHead(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, early := h.subscribe(t, ctx, refA)

	start := nextUpdate(t, early).Hash
	waitForAllSynced(t, h.server, refA, start, 1)

	next := h.provider.retune(t, refA, 9000)
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	followTo(t, early, next)
	waitForAllSynced(t, h.server, refA, next, 1)

	// Somebody arriving now holds nothing and is at nobody's mercy about
	// how far the ref has already travelled.
	_, late := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, late)

	if u.Hash != next {
		t.Errorf("late subscriber was served %s, want the current head %s", u.Hash, next)
	}
	if want := 17; u.ObjectCount != want {
		t.Errorf("late subscriber was sent %d objects, want the whole graph's %d", u.ObjectCount, want)
	}

	leaves, err := u.Graph.Leaves(u.Hash)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}
	if want := shared.LeafCount; len(leaves) != want {
		t.Errorf("late subscriber reconstructed %d leaves, want %d", len(leaves), want)
	}

	if prepared, _ := h.provider.counts(refA); prepared != 1 {
		t.Errorf("provider prepared %q %d times, want once", refA, prepared)
	}
}

func TestDataIsHeldUntilTheLastSubscriberLeaves(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first, firstUpdates := h.subscribe(t, ctx, refA)
	second, secondUpdates := h.subscribe(t, ctx, refA)

	nextUpdate(t, firstUpdates)
	nextUpdate(t, secondUpdates)

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eventually(t, "the server to notice one subscriber leaving", func() bool {
		return len(h.server.Subscribers()) == 1
	})

	// One of two gone is not the end of the ref: the other is still being
	// served from the same prepared objects.
	if _, released := h.provider.counts(refA); released != 0 {
		t.Errorf("provider released %q %d times while a subscriber was still following it", refA, released)
	}

	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eventually(t, "the provider to get its data back", func() bool {
		_, released := h.provider.counts(refA)

		return released == 1
	})

	if prepared, released := h.provider.counts(refA); prepared != 1 || released != 1 {
		t.Errorf("provider prepared %d and released %d, want one of each", prepared, released)
	}
}

func TestARefIsNotPreparedAgainUntilItHasBeenReleased(t *testing.T) {
	h := newHarness(t)

	// Letting go takes this provider a moment, which is the window a
	// second preparation could slip into.
	h.provider.releaseDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	leaving, updates := h.subscribe(t, ctx, refA)
	nextUpdate(t, updates)

	if err := leaving.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Somebody asks for the same ref while the provider is still busy
	// letting go of it. Subscribing waits for that to finish rather than
	// having the ref prepared underneath a release that is about to
	// discard it.
	_, rejoined := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, rejoined)

	want := []string{"prepare " + refA, "release " + refA, "prepare " + refA}

	got := h.provider.lifecycle()
	if len(got) != len(want) {
		t.Fatalf("provider saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("provider saw %v, want %v", got, want)
		}
	}

	// And what the second preparation built is still there to be worked
	// with, rather than having been swept up by the first one's release.
	next := h.provider.retune(t, refA, 9000)
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	moved := followTo(t, rejoined, next)

	if moved.Previous != u.Hash {
		t.Errorf("the new subscription moved from %s, want %s", moved.Previous, u.Hash)
	}
}

func TestOneSubscriberLeavingLeavesTheOthersServed(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	leaving, leavingUpdates := h.subscribe(t, ctx, refA)
	staying, stayingUpdates := h.subscribe(t, ctx, refA)

	nextUpdate(t, leavingUpdates)
	start := nextUpdate(t, stayingUpdates).Hash

	waitForAllSynced(t, h.server, refA, start, 2)

	if err := leaving.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eventually(t, "the server to notice one subscriber leaving", func() bool {
		return len(h.server.Subscribers()) == 1
	})

	next := h.provider.retune(t, refA, 9000)
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	u := followTo(t, stayingUpdates, next)

	if u.Previous != start {
		t.Errorf("the remaining subscriber moved from %s, want %s", u.Previous, start)
	}

	if err := staying.Err(); err != nil {
		t.Errorf("the remaining subscription failed: %v", err)
	}
}

func TestSubscribersOfOneRefAreListedApart(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, first := h.subscribe(t, ctx, refA)
	_, second := h.subscribe(t, ctx, refA)

	nextUpdate(t, first)
	nextUpdate(t, second)

	subs := h.server.Subscribers()
	if len(subs) != 2 {
		t.Fatalf("server lists %d subscribers, want 2", len(subs))
	}

	for _, sub := range subs {
		if sub.Ref != refA {
			t.Errorf("subscriber listed under %q, want %q", sub.Ref, refA)
		}
		if sub.Addr == "" {
			t.Error("subscriber listed without an address, so the two cannot be told apart")
		}
	}

	if subs[0].Addr == subs[1].Addr {
		t.Errorf("both subscribers are listed at %q", subs[0].Addr)
	}
}

func TestSubscribersArrivingTogetherPrepareOnce(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const followers = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		served  int
		heads   = map[plumbing.Hash]int{}
		streams = make([]<-chan client.Update, followers)
	)

	// All of them race into the same ref, so whichever gets there first
	// has the others waiting on the preparation it started.
	for i := range streams {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			c, err := client.Dial(ctx, h.addr)
			if err != nil {
				t.Errorf("Dial: %v", err)

				return
			}
			t.Cleanup(func() { c.Close() })

			updates, err := c.Subscribe(ctx, refA)
			if err != nil {
				t.Errorf("Subscribe: %v", err)

				return
			}

			streams[i] = updates
		}(i)
	}

	wg.Wait()

	for _, updates := range streams {
		if updates == nil {
			continue
		}

		u := nextUpdate(t, updates)

		mu.Lock()
		served++
		heads[u.Hash]++
		mu.Unlock()
	}

	if served != followers {
		t.Errorf("%d of %d subscribers were served", served, followers)
	}
	if len(heads) != 1 {
		t.Errorf("subscribers were served %d different trees, want one", len(heads))
	}

	if prepared, _ := h.provider.counts(refA); prepared != 1 {
		t.Errorf("provider prepared %q %d times, want once however many arrive at once", refA, prepared)
	}
}
