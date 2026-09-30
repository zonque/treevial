package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/receive"
	"github.com/zonque/treevial/server"
	"github.com/zonque/treevial/structtree"
)

// node is a member holding one ref of its own: the value, the store its objects
// live in, the builder that keeps the two in step, and the sequence its log is
// at. There is no provider — the ref exists because this node holds it.
type node struct {
	t       *testing.T
	srv     *server.Server
	addr    string
	config  *shared.Config
	store   *objects.Store
	builder *structtree.Builder
	seq     uint64
}

func newNode(t *testing.T, ref string) *node {
	t.Helper()

	n := &node{t: t, config: shared.Example(ref), store: objects.NewStore(), seq: 1}

	builder, err := structtree.NewBuilder(n.store, n.config)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	n.builder = builder

	// The front-loaded build, paid once at startup rather than when a
	// client happens to arrive.
	root, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.srv = srv

	if err := srv.Publish(ref, n.store, server.Head{Hash: root, Sequence: n.seq}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	n.addr = serve(t, srv)

	return n
}

// apply is one log entry: change a field, rebuild the path to it, move the ref.
func (n *node) apply(ref string, mtu int) server.Head {
	n.t.Helper()

	n.config.Network.Primary.MTU = mtu

	root, err := n.builder.Build(&n.config.Network.Primary.MTU)
	if err != nil {
		n.t.Fatalf("Build: %v", err)
	}

	n.seq++
	head := server.Head{Hash: root, Sequence: n.seq}

	if err := n.srv.SetHead(ref, head); err != nil {
		n.t.Fatalf("SetHead: %v", err)
	}

	return head
}

// A client following a published ref across several applies, on a server with
// no provider: the ref existed before the client arrived and outlives it.
func TestAClientFollowsAPublishedRef(t *testing.T) {
	n := newNode(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := dial(t, ctx, n.addr, refA)

	first := nextUpdate(t, updates)
	if first.Sequence != 1 {
		t.Errorf("first update carried sequence %d, want the published 1", first.Sequence)
	}
	if first.ObjectCount == 0 {
		t.Error("the first push carried no objects")
	}
	waitForSync(t, n.srv, refA, first.Hash)

	for _, mtu := range []int{4000, 9000} {
		head := n.apply(refA, mtu)

		u := followTo(t, updates, head)
		if u.Sequence != head.Sequence {
			t.Errorf("update carried sequence %d, want %d", u.Sequence, head.Sequence)
		}
		waitForSync(t, n.srv, refA, head.Hash)
	}
}

// A published ref is not released by the last subscriber leaving: the
// application holds it, so it is still there to be moved and still there for
// the next client.
func TestAPublishedRefOutlivesItsSubscribers(t *testing.T) {
	n := newNode(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, updates := dial(t, ctx, n.addr, refA)
	first := nextUpdate(t, updates)
	waitForSync(t, n.srv, refA, first.Hash)

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eventually(t, "the server to notice the disconnect", func() bool {
		return len(n.srv.Subscribers()) == 0
	})

	// Still held, and still movable with nobody connected at all.
	head := n.apply(refA, 9000)

	if got, _ := n.srv.Head(refA); got != head {
		t.Errorf("Head() = %+v after the last subscriber left, want %+v", got, head)
	}

	// And the next client is served the state it moved to.
	_, rejoined := dial(t, ctx, n.addr, refA)

	if u := nextUpdate(t, rejoined); u.Hash != head.Hash {
		t.Errorf("the next client was served %s, want %s", u.Hash, head.Hash)
	}
}

// A ref nobody holds is refused, on a server with no provider to ask.
func TestSubscribingToARefNobodyHoldsIsRefused(t *testing.T) {
	n := newNode(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, n.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	updates, err := c.Subscribe(ctx, refB)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if u, ok := <-updates; ok {
		t.Fatalf("a ref the server does not hold was served: %s", u.Hash)
	}
	if got := treevial.CodeOf(c.Err()); got != treevial.CodeInvalid {
		t.Errorf("code = %s (%v), want %s", got, c.Err(), treevial.CodeInvalid)
	}
}

// Unpublishing says the node no longer holds the objects its subscribers are
// being served from, so their subscriptions end rather than following a ref
// that can never be pushed again.
func TestUnpublishEndsTheSubscriptions(t *testing.T) {
	n := newNode(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := dial(t, ctx, n.addr, refA)
	first := nextUpdate(t, updates)
	waitForSync(t, n.srv, refA, first.Hash)

	if err := n.srv.Unpublish(refA); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}

	select {
	case u, ok := <-updates:
		if ok {
			t.Errorf("an unpublished ref pushed %s", u.Hash)
		}
	case <-time.After(10 * time.Second):
		t.Error("the subscription outlived the ref it was following")
	}

	// Unpublished is not the same as void: the ref is gone, not empty.
	if got, held := n.srv.Head(refA); held {
		t.Errorf("Head() = %+v, true after Unpublish, want it unheld", got)
	}
	if err := n.srv.SetHead(refA, server.Head{Hash: first.Hash, Sequence: 99}); !errors.Is(err, server.ErrUnknownRef) {
		t.Errorf("SetHead after Unpublish = %v, want ErrUnknownRef", err)
	}
}

// The claim the whole arrangement rests on: two members build the same state
// from the same data, so a client synced from one resumes onto the other and is
// served the difference rather than the whole tree again.
func TestAClientResumesOntoAnotherMember(t *testing.T) {
	first := newNode(t, refA)
	second := newNode(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// One graph, carried from one member to the other, the way an
	// application holding its state across a reconnect would.
	graph := receive.NewGraph()

	one, err := client.Dial(ctx, first.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	updates, err := one.Resume(ctx, refA, plumbing.ZeroHash, graph)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	nextUpdate(t, updates)

	// Both members apply the same entry, and must reach the same head.
	head := first.apply(refA, 9000)
	if other := second.apply(refA, 9000); other != head {
		t.Fatalf("the two members built different heads: %+v and %+v", head, other)
	}

	u := followTo(t, updates, head)

	if err := one.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Onto the other member, declaring what it already holds.
	two, err := client.Dial(ctx, second.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer two.Close()

	resumed, err := two.Resume(ctx, refA, u.Hash, graph)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Nothing has moved since, so the member it arrived at owes it nothing.
	if caught := nextUpdate(t, resumed); caught.ObjectCount != 0 {
		t.Errorf("the resumed subscription was sent %d objects for a state it already held", caught.ObjectCount)
	}

	moved := second.apply(refA, 1500)

	next := followTo(t, resumed, moved)

	if next.ObjectCount == 0 {
		t.Error("the resumed subscription was sent nothing for a move")
	}
	if next.ObjectCount > 8 {
		t.Errorf("the resumed subscription was sent %d objects for one field, want a handful", next.ObjectCount)
	}
}
