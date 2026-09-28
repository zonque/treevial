package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/internal/tcpkeep"
	"github.com/zonque/treevial/server"
)

// The option bounds unresponsiveness, not silence, so a healthy pair must sync
// exactly as it would without it — including across a second push, which is the
// one a too-eager timeout would cut short.
func TestAShortBudgetDoesNotDisturbAHealthyConnection(t *testing.T) {
	provider := newTestProvider()

	srv, err := server.New(provider, server.WithDeadPeerTimeout(tcpkeep.MinTimeout))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h := &harness{server: srv, provider: provider, addr: serve(t, srv)}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cli, err := client.Dial(ctx, h.addr, client.WithDeadPeerTimeout(tcpkeep.MinTimeout))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { cli.Close() })

	updates, err := cli.Subscribe(ctx, refA)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	first := nextUpdate(t, updates)
	if first.ObjectCount == 0 {
		t.Fatal("the first push carried nothing")
	}

	// Past the idle half of the budget, so probes are actually going out
	// while nothing is being pushed — which is the state a too-eager
	// timeout would tear down.
	time.Sleep(tcpkeep.MinTimeout/2 + time.Second)

	next := h.provider.retune(t, refA, 9000)
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	second := followTo(t, updates, next)
	if second.Hash != next {
		t.Errorf("second push reached %s, want %s", second.Hash, next)
	}

	if err := cli.Err(); err != nil {
		t.Errorf("the connection reported %v after sitting idle", err)
	}
}
