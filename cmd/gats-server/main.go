// Command gats-server serves a nested tree of ten blobs per client. A client
// identifies itself in the gats-client-id request header; the server prepares
// that client's objects on connect, publishes them at
// refs/heads/<client-id>/config, and pushes them down the stream. A few seconds
// after a client has synced it rewrites one leaf and pushes again, which is
// where the long-lived connection earns its keep: the second transfer costs
// four objects, not fourteen.
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

	"github.com/holoplot/gats"
	"github.com/holoplot/gats/server"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:9418", "address to listen on")
	mutate := flag.Duration("mutate", 3*time.Second, "rewrite a leaf this long after a client syncs (0 to disable)")
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

	log.Printf("listening on %s; data is prepared per client on connect", lis.Addr())

	if mutate > 0 {
		go mutateSyncedClients(srv, provider, mutate)
	}

	go watchSignals(srv)

	return srv.Serve(lis)
}

// mutateSyncedClients watches for clients that have caught up and, once each
// has, rewrites a leaf of its tree and moves its ref, which makes the server
// push again of its own accord. One rewrite per client is enough to show it.
func mutateSyncedClients(srv *server.Server, provider *demoProvider, after time.Duration) {
	done := map[string]bool{}

	for range time.Tick(50 * time.Millisecond) {
		for _, c := range srv.Clients() {
			if done[c.ID] || c.Synced.IsZero() || c.Synced != c.Head {
				continue
			}

			done[c.ID] = true

			go mutateOnce(srv, provider, c.ID, c.Head, after)
		}
	}
}

func mutateOnce(
	srv *server.Server,
	provider *demoProvider,
	clientID string,
	head plumbing.Hash,
	after time.Duration,
) {
	log.Printf("[%s] synced at %s; rewriting b/c/leaf-07 in %s", clientID, head, after)
	time.Sleep(after)

	store := provider.store(clientID)
	if store == nil {
		log.Printf("[%s] gone before its leaf could be rewritten", clientID)

		return
	}

	next, err := store.ReplaceBlob(head, "b/c/leaf-07", []byte("leaf-07 v2\n"))
	if err != nil {
		log.Printf("[%s] rewrite leaf: %v", clientID, err)

		return
	}

	log.Printf("[%s] moving %s -> %s and pushing", clientID, gats.RefFor(clientID), next)

	if err := srv.SetHead(clientID, next); err != nil {
		log.Printf("[%s] set head: %v", clientID, err)
	}
}

func watchSignals(srv *server.Server) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Print("shutting down")
	srv.Stop()
}
