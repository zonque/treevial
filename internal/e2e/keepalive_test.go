package e2e

import (
	"context"
	"testing"
	"time"
)

// TestSubscriptionSurvivesAQuietStretch checks that a subscription left idle,
// with keepalive pinging switched on, is still there when the server decides to
// push.
//
// It does not reach gRPC's ping-strike threshold: grpc-go clamps a client's
// ping interval to ten seconds, so provoking GOAWAY through a real client would
// take some forty seconds of idling. The server's ping policy is covered
// directly, and quickly, by TestServerNeverAnswersPingsWithEnhanceYourCalm.
//
// What it does cover is that a quiet subscription is still live: the server's
// keepalive settings never reap it, and the client's never give up on it.
func TestSubscriptionSurvivesAQuietStretch(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, updates := h.subscribe(t, ctx, "printer-7")

	first := nextUpdate(t, updates)
	waitForSync(t, h.server, "printer-7", first.Hash)

	// Nothing crosses the connection for a while.
	time.Sleep(2 * time.Second)

	if err := c.Err(); err != nil {
		t.Fatalf("connection died while idle: %v", err)
	}

	// The connection must still be usable for an unprompted push.
	store := h.provider.store(t, "printer-7")

	v2, err := store.ReplaceBlob(first.Hash, "b/c/leaf-07", []byte("leaf-07 v2\n"))
	if err != nil {
		t.Fatalf("ReplaceBlob: %v", err)
	}
	if err := h.server.SetHead("printer-7", v2); err != nil {
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
