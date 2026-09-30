// Command client follows one device's configuration across the members of a
// cluster.
//
// One treevial client is one connection to one server, so surviving the loss of
// a node is this program's own business rather than the library's. It is not
// much: keep the graph and the hash you hold, dial the next address, and resume
// from that hash. Because every member built the same objects from the same
// entries, the state the client is holding is one the next member it reaches
// already has, and the resume costs nothing — not because anything was
// coordinated, but because the state is content addressed.
//
// Run more than one of these, pointed at the addresses in a different order, to
// watch them arrive at the same hashes through different nodes. Then kill the
// node one of them is on.
//
// Nothing here imports the cluster's own package. The ref is derived from the
// device name the same way the nodes derive it, and neither side tells the
// other — treevial attaches no meaning to the shape of a ref.
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
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/receive"
	"github.com/zonque/treevial/structtree"
)

// retry is how long to wait before trying the next address. A node that has not
// applied the cluster's state yet holds no refs at all, so a client may arrive
// before there is anything to follow.
const retry = 2 * time.Second

func main() {
	device := flag.String("device", "", "the device whose configuration to follow (required)")
	servers := flag.String("servers", "127.0.0.1:9418,127.0.0.1:9419,127.0.0.1:9420",
		"the cluster's treevial addresses, tried in this order")
	flag.Parse()

	if *device == "" {
		log.Fatal("a device is required: pass -device")
	}

	addrs := strings.Split(*servers, ",")
	for i, addr := range addrs {
		addrs[i] = strings.TrimSpace(addr)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	f := &follower{
		device: *device,
		ref:    fmt.Sprintf("refs/heads/%s/config", *device),
		out:    log.New(os.Stderr, *device+" ", log.Ltime),
		graph:  receive.NewGraph(),
	}

	f.run(ctx, addrs)
}

// follower is one device's configuration and everything needed to go on
// following it through a different member.
type follower struct {
	device string
	ref    string
	out    *log.Logger

	// graph accumulates the objects, across connections as well as across
	// pushes: what a member sends is only what the client does not have,
	// and after a reconnection that is still true.
	graph *receive.Graph

	// held is the last hash that arrived. It is what the next member is
	// asked to bring the client up from, and is deliberately not the same
	// as applied: a member may push a state this client has decided not to
	// act on, and the delta it computes next is against what it sent.
	held plumbing.Hash

	// applied is the last hash this client decoded into config, and
	// sequence the ordinal it came with. An update older than that is one
	// a member behind the rest of the cluster pushed, and acting on it
	// would be going backwards.
	applied  plumbing.Hash
	sequence uint64

	config shared.Config

	pushes int
}

// run follows the ref for as long as the context lasts, moving to the next
// address whenever the connection it is on ends.
func (f *follower) run(ctx context.Context, addrs []string) {
	for attempt := 0; ctx.Err() == nil; attempt++ {
		addr := addrs[attempt%len(addrs)]

		if err := f.follow(ctx, addr); err != nil && ctx.Err() == nil {
			f.out.Printf("%s: %v", addr, err)
		}

		if ctx.Err() != nil {
			return
		}

		f.out.Printf("holding %s at sequence %d; next address in %s", short(f.held), f.sequence, retry)

		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// follow subscribes through one member and reads until the connection ends.
func (f *follower) follow(ctx context.Context, addr string) error {
	// The dead peer timeout is what notices a node whose host has gone
	// rather than one whose process closed its socket. A treevial
	// connection is quiet by design, so nothing else would.
	cli, err := client.Dial(ctx, addr,
		client.WithID(f.device),
		client.WithDeadPeerTimeout(10*time.Second),
	)
	if err != nil {
		return err
	}
	defer cli.Close()

	updates, err := cli.Resume(ctx, f.ref, f.held, f.graph)
	if err != nil {
		return err
	}

	f.out.Printf("connected to %s, resuming from %s", addr, short(f.held))

	for u := range updates {
		f.push(u, addr)
	}

	if ctx.Err() != nil {
		return nil
	}

	return cli.Err()
}

// push reports one update and, if it is not one to go backwards for, applies it.
func (f *follower) push(u client.Update, addr string) {
	f.pushes++

	// Whatever arrived is what the next member has to compute a delta
	// against, whether or not this client acts on it.
	f.held = u.Hash

	where := u.ServerID
	if where == "" {
		where = addr
	}

	if u.Sequence != 0 && u.Sequence < f.sequence {
		// This member is behind the one that was pushing before. The
		// ordinal is the only thing that can say so: the state it sent
		// is a perfectly good state, just not the newest one this
		// client has seen.
		f.out.Printf("push %d: %s at sequence %d from %s, behind the sequence %d already held — not applied",
			f.pushes, short(u.Hash), u.Sequence, where, f.sequence)

		return
	}

	f.out.Printf("push %d: %s seq %d from %s, %d object(s), %s (%s in total)",
		f.pushes, short(u.Hash), u.Sequence, where, u.ObjectCount,
		size(u.Bytes), size(u.TotalBytes))

	if u.Hash.IsZero() {
		// The ref is held and empty: what this client had has been
		// dropped, and it stays subscribed for whatever comes next.
		f.out.Print("  the state behind this ref is gone")

		f.config = shared.Config{}
		f.applied = plumbing.ZeroHash
		f.sequence = u.Sequence

		if _, err := u.Graph.Retain(u.Hash); err != nil {
			f.out.Printf("  sweep: %v", err)
		}

		return
	}

	// Decoded from what this client last applied rather than from the
	// update's own Previous, which are the same hash until a member pushes
	// something older and they are not the same again afterwards.
	if err := structtree.ApplySince(&f.config, u.Graph, f.applied, u.Hash); err != nil {
		f.out.Printf("  decode: %v", err)

		return
	}

	f.applied = u.Hash
	f.sequence = u.Sequence

	// Everything this client no longer needs: the state it holds is the
	// baseline for the next update, and nothing older is asked about.
	if _, err := u.Graph.Retain(u.Hash); err != nil {
		f.out.Printf("  sweep: %v", err)
	}

	f.out.Printf("  %s: MTU %d, gain %.1f, installed %s",
		f.config.Network.Hostname, f.config.Network.Primary.MTU,
		f.config.Audio.Gain, f.config.Device.Installed.Format(time.DateOnly))
}

// short is a hash as a person refers to one.
func short(h plumbing.Hash) string {
	if h.IsZero() {
		return "nothing"
	}

	return h.String()[:12]
}

// size renders a byte count measured on the connection, framing and all.
func size(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}

	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}
