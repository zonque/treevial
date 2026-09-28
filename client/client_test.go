package client_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/client"
)

// listener accepts and ignores, which is enough: these tests are about what
// Dial does to the socket, not about the protocol.
func listener(t *testing.T) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { lis.Close() })

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()

	return lis.Addr().String()
}

func TestDialRefusesATimeoutTooSmallToEnforce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, listener(t), client.WithDeadPeerTimeout(time.Millisecond))
	if err == nil {
		c.Close()
		t.Fatal("Dial accepted a one-millisecond dead-peer timeout")
	}

	if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
		t.Errorf("code = %s, want %s", got, treevial.CodeInvalid)
	}
	if !strings.Contains(err.Error(), "dead-peer timeout") {
		t.Errorf("error %q does not say what was wrong", err)
	}
}

func TestDialAcceptsAUsableTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, listener(t), client.WithDeadPeerTimeout(90*time.Second))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
}

// The option is opt-in: without it a client dials exactly as it always has.
func TestDialWithoutTheOptionStillWorks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, listener(t))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
}
