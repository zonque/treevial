package e2e

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/server"
)

// namedHarness is newHarness with the server given a name of its own.
func namedHarness(t *testing.T, id string) *harness {
	t.Helper()

	provider := newTestProvider()

	srv, err := server.New(provider, server.WithID(id))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return &harness{server: srv, provider: provider, addr: serve(t, srv)}
}

// The name belongs to the connection, so it is on every update of it, not only
// the first.
func TestEveryUpdateCarriesTheServersName(t *testing.T) {
	h := namedHarness(t, "node-3")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	first := nextUpdate(t, updates)
	if first.ServerID != "node-3" {
		t.Errorf("first update ServerID = %q, want %q", first.ServerID, "node-3")
	}

	next := h.provider.retune(t, refA, 9000)
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	second := followTo(t, updates, next)
	if second.ServerID != "node-3" {
		t.Errorf("second update ServerID = %q, want %q", second.ServerID, "node-3")
	}
}

// A server that does not name itself sends no server line at all, so the first
// thing a client reads is its update — which is what keeps an unnamed server
// byte for byte what it has always been.
func TestAnUnnamedServerSaysNothingExtra(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, updates)

	if u.ServerID != "" {
		t.Errorf("ServerID = %q, want empty", u.ServerID)
	}
	if u.ObjectCount == 0 {
		t.Error("the first message was not an update carrying objects")
	}
}

// The name precedes the refusal, so an implementation that logs it learns which
// server turned it away. Read off the wire directly, because a refused
// subscription yields no Update for this client to carry it on.
func TestTheNameArrivesBeforeARefusal(t *testing.T) {
	h := namedHarness(t, "node-3")
	h.provider.failPrepare = true

	nc, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer nc.Close()

	conn := wire.NewConn(nc)

	if err := conn.WriteRegister(refA, plumbing.ZeroHash, "probe"); err != nil {
		t.Fatalf("WriteRegister: %v", err)
	}

	first, err := conn.ReadServerMessage()
	if err != nil {
		t.Fatalf("the name did not arrive first: %v", err)
	}
	if first.Type != wire.Announce {
		t.Fatalf("first message was %v, want the server naming itself", first.Type)
	}
	if first.ServerID != "node-3" {
		t.Errorf("ServerID = %q, want %q", first.ServerID, "node-3")
	}

	// Then, and only then, the refusal.
	if _, err = conn.ReadServerMessage(); err == nil {
		t.Fatal("the refused registration reported no error")
	}
	if got := treevial.CodeOf(err); got != treevial.CodeInternal {
		t.Errorf("code = %s, want %s", got, treevial.CodeInternal)
	}
}
