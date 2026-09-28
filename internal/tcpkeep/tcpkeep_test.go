package tcpkeep_test

import (
	"testing"
	"time"

	"github.com/zonque/treevial/internal/tcpkeep"
)

// The kernel keeps the keepalive schedule in whole seconds, and Go rounds up
// to them before the setsockopt. A schedule with a fractional second in it is
// therefore not the schedule that gets applied, so it is computed in whole
// seconds here.
func TestTheScheduleIsWholeSeconds(t *testing.T) {
	for d := tcpkeep.MinTimeout; d < 20*time.Minute; d += 337 * time.Millisecond {
		s := tcpkeep.For(d)

		if s.KeepAlive.Idle%time.Second != 0 {
			t.Fatalf("d=%v: Idle=%v is not whole seconds", d, s.KeepAlive.Idle)
		}
		if s.KeepAlive.Interval%time.Second != 0 {
			t.Fatalf("d=%v: Interval=%v is not whole seconds", d, s.KeepAlive.Interval)
		}
		if s.KeepAlive.Interval < time.Second {
			t.Fatalf("d=%v: Interval=%v is below the one-second floor", d, s.KeepAlive.Interval)
		}
	}
}

// The whole justification for one knob: the kernel recomputes the probe count
// from the user timeout, and must land on the count we configured. Computed
// the way the kernel computes it — in its own units, from values that survive
// the setsockopt — rather than in nanoseconds it never sees.
func TestTheKernelsProbeCountAgreesWithOurs(t *testing.T) {
	for d := tcpkeep.MinTimeout; d < 20*time.Minute; d += 337 * time.Millisecond {
		s := tcpkeep.For(d)

		var (
			idle     = int(s.KeepAlive.Idle.Seconds())
			interval = int(s.KeepAlive.Interval.Seconds())
			timeout  = int(s.UserTimeout.Seconds())
		)

		if got := (timeout - idle) / interval; got != s.KeepAlive.Count {
			t.Fatalf("d=%v: idle=%ds interval=%ds user timeout=%ds -> kernel computes %d probes, we configured %d",
				d, idle, interval, timeout, got, s.KeepAlive.Count)
		}
	}
}

// The budget is what a caller asked to wait at most; rounding to whole seconds
// may cost a little more, never less.
func TestTheScheduleNeverGivesUpSoonerThanAsked(t *testing.T) {
	for d := tcpkeep.MinTimeout; d < 20*time.Minute; d += 337 * time.Millisecond {
		s := tcpkeep.For(d)

		if s.UserTimeout < d {
			t.Fatalf("d=%v: gives up after %v, sooner than asked", d, s.UserTimeout)
		}
		if s.UserTimeout > d+4*time.Second+s.KeepAlive.Interval {
			t.Fatalf("d=%v: gives up after %v, far later than asked", d, s.UserTimeout)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
		ok   bool
	}{
		{"zero", 0, false},
		{"negative", -time.Second, false},
		{"a millisecond", time.Millisecond, false},
		{"just under the floor", tcpkeep.MinTimeout - time.Nanosecond, false},
		{"the floor", tcpkeep.MinTimeout, true},
		{"a normal budget", 90 * time.Second, true},
		{"an hour", time.Hour, true},
		// The user timeout goes to the kernel as int32 milliseconds, so a
		// budget that cannot be expressed in them is refused rather than
		// silently wrapping into a much shorter one.
		{"a year", 365 * 24 * time.Hour, false},
		{"just over what milliseconds hold", tcpkeep.MaxTimeout + time.Hour, false},
		{"the ceiling", tcpkeep.MaxTimeout, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tcpkeep.Validate(tc.d)

			if tc.ok && err != nil {
				t.Errorf("Validate(%v) = %v, want nil", tc.d, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("Validate(%v) = nil, want an error", tc.d)
			}
		})
	}
}

// Whatever Validate accepts must produce a user timeout the sockopt can hold.
func TestEveryAcceptedBudgetFitsTheSockopt(t *testing.T) {
	for _, d := range []time.Duration{
		tcpkeep.MinTimeout,
		time.Minute,
		24 * time.Hour,
		tcpkeep.MaxTimeout,
	} {
		if tcpkeep.Validate(d) != nil {
			t.Fatalf("Validate(%v) refused a budget the table calls usable", d)
		}

		if ms := tcpkeep.For(d).UserTimeout.Milliseconds(); ms > 1<<31-1 {
			t.Errorf("d=%v: user timeout %d ms overflows the sockopt", d, ms)
		}
	}
}
