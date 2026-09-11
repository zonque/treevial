// Command clientside is a minimal gats client living in a module of its own.
// It imports the client side and nothing else: no server package, no object
// store.
//
// It decodes into the same settings.Settings the server walked — the two
// sides share that one baseline struct, and nothing else about the wire
// format need concern either of them.
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
	"github.com/holoplot/gats/structtree"

	"github.com/holoplot/gats-consumer-example/settings"
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

	// Each update settles this value completely.
	var current settings.Settings

	for u := range updates {
		// Decodes only what moved since the previous push.
		if err := structtree.ApplySince(&current, u.Graph, u.Previous, u.Hash); err != nil {
			log.Fatal(err)
		}

		log.Printf("%s -> %s, %d objects", u.Ref, u.Hash, u.ObjectCount)
		log.Printf("  owner   %s (%s)", current.Owner.Name, current.Owner.Team)
		log.Printf("  display brightness %d, rotation %d", current.Display.Brightness, current.Display.Rotation)
		log.Printf("  seen    %s", current.Seen.AsTime())
	}

	if ctx.Err() != nil {
		os.Exit(0)
	}

	if err := conn.Err(); err != nil {
		log.Fatal(err)
	}
}
