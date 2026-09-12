package e2e

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/internal/demo"
	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/receive"
	"github.com/zonque/treevial/structtree"
)

func TestServerPreparesDataWhenAClientConnects(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if prepared, _ := h.provider.counts(refA); prepared != 0 {
		t.Fatalf("data was prepared before the client connected")
	}

	_, updates := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, updates)

	if prepared, _ := h.provider.counts(refA); prepared != 1 {
		t.Errorf("provider prepared data %d times, want once", prepared)
	}
	if want := 17; u.ObjectCount != want {
		t.Errorf("pushed %d objects, want %d", u.ObjectCount, want)
	}

	leaves, err := u.Graph.Leaves(u.Hash)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}
	if want := demo.LeafCount; len(leaves) != want {
		t.Fatalf("client reconstructed %d leaves, want %d", len(leaves), want)
	}
}

func TestClientIsServedAtItsOwnRef(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, updates)

	if u.Ref != refA {
		t.Errorf("client reports ref %q, want %q", u.Ref, refA)
	}

	want := refA

	clients := h.server.Subscribers()
	if len(clients) != 1 {
		t.Fatalf("got %d clients, want 1", len(clients))
	}
	if clients[0].Ref != want {
		t.Errorf("server publishes at %q, want %q", clients[0].Ref, want)
	}
	if clients[0].Ref != refA {
		t.Errorf("server ref %q disagrees with clientref.For", clients[0].Ref)
	}
}

func TestEachClientGetsItsOwnData(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, first := h.subscribe(t, ctx, refA)
	_, second := h.subscribe(t, ctx, refB)

	a := nextUpdate(t, first)
	b := nextUpdate(t, second)

	if a.Hash == b.Hash {
		t.Error("both clients were served the same tree")
	}

	leavesA, err := a.Graph.Leaves(a.Hash)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}

	leavesB, err := b.Graph.Leaves(b.Hash)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}

	// The provider labels each struct with the ref it was handed, so the
	// leaf shows what the server was actually asked for, verbatim.
	if got, want := string(leavesA["Device/Name"]), `"`+refA+`"`; got != want {
		t.Errorf("printer-7 leaf: got %s, want %s", got, want)
	}
	if got, want := string(leavesB["Device/Name"]), `"`+refB+`"`; got != want {
		t.Errorf("printer-8 leaf: got %s, want %s", got, want)
	}
}

func TestServerPushesOnlyChangedObjectsOnTheOpenConnection(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	first := nextUpdate(t, updates)

	// The server must know the client is caught up before it can compute a
	// minimal second push.
	waitForSync(t, h.server, refA, first.Hash)

	store := h.provider.store(t, refA)

	v2, err := store.ReplaceBlob(first.Hash, "Network/Primary/MTU", []byte("9000"))
	if err != nil {
		t.Fatalf("ReplaceBlob: %v", err)
	}
	if err := h.server.SetHead(refA, v2); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	second := nextUpdate(t, updates)

	if second.Hash != v2 {
		t.Errorf("second update hash %s, want %s", second.Hash, v2)
	}
	// The rewritten blob plus the Primary, Network and root trees on its
	// path; everything else is pruned.
	if want := 4; second.ObjectCount != want {
		t.Errorf("second push carried %d objects, want %d", second.ObjectCount, want)
	}
	if second.Previous != first.Hash {
		t.Errorf("second update reported previous %s, want %s", second.Previous, first.Hash)
	}

	changes, err := second.Graph.Diff(second.Previous, second.Hash)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "Network/Primary/MTU" {
		t.Fatalf("got changes %v, want only Network/Primary/MTU", changes)
	}
	if got, want := string(changes[0].Content), "9000"; got != want {
		t.Errorf("changed content: got %q, want %q", got, want)
	}
}

func TestServerTracksWhenAClientHasSynced(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, updates)
	waitForSync(t, h.server, refA, u.Hash)

	clients := h.server.Subscribers()
	if len(clients) != 1 {
		t.Fatalf("got %d clients, want 1", len(clients))
	}
	if clients[0].Synced != u.Hash {
		t.Errorf("client synced at %s, want %s", clients[0].Synced, u.Hash)
	}
	if clients[0].Head != u.Hash {
		t.Errorf("client head %s, want %s", clients[0].Head, u.Hash)
	}
}

func TestDisconnectReleasesTheClientsResources(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, updates := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, updates)
	waitForSync(t, h.server, refA, u.Hash)

	if _, released := h.provider.counts(refA); released != 0 {
		t.Fatal("resources were released while the client was still connected")
	}

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eventually(t, "the server to notice the disconnect", func() bool {
		return len(h.server.Subscribers()) == 0
	})

	eventually(t, "the provider to release the client's resources", func() bool {
		_, released := h.provider.counts(refA)

		return released == 1
	})

	if h.server.Head(refA) != plumbing.ZeroHash {
		t.Error("the disconnected client's ref is still published")
	}
}

func TestSetHeadForAnUnknownClientFails(t *testing.T) {
	h := newHarness(t)

	err := h.server.SetHead("nobody", plumbing.NewHash("1111111111111111111111111111111111111111"))
	if err == nil {
		t.Error("SetHead accepted an unknown client")
	}
}

func TestClientReconnectingIsPreparedAgain(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, updates := h.subscribe(t, ctx, refA)
	first := nextUpdate(t, updates)
	waitForSync(t, h.server, refA, first.Hash)

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eventually(t, "the server to notice the disconnect", func() bool {
		return len(h.server.Subscribers()) == 0
	})

	// Reconnecting under the same ID gets freshly prepared data, and the
	// client says what it already holds so the server sends nothing.
	c2, err := client.Dial(ctx, h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c2.Close()

	updates2, err := c2.Resume(ctx, refA, first.Hash, first.Graph)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	second := nextUpdate(t, updates2)
	if second.ObjectCount != 0 {
		t.Errorf("server pushed %d objects to an already-synced client, want 0", second.ObjectCount)
	}
	if second.Hash != first.Hash {
		t.Errorf("update hash %s, want %s", second.Hash, first.Hash)
	}

	if prepared, _ := h.provider.counts(refA); prepared != 2 {
		t.Errorf("provider prepared data %d times, want twice", prepared)
	}
}

func TestSecondConnectionWithTheSameIDIsRejected(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)
	nextUpdate(t, updates)

	c2, err := client.Dial(ctx, h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c2.Close()

	updates2, err := c2.Subscribe(ctx, refA)
	if err == nil {
		err = drainForError(t, c2, updates2)
	}

	if got := treevial.CodeOf(err); got != treevial.CodeAlreadyExists {
		t.Errorf("got error %v (code %s), want %s", err, got, treevial.CodeAlreadyExists)
	}
}

func TestClientWithAnUnusableRefIsRejected(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	updates, err := c.Subscribe(ctx, "../../heads/somebody-else")
	if err == nil {
		err = drainForError(t, c, updates)
	}

	if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
		t.Errorf("got error %v (code %s), want %s", err, got, treevial.CodeInvalid)
	}
	if prepared, _ := h.provider.counts("../../heads/somebody-else"); prepared != 0 {
		t.Error("the server prepared data for an unusable ref")
	}
}

func TestAConnectionThatDoesNotRegisterFirstIsRejected(t *testing.T) {
	h := newHarness(t)

	// The wire package is used directly here, because a client always
	// registers before anything else.
	nc, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer nc.Close()

	conn := wire.NewConn(nc)

	if err := conn.WriteAck(plumbing.NewHash("1111111111111111111111111111111111111111")); err != nil {
		t.Fatalf("WriteAck: %v", err)
	}

	if _, err = conn.ReadServerMessage(); treevial.CodeOf(err) != treevial.CodeInvalid {
		t.Errorf("got error %v (code %s), want %s", err, treevial.CodeOf(err), treevial.CodeInvalid)
	}
}

func TestSubscribingWithAnUnknownSyncedHashFails(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	unknown := plumbing.NewHash("1111111111111111111111111111111111111111")

	updates, err := c.Resume(ctx, refA, unknown, receive.NewGraph())
	if err == nil {
		err = drainForError(t, c, updates)
	}

	if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
		t.Errorf("got error %v (code %s), want %s", err, got, treevial.CodeInvalid)
	}
}

// drainForError waits for a subscription to fail, for the cases where the
// server's rejection surfaces on the first receive rather than at registration.
func drainForError(t *testing.T, c *client.Client, updates <-chan client.Update) error {
	t.Helper()

	select {
	case u, ok := <-updates:
		if ok {
			t.Fatalf("got update %s where an error was expected", u.Hash)
		}

		return c.Err()
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the server to reject the subscription")
	}

	return nil
}

func TestClientRebuildsTheStructTheServerPublished(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	first := nextUpdate(t, updates)

	// Both sides share the same baseline struct, so the client decodes
	// into the very type the server walked.
	var got demo.Config
	if err := applyUpdate(t, &got, first); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Device.Name != refA {
		t.Errorf("Device.Name: got %q, want %q", got.Device.Name, refA)
	}
	if got.Network.Primary == nil || got.Network.Primary.MTU != 1500 {
		t.Errorf("Network.Primary: got %+v, want MTU 1500", got.Network.Primary)
	}
	if got.Network.Secondary != nil {
		t.Errorf("Network.Secondary: got %+v, want nil", got.Network.Secondary)
	}
	if !proto.Equal(got.Audio.Delay, durationpb.New(12*time.Millisecond)) {
		t.Errorf("Audio.Delay: got %v, want 12ms", got.Audio.Delay)
	}

	// Rebuilding the decoded struct must reproduce the server's tree
	// exactly, which is only true if nothing was lost on the way.
	if rebuild(t, &got) != first.Hash {
		t.Errorf("rebuilt tree %s does not match the published %s", rebuild(t, &got), first.Hash)
	}
}

func TestClientStructFollowsALaterPush(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	first := nextUpdate(t, updates)
	waitForSync(t, h.server, refA, first.Hash)

	var cfg demo.Config
	if err := applyUpdate(t, &cfg, first); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	store := h.provider.store(t, refA)

	next, err := store.ReplaceBlob(first.Hash, "Network/Primary/MTU", []byte("9000"))
	if err != nil {
		t.Fatalf("ReplaceBlob: %v", err)
	}
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	second := nextUpdate(t, updates)

	// The second push carried four objects, but the client applies the
	// whole tree, so the struct ends up wholly current.
	if err := applyUpdate(t, &cfg, second); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if cfg.Network.Primary.MTU != 9000 {
		t.Errorf("MTU: got %d, want 9000", cfg.Network.Primary.MTU)
	}
	if cfg.Device.Name != refA {
		t.Errorf("an untouched field changed: %q", cfg.Device.Name)
	}
	if rebuild(t, &cfg) != second.Hash {
		t.Errorf("rebuilt tree does not match the published %s", second.Hash)
	}
}

// applyUpdate decodes an update into dst, the way a client would.
func applyUpdate(t *testing.T, dst any, u client.Update) error {
	t.Helper()

	leaves, err := u.Graph.Leaves(u.Hash)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}

	return structtree.Apply(dst, leaves)
}

// rebuild stores dst and returns its root hash, so a decoded struct can be
// compared with the tree it came from.
func rebuild(t *testing.T, dst any) plumbing.Hash {
	t.Helper()

	root, err := structtree.Build(objects.NewStore(), dst)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return root
}

func TestClientFollowsPushesIncrementally(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	// An Update carries exactly what ApplySince needs: the hash the
	// subscription was at, and the one it has moved to.
	var config demo.Config

	first := nextUpdate(t, updates)
	if err := structtree.ApplySince(&config, first.Graph, first.Previous, first.Hash); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	if rebuild(t, &config) != first.Hash {
		t.Fatal("the first update did not produce the whole value")
	}

	waitForSync(t, h.server, refA, first.Hash)

	store := h.provider.store(t, refA)

	next, err := store.ReplaceBlob(first.Hash, "Network/Primary/MTU", []byte("9000"))
	if err != nil {
		t.Fatalf("ReplaceBlob: %v", err)
	}
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	second := nextUpdate(t, updates)

	if err := structtree.ApplySince(&config, second.Graph, second.Previous, second.Hash); err != nil {
		t.Fatalf("ApplySince: %v", err)
	}

	if config.Network.Primary.MTU != 9000 {
		t.Errorf("MTU: got %d, want 9000", config.Network.Primary.MTU)
	}
	// Fields under subtrees that did not move were never decoded again,
	// and are still right.
	if config.Device.Name != refA {
		t.Errorf("Device.Name: got %q, want %q", config.Device.Name, refA)
	}
	if config.Audio.Gain != -6.5 {
		t.Errorf("Audio.Gain: got %v, want -6.5", config.Audio.Gain)
	}
	if rebuild(t, &config) != second.Hash {
		t.Errorf("incremental application does not match the published %s", second.Hash)
	}
}

func TestServerRefusesARefItCannotUse(t *testing.T) {
	h := newHarness(t)

	// The client builds a ref from its ID and would never send these, so
	// the wire package is used directly: this is the server's own guard on
	// text it takes verbatim.
	for _, ref := range []string{
		"printer-7",               // not a ref at all
		"refs/heads/../../escape", // climbs out of the namespace
		"refs/heads/has space",    // not a usable ref name
	} {
		nc, err := net.Dial("tcp", h.addr)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		conn := wire.NewConn(nc)

		if err := conn.WriteRegister(ref, plumbing.ZeroHash); err != nil {
			t.Fatalf("WriteRegister: %v", err)
		}

		_, err = conn.ReadServerMessage()
		if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
			t.Errorf("ref %q: got error %v (code %s), want %s", ref, err, got, treevial.CodeInvalid)
		}

		if prepared, _ := h.provider.counts(ref); prepared != 0 {
			t.Errorf("ref %q: the server prepared data for it", ref)
		}

		nc.Close()
	}
}

func TestServerServesAnyWellFormedRefItIsGiven(t *testing.T) {
	h := newHarness(t)

	// Nothing about refs/heads/<id>/config is special to the server: it
	// serves whatever ref a client names, because it derives nothing.
	ref := "refs/devices/hall-a/row-3/seat-9"

	nc, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer nc.Close()

	conn := wire.NewConn(nc)

	if err := conn.WriteRegister(ref, plumbing.ZeroHash); err != nil {
		t.Fatalf("WriteRegister: %v", err)
	}

	msg, err := conn.ReadServerMessage()
	if err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}

	if msg.ObjectCount != 17 {
		t.Errorf("pushed %d objects, want 17", msg.ObjectCount)
	}

	if prepared, _ := h.provider.counts(ref); prepared != 1 {
		t.Errorf("provider prepared %q %d times, want once", ref, prepared)
	}

	subs := h.server.Subscribers()
	if len(subs) != 1 || subs[0].Ref != ref {
		t.Errorf("subscribers %+v, want one at %q", subs, ref)
	}
}
