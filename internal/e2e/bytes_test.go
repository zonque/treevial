package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/zonque/treevial/client"
)

func TestUpdateReportsWhatItCostOnTheWire(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, updates := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, updates)

	if u.Bytes <= 0 {
		t.Fatalf("update reports %d bytes for a push of %d objects", u.Bytes, u.ObjectCount)
	}

	// The first update is the whole connection so far, since nothing else
	// has come down it.
	if u.TotalBytes != u.Bytes {
		t.Errorf("first update cost %d bytes but the connection counted %d", u.Bytes, u.TotalBytes)
	}
}

func TestUpdateBytesAccumulateAcrossPushes(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, updates := h.subscribe(t, ctx, refA)

	first := nextUpdate(t, updates)

	next := h.provider.retune(t, refA, 9000)
	if err := h.server.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	second := nextUpdate(t, updates)

	if second.Bytes <= 0 {
		t.Fatalf("second update reports %d bytes", second.Bytes)
	}

	if want := first.Bytes + second.Bytes; second.TotalBytes != want {
		t.Errorf("connection counted %d bytes over two pushes, want %d", second.TotalBytes, want)
	}

	// The point of the open connection: a one-field change costs a
	// fraction of the first transfer.
	if second.Bytes >= first.Bytes {
		t.Errorf("a one-field push cost %d bytes, no less than the whole tree's %d",
			second.Bytes, first.Bytes)
	}

	if got := cli.Received(); got != second.TotalBytes {
		t.Errorf("client reports %d bytes received, but its last update counted %d",
			got, second.TotalBytes)
	}

	// Registration and two acknowledgements, and nothing else: the client
	// asks for nothing after the first message.
	if cli.Sent() <= 0 {
		t.Errorf("client reports %d bytes sent, but it registered and acknowledged twice", cli.Sent())
	}
}

func TestAnEmptyUpdateStillCostsItsMessage(t *testing.T) {
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

	// A client that names what it holds is sent no objects at all — but
	// the update announcing that still crossed the wire, which is what
	// measuring rather than deriving from the object count shows.
	again, err := client.Dial(ctx, h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer again.Close()

	resumed, err := again.Resume(ctx, refA, first.Hash, first.Graph)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	u := nextUpdate(t, resumed)

	if u.ObjectCount != 0 {
		t.Fatalf("server pushed %d objects to an already-synced client, want 0", u.ObjectCount)
	}

	// The update line -- "update", a hash and a zero count -- behind its
	// four-digit header, then the flush-pkt that ends an update with
	// nothing in it.
	want := int64(4 + len("update ") + 40 + len(" 0") + 1 + 4)

	if u.Bytes != want {
		t.Errorf("an empty update cost %d bytes, want %d", u.Bytes, want)
	}
}

func TestServerReportsWhatItExchangedWithEachSubscriber(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cli, updates := h.subscribe(t, ctx, refA)

	u := nextUpdate(t, updates)
	waitForSync(t, h.server, refA, u.Hash)

	subs := h.server.Subscribers()
	if len(subs) != 1 {
		t.Fatalf("got %d subscribers, want 1", len(subs))
	}

	// The two ends count the same bytes from opposite sides, so at rest
	// they agree exactly.
	if subs[0].Sent != u.TotalBytes {
		t.Errorf("server sent %d bytes, client received %d", subs[0].Sent, u.TotalBytes)
	}

	eventually(t, "the server to account for what the client sent", func() bool {
		subs := h.server.Subscribers()

		return len(subs) == 1 && subs[0].Received == cli.Sent()
	})
}
