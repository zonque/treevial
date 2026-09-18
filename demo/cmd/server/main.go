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

// mutateSyncedRefs watches for subscribers that have caught up and, once each
// has, changes a field of its configuration and moves its ref, which makes the
// server push again of its own accord. One change per ref is enough to show it.
func mutateSyncedRefs(srv *server.Server, provider *demoProvider, after time.Duration) {
	done := map[string]bool{}

	for range time.Tick(50 * time.Millisecond) {
		for _, sub := range srv.Subscribers() {
			if done[sub.Ref] || sub.Synced.IsZero() || sub.Synced != sub.Head {
				continue
			}

			done[sub.Ref] = true

			go mutateOnce(srv, provider, sub, after)
		}
	}
}

func mutateOnce(
	srv *server.Server,
	provider *demoProvider,
	sub server.Subscription,
	after time.Duration,
) {
	ref := sub.Ref

	log.Printf("[%s] synced at %s after %d bytes; setting Network.Primary.MTU in %s",
		ref, sub.Head, sub.Sent, after)
	time.Sleep(after)

	next, err := provider.Retune(ref)
	if err != nil {
		log.Printf("[%s] retune: %v", ref, err)

		return
	}

	log.Printf("[%s] moving -> %s and pushing", ref, next)

	if err := srv.SetHead(ref, next); err != nil {
		log.Printf("[%s] set head: %v", ref, err)

		return
	}

	reportCost(srv, ref, next, sub.Sent)
}

// reportCost waits for the subscriber to acknowledge the new head and then
// says what that push cost, measured against what had already gone out. The
// counts come from the connection itself, so they include the pkt-line framing
// and are what the link carried rather than an estimate from the object count.
func reportCost(srv *server.Server, ref string, head plumbing.Hash, before int64) {
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		for _, sub := range srv.Subscribers() {
			if sub.Ref != ref || sub.Synced != head {
				continue
			}

			log.Printf("[%s] synced at %s; that push cost %d bytes, %d sent in total",
				ref, head, sub.Sent-before, sub.Sent)

			return
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func watchSignals(srv *server.Server) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Print("shutting down")
	srv.Stop()
}
