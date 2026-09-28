// Package tcpkeep turns one budget — how long a peer may be unresponsive
// before its connection is given up on — into the socket settings that
// enforce it.
//
// Both halves of treevial need this and neither imports the other, which is
// why it lives here rather than in client or server.
package tcpkeep

import (
	"fmt"
	"math"
	"net"
	"time"
)

// probes is how many keepalive probes go unanswered before the connection is
// given up on. Half the budget is spent waiting and the other half probing,
// so four is what divides the second half into useful pieces.
const probes = 4

// MinTimeout is the smallest budget that can be expressed.
//
// The kernel keeps the keepalive schedule in whole seconds, and the probe
// interval is an eighth of the budget, so anything under eight seconds cannot
// have its interval expressed at all — it would be rounded up to a second and
// the schedule would overshoot what was asked for rather than honour it.
const MinTimeout = probes * 2 * time.Second

// maxUserTimeout is what TCP_USER_TIMEOUT can hold: it is set as int32
// milliseconds, which runs out a little under twenty-five days.
const maxUserTimeout = time.Duration(math.MaxInt32) * time.Millisecond

// MaxTimeout is the largest budget that can be expressed. It sits below what
// the sockopt itself holds because the timeout is derived from a schedule
// whose parts are each rounded up to a whole second, and that rounding can
// add as much as five seconds to it.
const MaxTimeout = maxUserTimeout - (probes+1)*time.Second

// Settings are the socket options that enforce one budget. They are returned
// together because they have to agree: see [For].
type Settings struct {
	KeepAlive net.KeepAliveConfig
	// UserTimeout is what TCP_USER_TIMEOUT is set to where it exists. It
	// is derived from KeepAlive rather than from the budget directly, so
	// that the kernel's own arithmetic lands on KeepAlive.Count.
	UserTimeout time.Duration
}

// Validate reports whether d is a budget that can be enforced.
func Validate(d time.Duration) error {
	switch {
	case d < MinTimeout:
		return fmt.Errorf("a dead-peer timeout of %v is below the %v minimum", d, MinTimeout)
	case d > MaxTimeout:
		return fmt.Errorf("a dead-peer timeout of %v is above the %v maximum", d, MaxTimeout)
	}

	return nil
}

// For turns a budget into the settings that enforce it: wait half of it, then
// probe four times across the other half.
//
// Everything here is whole seconds, because that is the unit the kernel keeps
// a keepalive schedule in — Go rounds these up before the setsockopt, so a
// schedule with a fraction of a second in it is not the schedule that gets
// applied. Computing in seconds is what makes what is asked for and what is
// set the same thing.
//
// The user timeout is then derived from that schedule rather than from the
// budget. It matters because where TCP_USER_TIMEOUT is set, Linux overrides
// the configured probe count with its own, computed as
// (user timeout - idle) / interval. Deriving the timeout as idle + count *
// interval makes that arithmetic land on exactly the count configured here,
// so the two mechanisms agree and a platform that has the user timeout
// reaches the same verdict as one that does not.
//
// Rounding up can cost a little more than the budget asked for and never
// less: a connection is given up on no sooner than d.
func For(d time.Duration) Settings {
	idle := roundUpSeconds(d / 2)

	interval := max(roundUpSeconds(d/(probes*2)), time.Second)

	return Settings{
		KeepAlive: net.KeepAliveConfig{
			Enable:   true,
			Idle:     idle,
			Interval: interval,
			Count:    probes,
		},
		UserTimeout: idle + probes*interval,
	}
}

// roundUpSeconds rounds a duration up to a whole second, which is what the
// kernel would do to it anyway, and never down to nothing.
func roundUpSeconds(d time.Duration) time.Duration {
	if d <= time.Second {
		return time.Second
	}

	if r := d % time.Second; r != 0 {
		d += time.Second - r
	}

	return d
}
