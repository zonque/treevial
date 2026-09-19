package e2e

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/server"
)

// recorder is a watcher that keeps what it saw, which is all a test needs to
// say whether the server reported the right things in the right order.
type recorder struct {
	mu     sync.Mutex
	events []server.Event
	// seen is closed and replaced on every event, so a test can wait for
	// one instead of sleeping.
	seen chan struct{}
	// during is run inside the handler, where the guarantees about what a
	// handler may do are worth checking.
	during func(server.Event)
}

func newRecorder() *recorder {
	return &recorder{seen: make(chan struct{})}
}

func (r *recorder) Observe(e server.Event) {
	if r.during != nil {
		r.during(e)
	}

	r.mu.Lock()
	r.events = append(r.events, e)
	wake := r.seen
	r.seen = make(chan struct{})
	r.mu.Unlock()

	close(wake)
}

// await waits for the recorded events to satisfy cond, so a test never has to
// guess how long the server will take.
func (r *recorder) await(t *testing.T, what string, cond func([]server.Event) bool) []server.Event {
	t.Helper()

	deadline := time.After(10 * time.Second)

	for {
		r.mu.Lock()
		events := append([]server.Event(nil), r.events...)
		wake := r.seen
		r.mu.Unlock()

		if cond(events) {
			return events
		}

		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %s; saw %v", what, kinds(events))
		}
	}
}

func kinds(events []server.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind.String())
	}

	return out
}

func counted(events []server.Event, kind server.EventKind) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}

	return n
}

func TestWatcherSeesASubscriptionThroughItsLife(t *testing.T) {
	h := newHarness(t)

	rec := newRecorder()
	h.server.Watch(rec)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, err := client.Dial(ctx, h.addr, client.WithID("press-hall-a-7"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	updates, err := cli.Subscribe(ctx, refA)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	u := nextUpdate(t, updates)

	events := rec.await(t, "the subscription to be reported as synced", func(events []server.Event) bool {
		return counted(events, server.Synced) == 1
	})

	if events[0].Kind != server.Subscribed {
		t.Errorf("first event was %s, want %s", events[0].Kind, server.Subscribed)
	}

	synced := events[len(events)-1]
	if synced.Kind != server.Synced {
		t.Fatalf("last event was %s, want %s", synced.Kind, server.Synced)
	}
	if synced.Ref != refA {
		t.Errorf("event ref %q, want %q", synced.Ref, refA)
	}
	if synced.ClientID != "press-hall-a-7" {
		t.Errorf("event client ID %q, want %q", synced.ClientID, "press-hall-a-7")
	}
	if synced.Synced != u.Hash {
		t.Errorf("event reports %s synced, want %s", synced.Synced, u.Hash)
	}
	if synced.Synced != synced.Head {
		t.Errorf("event reports %s synced at head %s; a caught-up subscriber should show both alike",
			synced.Synced, synced.Head)
	}

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events = rec.await(t, "the subscription to be reported as gone", func(events []server.Event) bool {
		return counted(events, server.Unsubscribed) == 1
	})

	last := events[len(events)-1]
	if last.Kind != server.Unsubscribed {
		t.Errorf("last event was %s, want %s", last.Kind, server.Unsubscribed)
	}
	if last.ClientID != "press-hall-a-7" {
		t.Errorf("departure reported for %q, want %q", last.ClientID, "press-hall-a-7")
	}
}

func TestEverySubscriberIsReportedSynced(t *testing.T) {
	h := newHarness(t)

	rec := newRecorder()
	h.server.Watch(rec)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const followers = 3

	streams := make([]<-chan client.Update, followers)
	for i := range streams {
		_, streams[i] = h.subscribe(t, ctx, refA)
	}

	for _, updates := range streams {
		nextUpdate(t, updates)
	}

	rec.await(t, "all three to take up the first tree", func(events []server.Event) bool {
		return counted(events, server.Synced) == followers
	})

	next := h.provider.retune(t, refA, 9000)
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	for _, updates := range streams {
		followTo(t, updates, next)
	}

	events := rec.await(t, "all three to take up the move", func(events []server.Event) bool {
		return counted(events, server.Synced) == 2*followers
	})

	// The second round of reports is about the new head, one per
	// subscriber: the ref moved once and everyone moved with it.
	moved := 0
	for _, e := range events {
		if e.Kind == server.Synced && e.Synced == next {
			moved++
		}
	}

	if moved != followers {
		t.Errorf("%d subscribers were reported synced at %s, want %d", moved, next, followers)
	}
}

func TestAHandlerMaySeeAndAskTheServer(t *testing.T) {
	h := newHarness(t)

	rec := newRecorder()

	var (
		mu       sync.Mutex
		listed   = map[server.EventKind]int{}
		refFound = map[server.EventKind]bool{}
	)

	// Handlers run with no server lock held, so asking the server what it
	// knows — the obvious thing to do from one — must work rather than
	// deadlock.
	rec.during = func(e server.Event) {
		subs := h.server.Subscribers()

		found := false
		for _, sub := range subs {
			if sub.Addr == e.Addr {
				found = true
			}
		}

		mu.Lock()
		listed[e.Kind] = len(subs)
		refFound[e.Kind] = found
		mu.Unlock()
	}

	h.server.Watch(rec)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, updates := h.subscribe(t, ctx, refA)
	nextUpdate(t, updates)

	rec.await(t, "the first sync", func(events []server.Event) bool {
		return counted(events, server.Synced) == 1
	})

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec.await(t, "the departure", func(events []server.Event) bool {
		return counted(events, server.Unsubscribed) == 1
	})

	mu.Lock()
	defer mu.Unlock()

	if !refFound[server.Synced] {
		t.Error("a handler asking about a synced subscriber could not find it")
	}

	// Gone means gone: by the time the departure is reported the server no
	// longer lists it.
	if refFound[server.Unsubscribed] {
		t.Error("a departed subscriber was still listed when its departure was reported")
	}
	if listed[server.Unsubscribed] != 0 {
		t.Errorf("server listed %d subscribers when the last one left, want 0", listed[server.Unsubscribed])
	}
}

func TestTheServerTracksTheNameAClientGaveItself(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	named, err := client.Dial(ctx, h.addr, client.WithID("press-hall-a-7"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer named.Close()

	updates, err := named.Subscribe(ctx, refA)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	nextUpdate(t, updates)

	// A second client may call itself whatever it likes, including the
	// same thing: the name is a label, not an identity the server keys on.
	anonymous, err := client.Dial(ctx, h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer anonymous.Close()

	quiet, err := anonymous.Subscribe(ctx, refA)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	nextUpdate(t, quiet)

	eventually(t, "both subscribers to be listed", func() bool {
		return len(h.server.Subscribers()) == 2
	})

	names := map[string]int{}
	for _, sub := range h.server.Subscribers() {
		names[sub.ClientID]++
	}

	if names["press-hall-a-7"] != 1 {
		t.Errorf("server lists %d subscribers named %q, want 1", names["press-hall-a-7"], "press-hall-a-7")
	}
	if names[""] != 1 {
		t.Errorf("server lists %d unnamed subscribers, want 1", names[""])
	}

	if got := named.ID(); got != "press-hall-a-7" {
		t.Errorf("client reports its own name as %q, want %q", got, "press-hall-a-7")
	}
}

func TestAnUnusableClientIDIsRefusedBeforeDialling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Nothing is listening on this address: the name is refused before a
	// connection is even attempted.
	_, err := client.Dial(ctx, "127.0.0.1:1", client.WithID("press hall a 7"))
	if err == nil {
		t.Fatal("a client ID with a space was accepted")
	}

	if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
		t.Errorf("got error %v (code %s), want %s", err, got, treevial.CodeInvalid)
	}
}
