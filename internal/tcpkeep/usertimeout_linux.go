//go:build linux

package tcpkeep

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// SetUserTimeout bounds how long transmitted data may go unacknowledged before
// the connection is given up on.
//
// This is the half keepalive cannot do: probes are only sent on an idle
// connection, so a peer that vanishes mid-push is invisible to them. Without
// this, such a connection waits out the kernel's retransmit default instead.
func SetUserTimeout(c syscall.RawConn, d time.Duration) error {
	var opErr error

	if err := c.Control(func(fd uintptr) {
		opErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT,
			int(d.Milliseconds()))
	}); err != nil {
		return err
	}

	return opErr
}
