// Command clientside is a minimal treevial client living in a module of its
// own. It imports the client side and nothing else: no server package, no
// object store.
//
// It decodes into the same settings.Settings the server walked — the two
// sides share that one baseline struct, and nothing else about the wire
// format need concern either of them.
//
// The ref is given on the command line: treevial attaches no meaning to its
// shape, so how an application picks one is its own affair.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/structtree"

	"github.com/zonque/treevial-consumer-example/settings"
)

func main() {
	addr := flag.String("server", "127.0.0.1:9418", "address of the treevial server")
	ref := flag.String("ref", "", "head to subscribe to, e.g. refs/heads/printer-7/config (required)")
	id := flag.String("id", "", "name this client reports to the server (optional)")
	flag.Parse()

	if err := treevial.ValidateRef(*ref); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Naming the client is optional; it gives whoever runs the server
	// something better than an address to recognise this one by.
	conn, err := client.Dial(ctx, *addr, client.WithID(*id))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	log.Printf("subscribing to %s", *ref)

	updates, err := conn.Subscribe(ctx, *ref)
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

		log.Printf("%s -> %s, %d objects, %d bytes (%d in total)",
			u.Ref, u.Hash, u.ObjectCount, u.Bytes, u.TotalBytes)
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
