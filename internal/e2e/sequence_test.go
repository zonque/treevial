package e2e

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
	"github.com/zonque/treevial/structtree"
)

// initialRoot is the root the provider's first build of ref produces, computed
// here in a store of its own so that a test can name the head the provider is
// about to hand over before it has handed it over.
//
// structtree is deterministic — map keys sorted, tree entries in git's
// canonical order, nothing hashed that is not the value itself — so the same
// value hashes the same way wherever it is built. That is the property the
// whole cluster arrangement rests on, and naming a head this way leans on it
// on purpose.
func initialRoot(t *testing.T, ref string) plumbing.Hash {
	t.Helper()

	builder, err := structtree.NewBuilder(objects.NewStore(), shared.Example(ref))
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	root, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return root
}

// A sequenced ref moves forward and nowhere else. A head at a sequence the ref
// has reached already is a re-delivered one, and taking it would push a state
// the subscribers have been told about.
func TestASequencedRefOnlyMovesForward(t *testing.T) {
	h := newHarness(t)
	h.provider.sequenced = true

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	if first := nextUpdate(t, updates); first.Sequence != 1 {
		t.Fatalf("first update carried sequence %d, want the prepared 1", first.Sequence)
	}

	ahead := h.provider.retune(t, refA, 9000)
	ahead.Sequence = 2

	if err := h.server.SetHead(refA, ahead); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	behind := h.provider.retune(t, refA, 1500)

	for _, tc := range []struct {
		name string
		seq  uint64
	}{
		{"an older sequence", 1},
		{"the sequence it is already at", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := behind
			stale.Sequence = tc.seq

			if err := h.server.SetHead(refA, stale); !errors.Is(err, server.ErrNotAdvancing) {
				t.Errorf("SetHead = %v, want ErrNotAdvancing", err)
			}
			if got := h.server.Head(refA); got != ahead {
				t.Errorf("head moved to %+v, want it left at %+v", got, ahead)
			}
		})
	}
}

// A refused SetHead must wake nobody. Waking on every apply that did not move
// a ref would push a zero-object update at a sequence the subscriber has
// already been told, over and over.
func TestARefusedSetHeadWakesNobody(t *testing.T) {
	h := newHarness(t)
	h.provider.sequenced = true

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)
	first := nextUpdate(t, updates)
	waitForSync(t, h.server, refA, first.Hash)

	stale := h.provider.retune(t, refA, 9000)
	stale.Sequence = 1

	if err := h.server.SetHead(refA, stale); !errors.Is(err, server.ErrNotAdvancing) {
		t.Fatalf("SetHead = %v, want ErrNotAdvancing", err)
	}

	select {
	case u := <-updates:
		t.Fatalf("a refused SetHead pushed %s at sequence %d", u.Hash, u.Sequence)
	case <-time.After(250 * time.Millisecond):
	}
}

// Ordering across a mixture of sequenced and unsequenced heads is undefined, so
// a head of the wrong kind is refused rather than taken.
func TestAHeadOfTheWrongKindIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sequenced bool
		sequence  uint64
	}{
		{"a sequenced head on an unsequenced ref", false, 7},
		{"an unsequenced head on a sequenced ref", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.provider.sequenced = tc.sequenced

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			_, updates := h.subscribe(t, ctx, refA)
			nextUpdate(t, updates)

			before := h.server.Head(refA)

			next := h.provider.retune(t, refA, 9000)
			next.Sequence = tc.sequence

			err := h.server.SetHead(refA, next)
			if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
				t.Errorf("SetHead = %v (code %s), want a refusal with %s", err, got, treevial.CodeInvalid)
			}
			if got := h.server.Head(refA); got != before {
				t.Errorf("head moved to %+v, want it left at %+v", got, before)
			}
		})
	}
}

// A head set while the ref is still being prepared stands. Both the provider
// and the application install through the same door, so the ref ends up at
// whichever of the two is further on, and a preparation that answers with an
// older state does not drag the ref back to it.
//
// The head set here is the one the provider is about to return, at a sequence
// consensus has since reached, so the push stays serveable and the ordinal is
// the only thing under test.
func TestAHeadSetDuringPreparationIsNotOverwritten(t *testing.T) {
	h := newHarness(t)
	h.provider.sequenced = true
	h.provider.preparing = make(chan struct{})
	h.provider.prepareGate = make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	// The subscriber is in the ref's member list by now, which is what
	// makes the ref visible to SetHead while the provider still has not
	// answered.
	<-h.provider.preparing

	ahead := server.Head{Hash: initialRoot(t, refA), Sequence: 9}
	if err := h.server.SetHead(refA, ahead); err != nil {
		t.Fatalf("SetHead during preparation: %v", err)
	}

	close(h.provider.prepareGate)

	if got := nextUpdate(t, updates); got.Sequence != 9 {
		t.Errorf("client was pushed sequence %d, want the 9 consensus set", got.Sequence)
	}
	if got := h.server.Head(refA); got != ahead {
		t.Errorf("head = %+v, want %+v", got, ahead)
	}
}

// The other half of that window: the kind is fixed by whichever of the two
// installs a head first, so a provider whose head then disagrees fails the
// preparation rather than leaving a ref whose ordering is undefined. Read off
// the wire directly, because a refused subscription yields no Update to carry
// the refusal.
//
// The provider here is unsequenced and the head consensus set is not.
func TestAPreparedHeadOfTheWrongKindFailsThePreparation(t *testing.T) {
	h := newHarness(t)
	h.provider.preparing = make(chan struct{})
	h.provider.prepareGate = make(chan struct{})

	nc, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer nc.Close()

	conn := wire.NewConn(nc)
	if err := conn.WriteRegister(refA, plumbing.ZeroHash, "probe"); err != nil {
		t.Fatalf("WriteRegister: %v", err)
	}

	<-h.provider.preparing

	// Sequenced, while the provider this server was given is not.
	ahead := server.Head{Hash: initialRoot(t, refA), Sequence: 9}
	if err := h.server.SetHead(refA, ahead); err != nil {
		t.Fatalf("SetHead during preparation: %v", err)
	}

	close(h.provider.prepareGate)

	if _, err := conn.ReadServerMessage(); err == nil {
		t.Fatal("the subscription was served despite a head of the wrong kind")
	} else if got := treevial.CodeOf(err); got != treevial.CodeInternal {
		t.Errorf("code = %s, want %s", got, treevial.CodeInternal)
	}
}
