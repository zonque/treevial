// Command treevial-server synchronises a deeply nested Go struct per ref. A
// client names the head it wants; the server takes that ref verbatim, builds
// its configuration on connect, walks it with structtree into a git tree, and
// pushes it down the connection. A few seconds after a client has synced it
// changes one deeply nested field and pushes again, which is where the
// long-lived connection earns its keep: the second transfer costs four
// objects, not sixteen.
//
// It never polls the server for the state of its clients. Everything it prints
// and everything it sets off comes from a server.Watcher, which is told about
// subscriptions as they arrive, catch up and go.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/server"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:9418", "address to listen on")
	id := flag.String("id", "", "name this server reports to its clients (optional)")
	mutate := flag.Duration("mutate", 3*time.Second, "change a nested field this long after a client syncs (0 to disable)")
	flag.Parse()

	if err := run(*addr, *id, *mutate); err != nil {
		log.Fatal(err)
	}
}

func run(addr, id string, mutate time.Duration) error {
	provider := newDemoProvider()

	var opts []server.Option
	if id != "" {
		opts = append(opts, server.WithID(id))
	}

	srv, err := server.New(provider, opts...)
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}

	srv.Watch(newTracker(srv, provider, mutate))

	lis, listenErr := net.Listen("tcp", addr)
	if listenErr != nil {
		return fmt.Errorf("listen: %w", listenErr)
	}

	if named := srv.ID(); named != "" {
		log.Printf("listening on %s as %q; data is prepared per ref on connect", lis.Addr(), named)
	} else {
		log.Printf("listening on %s; data is prepared per ref on connect", lis.Addr())
	}

	go watchSignals(srv)

	return srv.Serve(lis)
}

// tracker follows the server's subscriptions. It prints each one as it
// happens, which is what a running server looks like from the outside, and
// uses the same events to decide when it is safe to change a ref's data.
type tracker struct {
	srv      *server.Server
	provider *demoProvider
	after    time.Duration

	mu sync.Mutex
	// changed is closed and replaced whenever anything happens, so waiting
	// for the server to reach some state is a wait rather than a poll.
	changed chan struct{}
	mutated map[string]bool
}

func newTracker(srv *server.Server, provider *demoProvider, after time.Duration) *tracker {
	return &tracker{
		srv:      srv,
		provider: provider,
		after:    after,
		changed:  make(chan struct{}),
		mutated:  map[string]bool{},
	}
}

// Observe implements server.Watcher. It runs on the goroutine serving that
// subscriber, so it says its piece and gets out of the way: the rebuild it may
// set off happens on a goroutine of its own.
func (t *tracker) Observe(e server.Event) {
	switch e.Kind {
	case server.Synced:
		log.Printf("[%s] %s synced at %s (%d B sent, %d B received)",
			e.Ref, who(e.Subscription), e.Synced, e.Sent, e.Received)
	default:
		log.Printf("[%s] %s %s", e.Ref, who(e.Subscription), e.Kind)
	}

	t.wake()

	if e.Kind != server.Synced || t.after == 0 {
		return
	}

	// One change per ref is enough to show it, and only once every
	// subscriber to that ref has taken up what it already has.
	if _, quiet := quietRefs(t.srv)[e.Ref]; !quiet {
		return
	}

	t.mu.Lock()
	first := !t.mutated[e.Ref]
	t.mutated[e.Ref] = true
	t.mu.Unlock()

	if first {
		go t.mutateOnce(e.Ref)
	}
}

// who names a subscriber the way a person would: by what it called itself, or
// by where it connected from if it called itself nothing.
func who(sub server.Subscription) string {
	if sub.ClientID == "" {
		return sub.Addr
	}

	return fmt.Sprintf("%s (%s)", sub.ClientID, sub.Addr)
}

// wake releases everything waiting for the server's state to change.
func (t *tracker) wake() {
	t.mu.Lock()
	changed := t.changed
	t.changed = make(chan struct{})
	t.mu.Unlock()

	close(changed)
}

// quiet is a ref nobody is mid-transfer on: every subscriber following it has
// taken up the head it points at.
type quiet struct {
	head        plumbing.Hash
	subscribers int
	sent        int64
}

// quietRefs returns the refs whose subscribers have all caught up. Several
// clients may follow one ref, so this is a statement about all of them —
// rebuilding a store while any one of them is being pushed to would have the
// provider writing under the server's feet.
func quietRefs(srv *server.Server) map[string]quiet {
	type tally struct {
		state  quiet
		behind bool
	}

	refs := map[string]*tally{}

	for _, sub := range srv.Subscribers() {
		t, following := refs[sub.Ref]
		if !following {
			t = &tally{state: quiet{head: sub.Head}}
			refs[sub.Ref] = t
		}

		t.state.subscribers++
		t.state.sent += sub.Sent

		if sub.Synced.IsZero() || sub.Synced != sub.Head {
			t.behind = true
		}
	}

	out := map[string]quiet{}
	for ref, t := range refs {
		if !t.behind {
			out[ref] = t.state
		}
	}

	return out
}

// awaitQuiet waits until every subscriber of ref has taken up its current
// head, and until that head is the one given if one is named. It is woken by
// the server's own events rather than a ticker.
func (t *tracker) awaitQuiet(ref string, head plumbing.Hash) (quiet, bool) {
	deadline := time.After(10 * time.Second)

	for {
		// Taken before looking, so a change that lands between the two
		// wakes this rather than being missed.
		t.mu.Lock()
		changed := t.changed
		t.mu.Unlock()

		if state, ok := quietRefs(t.srv)[ref]; ok && (head.IsZero() || state.head == head) {
			return state, true
		}

		select {
		case <-changed:
		case <-deadline:
			return quiet{}, false
		}
	}
}

func (t *tracker) mutateOnce(ref string) {
	state, ok := t.awaitQuiet(ref, plumbing.ZeroHash)
	if !ok {
		return
	}

	log.Printf("[%s] %d subscriber(s) synced at %s after %d bytes; setting Network.Primary.MTU in %s",
		ref, state.subscribers, state.head, state.sent, t.after)
	time.Sleep(t.after)

	// Somebody may have joined during the wait and still be taking up the
	// tree, and the rebuild writes to the store they are being served
	// from.
	state, ok = t.awaitQuiet(ref, plumbing.ZeroHash)
	if !ok {
		log.Printf("[%s] still mid-transfer; leaving it alone", ref)

		return
	}

	next, err := t.provider.Retune(ref)
	if err != nil {
		log.Printf("[%s] retune: %v", ref, err)

		return
	}

	log.Printf("[%s] moving -> %s and pushing to %d subscriber(s)", ref, next, state.subscribers)

	if err := t.srv.SetHead(ref, next); err != nil {
		log.Printf("[%s] set head: %v", ref, err)

		return
	}

	t.reportCost(ref, next, state.sent)
}

// reportCost waits for every subscriber to acknowledge the new head and then
// says what that push cost across all of them, measured against what had
// already gone out. The counts come from the connections themselves, so they
// include the pkt-line framing and are what the links carried rather than an
// estimate from the object count.
func (t *tracker) reportCost(ref string, head plumbing.Hash, before int64) {
	state, ok := t.awaitQuiet(ref, head)
	if !ok {
		return
	}

	log.Printf("[%s] %d subscriber(s) synced at %s; that push cost %d bytes, %d sent in total",
		ref, state.subscribers, head, state.sent-before, state.sent)
}

func watchSignals(srv *server.Server) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Print("shutting down")
	srv.Stop()
}
