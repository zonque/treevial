//go:build !linux

package tcpkeep

import (
	"syscall"
	"time"
)

// SetUserTimeout does nothing here, because TCP_USER_TIMEOUT is Linux's.
//
// The keepalive schedule still applies, so an idle connection to a peer that
// has gone away is still given up on within the budget. What is lost is the
// other case: a peer that vanishes mid-push, with data unacknowledged, waits
// out the kernel's retransmit default instead. macOS has TCP_RXT_CONNDROPTIME
// and Windows TCP_MAXRT; either could be added as another file of this name
// without changing a caller.
func SetUserTimeout(syscall.RawConn, time.Duration) error {
	return nil
}
