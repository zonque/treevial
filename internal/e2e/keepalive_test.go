package e2e

import (
	"context"
	"testing"
	"time"
)

// TestSubscriptionSurvivesAQuietStretch checks that a subscription left idle is
// still there when the server decides to push.
//
// Neither side sets a deadline, so nothing on the connection counts down while
// it is quiet: a subscription ends when one side closes it, and not before.
func TestSubscriptionSurvivesAQuietStretch(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, updates := h.subscribe(t, ctx, refA)

	first := nextUpdate(t, updates)
	waitForSync(t, h.server, refA, first.Hash)

	// Nothing crosses the connection for a while.
	time.Sleep(2 * time.Second)

	if err := c.Err(); err != nil {
		t.Fatalf("connection died while idle: %v", err)
	}

	// The connection must still be usable for an unprompted push.
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
	if err := c.Err(); err != nil {
		t.Errorf("client error after second push: %v", err)
	}
}
