// Command clientside is a minimal gats client living in a module of its own.
// It imports the client side and nothing else: no server package, no object
// store.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/holoplot/gats"
	"github.com/holoplot/gats/client"
)

func main() {
	addr := flag.String("server", "127.0.0.1:9418", "address of the gats server")
	id := flag.String("id", "", "client ID (required)")
	flag.Parse()

	if err := gats.ValidateID(*id); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	conn, err := client.Dial(ctx, *addr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	log.Printf("subscribing as %q, expecting %s", *id, gats.RefFor(*id))

	updates, err := conn.Subscribe(ctx, *id)
	if err != nil {
		log.Fatal(err)
	}

	for u := range updates {
		log.Printf("%s -> %s, %d objects", u.Ref, u.Hash, u.ObjectCount)

		leaves, err := u.Graph.Leaves(u.Hash)
		if err != nil {
			log.Fatal(err)
		}

		for path, content := range leaves {
			log.Printf("  %s = %q", path, content)
		}
	}

	if ctx.Err() != nil {
		os.Exit(0)
	}

	if err := conn.Err(); err != nil {
		log.Fatal(err)
	}
}
