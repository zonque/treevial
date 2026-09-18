// Command treevial-client dials a treevial server, registers under its own ID,
// and then waits. Everything it prints is reconstructed from objects
// interpreted as they arrived on the wire; no file is ever written and no
// repository exists.
//
// The configuration it decodes into is the same type the server walks: both
// sides share one baseline struct, so the paths on the wire and the fields in
// memory are the same thing seen from two ends.
//
// Turning a client ID into a head is this program's own convention, and lives
// nowhere else: the client package subscribes to whatever ref it is given, and
// the server serves whatever ref it is sent.
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

	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/structtree"
)

func main() {
	addr := flag.String("server", "127.0.0.1:9418", "address of the treevial server")
	id := flag.String("id", "", "client ID; decides which ref the server serves (required)")
	flag.Parse()

	if *id == "" {
		log.Fatal("a client ID is required: pass -id")
	}

	if err := run(*addr, *id); err != nil {
		log.Fatal(err)
	}
}

// refFor is how this program decides which head a client follows. Another
// application would map its own identities to refs however it liked — treevial
// attaches no meaning to the shape.
func refFor(clientID string) string {
	return fmt.Sprintf("refs/heads/%s/config", clientID)
}

func run(addr, clientID string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cli, err := client.Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer cli.Close()

	ref := refFor(clientID)

	log.Printf("subscribing as %q to %s, asking for %s", clientID, addr, ref)

	updates, err := cli.Subscribe(ctx, ref)
	if err != nil {
		return err
	}

	// The value being synchronised. Each update settles it completely.
	var config shared.Config

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
func render(u client.Update, config *shared.Config) (string, error) {
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

	// Every object this push moved, blobs and the trees above them, so it
	// is visible which paths are blobs, which are trees, and how far up a
	// change to one leaf reached.
	changeset, err := u.Graph.ListingSince(u.Previous, u.Hash)
	if err != nil {
		return "", err
	}

	b.WriteString(indent(changeset))

	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, c.Path)
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

	for l := range strings.SplitSeq(strings.TrimSuffix(block, "\n"), "\n") {
		fmt.Fprintf(&b, "    %s\n", l)
	}

	return b.String()
}
