package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/receive"
	"github.com/zonque/treevial/server"
)

// A ref whose state is gone is held and empty rather than taken away, and its
// subscribers are told so: an update carrying forty zeros, which the wire has
// always been able to say.
func TestAVoidRefTellsItsSubscribers(t *testing.T) {
	n := newNode(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := dial(t, ctx, n.addr, refA)

	first := nextUpdate(t, updates)
	waitForSync(t, n.srv, refA, first.Hash)

	n.seq++
	void := server.Head{Sequence: n.seq}

	if err := n.srv.SetHead(refA, void); err != nil {
		t.Fatalf("SetHead to a void head: %v", err)
	}

	u := followTo(t, updates, void)

	if !u.Hash.IsZero() {
		t.Errorf("the update carried %s, want the zero hash", u.Hash)
	}
	if u.ObjectCount != 0 {
		t.Errorf("a void head carried %d objects, want none", u.ObjectCount)
	}
	if u.Previous != first.Hash {
		t.Errorf("Previous = %s, want the state that was dropped (%s)", u.Previous, first.Hash)
	}

	// Held, not gone.
	if got, held := n.srv.Head(refA); !held || got != void {
		t.Errorf("Head() = %+v, %v; want %+v, true", got, held, void)
	}

	// The pair the README prescribes keeps the state that was dropped, and
	// only that: Previous still names a tree, so a client sweeping that way
	// holds one state until its next update rather than nothing.
	if _, err := u.Graph.Retain(u.Previous, u.Hash); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if got, want := u.Graph.Len(), first.ObjectCount; got != want {
		t.Errorf("the graph holds %d objects after the documented sweep, want the dropped state's %d", got, want)
	}

	// Letting go of it as well is retaining the void head alone, since
	// Retain ignores a zero root and retaining nothing keeps nothing.
	if _, err := u.Graph.Retain(u.Hash); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if got := u.Graph.Len(); got != 0 {
		t.Errorf("the graph still holds %d objects after retaining a void head", got)
	}
}

// And the subscription stays, so a ref given a real head again sends the whole
// tree to whoever is still following it.
func TestARefGivenAHeadAgainSendsTheWholeTree(t *testing.T) {
	n := newNode(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := dial(t, ctx, n.addr, refA)

	first := nextUpdate(t, updates)
	waitForSync(t, n.srv, refA, first.Hash)

	n.seq++
	if err := n.srv.SetHead(refA, server.Head{Sequence: n.seq}); err != nil {
		t.Fatalf("SetHead to a void head: %v", err)
	}

	followTo(t, updates, server.Head{Sequence: n.seq})

	// Back again, at the state it started from.
	n.seq++
	back := server.Head{Hash: first.Hash, Sequence: n.seq}

	if err := n.srv.SetHead(refA, back); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	u := followTo(t, updates, back)

	if u.ObjectCount != first.ObjectCount {
		t.Errorf("the revived ref sent %d objects, want the whole tree's %d",
			u.ObjectCount, first.ObjectCount)
	}
}

// A client arriving at a void ref holds nothing and is owed nothing, and is
// told that rather than walked into a hash that is not there.
func TestSubscribingToAVoidRefIsServedNothing(t *testing.T) {
	n := newNode(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	n.seq++
	if err := n.srv.SetHead(refA, server.Head{Sequence: n.seq}); err != nil {
		t.Fatalf("SetHead to a void head: %v", err)
	}

	_, updates := dial(t, ctx, n.addr, refA)

	u := nextUpdate(t, updates)
	if !u.Hash.IsZero() || u.ObjectCount != 0 {
		t.Errorf("got %s with %d objects, want the zero hash and none", u.Hash, u.ObjectCount)
	}
}

// A baseline the store does not hold is a state the server has dropped, so the
// client is resynchronised rather than refused.
func TestRegisteringWithABaselineTheStoreLacksIsServedInFull(t *testing.T) {
	n := newNode(t, refA)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, n.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	stranger := plumbing.NewHash("1111111111111111111111111111111111111111")

	updates, err := c.Resume(ctx, refA, stranger, receive.NewGraph())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	u := nextUpdate(t, updates)
	if u.ObjectCount == 0 {
		t.Error("a client holding a tree the server never had was sent nothing")
	}
	if head, _ := n.srv.Head(refA); u.Hash != head.Hash {
		t.Errorf("served %s, want the ref's head %s", u.Hash, head.Hash)
	}
}
