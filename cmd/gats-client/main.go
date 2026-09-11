// Command gats-client dials a gats server, registers under its own ID, and
// then waits. Everything it prints is reconstructed from objects interpreted
// as they arrived on the wire; no file is ever written and no repository
// exists.
//
// The configuration it decodes into is the same type the server walks: both
// sides share one baseline struct, so the paths on the wire and the fields in
// memory are the same thing seen from two ends.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/holoplot/gats"
	"github.com/holoplot/gats/client"
	"github.com/holoplot/gats/internal/demo"
	"github.com/holoplot/gats/receive"
	"github.com/holoplot/gats/structtree"
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

	// The value being synchronised. Each update settles it completely.
	var config demo.Config

	push := 0

	for u := range updates {
		push++

		log.Printf("push %d: %s -> %s, %d objects received", push, u.Ref, u.Hash, u.ObjectCount)

		report, err := render(u, &config)
		if err != nil {
			return err
		}

		if _, err := os.Stdout.WriteString(report); err != nil {
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

// render describes an update from both ends: which paths moved in the tree,
// and the part of the Go value they landed in.
func render(u client.Update, config *demo.Config) (string, error) {
	// Only the subtrees that moved are decoded; on a push that changed one
	// field that is one leaf, whatever the size of the rest of the struct.
	if err := structtree.ApplySince(config, u.Graph, u.Previous, u.Hash); err != nil {
		return "", err
	}

	changes, err := u.Graph.Diff(u.Previous, u.Hash)
	if err != nil {
		return "", err
	}

	if len(changes) == 0 {
		return "  (no change)\n", nil
	}

	var b strings.Builder

	// The serialised tree, so it is visible which paths became blobs and
	// which became trees, and which object hashes this push replaced.
	listing, err := u.Graph.Listing(u.Hash)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(&b, "  tree %s\n", u.Hash)
	b.WriteString(indent(listing))

	paths := make([]string, 0, len(changes))

	for _, c := range changes {
		paths = append(paths, c.Path)

		if c.Kind == receive.Deleted {
			fmt.Fprintf(&b, "  %s %s\n", c.Kind.Symbol(), c.Path)

			continue
		}

		fmt.Fprintf(&b, "  %s %s  %s  %q\n", c.Kind.Symbol(), c.Path, c.Hash.String()[:8], string(c.Content))
	}

	// Show only the part of the struct the changes landed in. On the first
	// push everything is new, so that is the whole value; after a
	// one-field change it is the struct holding that field.
	subtree := changedSubtree(paths)

	name := subtree
	if name == "" {
		name = fmt.Sprintf("%T", config)
	}

	value, err := valueAt(config, subtree)
	if err != nil {
		fmt.Fprintf(&b, "  %s: %v\n", name, err)

		return b.String(), nil
	}

	encoded, err := json.MarshalIndent(value, "  ", "  ")
	if err != nil {
		return "", err
	}

	fmt.Fprintf(&b, "  %s = %s\n", name, encoded)

	return b.String(), nil
}

// indent shifts a block of lines under the push they belong to.
func indent(block string) string {
	var b strings.Builder

	for _, l := range strings.Split(strings.TrimSuffix(block, "\n"), "\n") {
		fmt.Fprintf(&b, "    %s\n", l)
	}

	return b.String()
}
