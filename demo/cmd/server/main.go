// Command treevial-server synchronises a deeply nested Go struct per ref. A
// client names the head it wants; the server takes that ref verbatim, builds
// its configuration on connect, walks it with structtree into a git tree, and
// pushes it down the connection. A few seconds after a client has synced it
// changes one deeply nested field and pushes again, which is where the
// long-lived connection earns its keep: the second transfer costs four
// objects, not sixteen.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/server"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:9418", "address to listen on")
	mutate := flag.Duration("mutate", 3*time.Second, "change a nested field this long after a client syncs (0 to disable)")
	flag.Parse()

	if err := run(*addr, *mutate); err != nil {
		log.Fatal(err)
	}
}

func run(addr string, mutate time.Duration) error {
	provider := newDemoProvider()
	srv := server.New(provider)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	log.Printf("listening on %s; data is prepared per ref on connect", lis.Addr())

	if mutate > 0 {
		go mutateSyncedRefs(srv, provider, mutate)
	}

	go watchSignals(srv)

	return srv.Serve(lis)
}

// mutateSyncedRefs watches for refs every subscriber has caught up on and,
// once one has, changes a field of its configuration and moves it, which makes
// the server push to all of them of its own accord. One change per ref is
// enough to show it.
func mutateSyncedRefs(srv *server.Server, provider *demoProvider, after time.Duration) {
	done := map[string]bool{}

	for range time.Tick(50 * time.Millisecond) {
		for ref := range quietRefs(srv) {
			if done[ref] {
				continue
			}

			done[ref] = true

			go mutateOnce(srv, provider, ref, after)
		}
	}
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
// head, and until that head is the one given if one is named.
func awaitQuiet(srv *server.Server, ref string, head plumbing.Hash) (quiet, bool) {
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		state, ok := quietRefs(srv)[ref]
		if ok && (head.IsZero() || state.head == head) {
			return state, true
		}

		time.Sleep(10 * time.Millisecond)
	}

	return quiet{}, false
}

func mutateOnce(srv *server.Server, provider *demoProvider, ref string, after time.Duration) {
	state, ok := awaitQuiet(srv, ref, plumbing.ZeroHash)
	if !ok {
		return
	}

	log.Printf("[%s] %d subscriber(s) synced at %s after %d bytes; setting Network.Primary.MTU in %s",
		ref, state.subscribers, state.head, state.sent, after)
	time.Sleep(after)

	// Somebody may have joined during the wait and still be taking up the
	// tree, and the rebuild writes to the store they are being served
	// from.
	state, ok = awaitQuiet(srv, ref, plumbing.ZeroHash)
	if !ok {
		log.Printf("[%s] still mid-transfer; leaving it alone", ref)

		return
	}

	next, err := provider.Retune(ref)
	if err != nil {
		log.Printf("[%s] retune: %v", ref, err)

		return
	}

	log.Printf("[%s] moving -> %s and pushing to %d subscriber(s)", ref, next, state.subscribers)

	if err := srv.SetHead(ref, next); err != nil {
		log.Printf("[%s] set head: %v", ref, err)

		return
	}

	reportCost(srv, ref, next, state.sent)
}

// reportCost waits for every subscriber to acknowledge the new head and then
// says what that push cost across all of them, measured against what had
// already gone out. The counts come from the connections themselves, so they
// include the pkt-line framing and are what the links carried rather than an
// estimate from the object count.
func reportCost(srv *server.Server, ref string, head plumbing.Hash, before int64) {
	state, ok := awaitQuiet(srv, ref, head)
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
