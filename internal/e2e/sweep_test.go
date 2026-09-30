package e2e

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/receive"
	"github.com/zonque/treevial/server"
	"github.com/zonque/treevial/structtree"
)

// refC is a third well-formed ref, so a sweep has one of each kind to do.
const refC = "refs/heads/printer-9/config"

// member is a node holding several refs over one store, which is what makes a
// sweep the whole moment rather than something done a ref at a time.
type member struct {
	t        *testing.T
	srv      *server.Server
	addr     string
	store    *objects.Store
	configs  map[string]*shared.Config
	builders map[string]*structtree.Builder
	heads    map[string]server.Head
	seq      uint64
}

func newMember(t *testing.T, refs ...string) *member {
	t.Helper()

	m := &member{
		t:        t,
		store:    objects.NewStore(),
		configs:  map[string]*shared.Config{},
		builders: map[string]*structtree.Builder{},
		heads:    map[string]server.Head{},
		seq:      1,
	}

	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.srv = srv

	for _, ref := range refs {
		config := shared.Example(ref)

		builder, err := structtree.NewBuilder(m.store, config)
		if err != nil {
			t.Fatalf("NewBuilder: %v", err)
		}

		root, err := builder.Build()
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		head := server.Head{Hash: root, Sequence: m.seq}

		if err := srv.Publish(ref, m.store, head); err != nil {
			t.Fatalf("Publish: %v", err)
		}

		m.configs[ref] = config
		m.builders[ref] = builder
		m.heads[ref] = head
	}

	m.addr = serve(t, srv)

	return m
}

// rebuild moves one ref's value without announcing it, the way the data behind
// a ref changing does.
func (m *member) rebuild(ref string, mtu int) plumbing.Hash {
	m.t.Helper()

	config := m.configs[ref]
	config.Network.Primary.MTU = mtu

	root, err := m.builders[ref].Build(&config.Network.Primary.MTU)
	if err != nil {
		m.t.Fatalf("Build: %v", err)
	}

	return root
}

// sweep compacts the store down to what survives, installs the new heads, and
// retargets every builder — the whole moment, in the order an application
// would do it. moved names the refs whose heads change, by the tree they now
// point at; the zero hash is a ref whose state is gone.
func (m *member) sweep(moved map[string]plumbing.Hash) int {
	m.t.Helper()

	m.seq++

	heads := map[string]server.Head{}
	for ref, hash := range moved {
		heads[ref] = server.Head{Hash: hash, Sequence: m.seq}
	}

	live := map[string]server.Head{}
	for ref, head := range m.heads {
		if next, named := heads[ref]; named {
			head = next
		}
		live[ref] = head
	}

	roots := make([]plumbing.Hash, 0, len(live))
	for _, head := range live {
		roots = append(roots, head.Hash)
	}

	compacted, dropped, err := m.store.Compact(roots...)
	if err != nil {
		m.t.Fatalf("Compact: %v", err)
	}
	if dropped == 0 {
		m.t.Error("the sweep reclaimed nothing")
	}

	reset, err := m.srv.Sweep(compacted, heads)
	if err != nil {
		m.t.Fatalf("Sweep: %v", err)
	}

	m.store = compacted
	m.heads = live

	for ref, builder := range m.builders {
		// A ref that went void has no tree left for its builder to
		// follow, so that builder starts again if the ref comes back.
		if live[ref].Hash.IsZero() {
			continue
		}

		if err := builder.Retarget(compacted); err != nil {
			m.t.Fatalf("Retarget %q: %v", ref, err)
		}
	}

	return reset
}

// One store, three refs, one sweep: one ref survives untouched, one moves, one
// goes void. Each subscriber hears exactly what became of its own ref.
func TestASweepTellsEachSubscriberWhatBecameOfItsRef(t *testing.T) {
	m := newMember(t, refA, refB, refC)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, settled := dial(t, ctx, m.addr, refA)
	_, moving := dial(t, ctx, m.addr, refB)
	_, going := dial(t, ctx, m.addr, refC)

	first := nextUpdate(t, settled)
	nextUpdate(t, moving)
	nextUpdate(t, going)

	for _, ref := range []string{refA, refB, refC} {
		waitForSync(t, m.srv, ref, m.heads[ref].Hash)
	}

	// refB's data changed; refC's is gone; refA's is as it was.
	movedTo := m.rebuild(refB, 9000)

	reset := m.sweep(map[string]plumbing.Hash{
		refB: movedTo,
		refC: plumbing.ZeroHash,
	})

	// refB's subscriber held a tree the compaction dropped, and so did
	// refC's; refA's held one that survived.
	if reset != 2 {
		t.Errorf("%d subscribers were reset, want 2", reset)
	}

	// The one that moved is sent its new state.
	if u := followTo(t, moving, m.heads[refB]); u.ObjectCount == 0 {
		t.Error("the moved ref's subscriber was sent nothing")
	}

	// The one that went is told its state is void, and stays connected.
	void := nextUpdate(t, going)
	if !void.Hash.IsZero() {
		t.Errorf("the void ref's subscriber was sent %s, want the zero hash", void.Hash)
	}
	if void.ObjectCount != 0 {
		t.Errorf("a void update carried %d objects, want none", void.ObjectCount)
	}

	// And the one that did not move hears nothing at all.
	select {
	case u := <-settled:
		t.Errorf("the untouched ref's subscriber was woken with %s", u.Hash)
	case <-time.After(250 * time.Millisecond):
	}

	// Its state is still exactly what it was, in the compacted store.
	if head, held := m.srv.Head(refA); !held || head.Hash != first.Hash {
		t.Errorf("refA is at %+v (held %v), want %s", head, held, first.Hash)
	}
	if !m.store.Has(first.Hash) {
		t.Error("the compacted store dropped the head it was told to keep")
	}
}

// A ref that went void can be given a head again, and whoever stayed is sent
// the whole tree.
func TestARefSweptToNothingCanComeBack(t *testing.T) {
	m := newMember(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, updates := dial(t, ctx, m.addr, refA)

	first := nextUpdate(t, updates)
	waitForSync(t, m.srv, refA, first.Hash)

	m.sweep(map[string]plumbing.Hash{refA: plumbing.ZeroHash})

	if u := nextUpdate(t, updates); !u.Hash.IsZero() {
		t.Fatalf("got %s, want the zero hash", u.Hash)
	}

	// Built afresh, since the tree its builder knew was reclaimed.
	config := shared.Example(refA)

	builder, err := structtree.NewBuilder(m.store, config)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	root, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	m.seq++
	back := server.Head{Hash: root, Sequence: m.seq}

	if err := m.srv.SetHead(refA, back); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	u := followTo(t, updates, back)
	if u.ObjectCount != first.ObjectCount {
		t.Errorf("the revived ref sent %d objects, want the whole tree's %d",
			u.ObjectCount, first.ObjectCount)
	}
}

// A client reconnecting after a sweep declares a tree the server has dropped,
// and is served the whole of the new state rather than refused.
func TestAClientReconnectingAfterASweepIsServedInFull(t *testing.T) {
	m := newMember(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cli, updates := dial(t, ctx, m.addr, refA)

	first := nextUpdate(t, updates)
	waitForSync(t, m.srv, refA, first.Hash)

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eventually(t, "the server to notice the disconnect", func() bool {
		return len(m.srv.Subscribers()) == 0
	})

	moved := m.rebuild(refA, 9000)
	m.sweep(map[string]plumbing.Hash{refA: moved})

	// Back with the hash it used to hold, which is now nowhere.
	again, err := client.Dial(ctx, m.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer again.Close()

	resumed, err := again.Resume(ctx, refA, first.Hash, receive.NewGraph())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	u := nextUpdate(t, resumed)
	if u.Hash != moved {
		t.Errorf("served %s, want the swept-to head %s", u.Hash, moved)
	}
	if u.ObjectCount != first.ObjectCount {
		t.Errorf("served %d objects, want the whole tree's %d", u.ObjectCount, first.ObjectCount)
	}
}

// A push whose acknowledgement arrives after the sweep that dropped its
// baseline must not put that baseline back: the next push would then compute a
// delta from a tree the new store no longer holds, and the subscription would
// die of it. Driven from a raw connection, because it needs the moment between
// reading a pack and acknowledging it.
func TestASweepOutrunsAnAcknowledgementInFlight(t *testing.T) {
	m := newMember(t, refA)

	nc, err := net.Dial("tcp", m.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer nc.Close()

	conn := wire.NewConn(nc)

	if err := conn.WriteRegister(refA, plumbing.ZeroHash, "probe"); err != nil {
		t.Fatalf("WriteRegister: %v", err)
	}

	// The whole tree, acknowledged, so this subscriber has a real baseline
	// for the sweep to drop.
	first := readUpdate(t, conn)
	if err := conn.WriteAck(first.Hash); err != nil {
		t.Fatalf("WriteAck: %v", err)
	}
	waitForSync(t, m.srv, refA, first.Hash)

	// A move, read but deliberately not acknowledged: the server is now
	// waiting for an acknowledgement of a push made from the old store.
	moved := m.rebuild(refA, 9000)
	m.seq++

	if err := m.srv.SetHead(refA, server.Head{Hash: moved, Sequence: m.seq}); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	second := readUpdate(t, conn)
	if second.Hash != moved {
		t.Fatalf("second update carried %s, want %s", second.Hash, moved)
	}

	// The sweep lands while that acknowledgement is still outstanding, and
	// reclaims both of the states this subscriber has been sent. The value
	// is one neither of them had, so neither tree survives the compaction —
	// content addressing would hand back the very same root otherwise.
	swept := m.rebuild(refA, 4321)

	if reset := m.sweep(map[string]plumbing.Hash{refA: swept}); reset != 1 {
		t.Errorf("%d subscribers were reset, want 1", reset)
	}

	// And only now does the old acknowledgement arrive.
	if err := conn.WriteAck(second.Hash); err != nil {
		t.Fatalf("WriteAck: %v", err)
	}

	// The subscription survives, and is sent the whole of the new state
	// rather than a delta from a tree that is gone.
	third := readUpdate(t, conn)

	if third.Hash != swept {
		t.Errorf("third update carried %s, want the swept-to %s", third.Hash, swept)
	}
	if third.ObjectCount != first.ObjectCount {
		t.Errorf("served %d objects, want the whole tree's %d", third.ObjectCount, first.ObjectCount)
	}
}

// readUpdate reads one update off a raw connection and drains its pack, which
// is what leaves the connection ready for the next message without
// acknowledging anything.
func readUpdate(t *testing.T, conn *wire.Conn) wire.ServerMessage {
	t.Helper()

	msg, err := conn.ReadServerMessage()
	if err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}
	if msg.Type != wire.Update {
		t.Fatalf("got %v, want an update", msg.Type)
	}

	if _, err := io.Copy(io.Discard, conn.PackReader()); err != nil {
		t.Fatalf("draining the pack: %v", err)
	}

	return msg
}
