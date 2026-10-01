//go:build linux

package client

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zonque/treevial/internal/tcpkeep"
)

// Without the option a connection must keep the plain 30-second keepalive it
// has always had. Asserting Dial merely succeeds would not catch the default
// branch being dropped.
func TestADialWithNoOptionKeepsTheOldKeepalive(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { conn.Close() })
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Dial(ctx, lis.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	raw, err := c.conn.Raw()
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}

	var (
		idle    int
		readErr error
	)

	if err := raw.Control(func(fd uintptr) {
		idle, readErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_KEEPIDLE)
	}); err != nil {
		t.Fatalf("Control: %v", err)
	}
	if readErr != nil {
		t.Fatalf("GetsockoptInt: %v", readErr)
	}

	if want := int(tcpkeep.DefaultPeriod.Seconds()); idle != want {
		t.Errorf("TCP_KEEPIDLE = %d s, want the unchanged %d", idle, want)
	}
}
