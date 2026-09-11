// Command gats-client dials a gats server, registers the ref it wants, and then
// waits. Everything it prints is reconstructed from objects interpreted as they
// arrived on the wire; no file is ever written and no repository exists.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/holoplot/gats"
	"github.com/holoplot/gats/client"
	"github.com/holoplot/gats/receive"
)

func main() {
	addr := flag.String("server", "127.0.0.1:9418", "address of the gats server")
	id := flag.String("id", "", "client ID; decides which ref the server serves (required)")
	flag.Parse()

	if *id == "" {
		log.Fatal("a client ID is required: pass -id")
	}

	if err := run(*addr, *id); err != nil {
		log.Fatal(err)
	}
}

func run(addr, clientID string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cli, err := client.Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer cli.Close()

	log.Printf("subscribing as %q to %s, expecting %s", clientID, addr, gats.RefFor(clientID))

	updates, err := cli.Subscribe(ctx, clientID)
	if err != nil {
		return err
	}

	push := 0

	for u := range updates {
		push++

		log.Printf("push %d: %s -> %s, %d objects received", push, u.Ref, u.Hash, u.ObjectCount)

		listing, err := render(u)
		if err != nil {
			return err
		}

		if _, err := os.Stdout.WriteString(listing); err != nil {
			return err
		}
	}

	// A cancelled context is how the signal handler ends the run, not a
	// failure.
	if ctx.Err() != nil {
		log.Print("disconnected")

		return nil
	}

	return cli.Err()
}

// render describes an update: the whole tree the first time, and only the paths
// that differ from the previous state on every push after that.
func render(u client.Update) (string, error) {
	if u.Previous.IsZero() {
		return u.Graph.Format(u.Hash)
	}

	changes, err := u.Graph.Diff(u.Previous, u.Hash)
	if err != nil {
		return "", err
	}

	if len(changes) == 0 {
		return "  (no change)\n", nil
	}

	var b strings.Builder

	for _, c := range changes {
		if c.Kind == receive.Deleted {
			fmt.Fprintf(&b, "  %s %s\n", c.Kind.Symbol(), c.Path)

			continue
		}

		fmt.Fprintf(&b, "  %s %s  %s  %q\n", c.Kind.Symbol(), c.Path, c.Hash.String()[:8], string(c.Content))
	}

	return b.String(), nil
}
