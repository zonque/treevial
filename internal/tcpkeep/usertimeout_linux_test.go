//go:build linux

package tcpkeep_test

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zonque/treevial/internal/tcpkeep"
)

// Reading the options back is the point: it proves the kernel was told, rather
// than proving the code ran without erroring.
func TestSetUserTimeoutReachesTheKernel(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}

	if err := tcpkeep.SetUserTimeout(raw, 90*time.Second); err != nil {
		t.Fatalf("SetUserTimeout: %v", err)
	}

	var (
		got     int
		readErr error
	)

	if err := raw.Control(func(fd uintptr) {
		got, readErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); err != nil {
		t.Fatalf("Control: %v", err)
	}
	if readErr != nil {
		t.Fatalf("GetsockoptInt: %v", readErr)
	}

	if want := 90_000; got != want {
		t.Errorf("TCP_USER_TIMEOUT = %d ms, want %d", got, want)
	}
}

// The claim one knob rests on, measured where it matters: after both options
// have been through the kernel, its own recomputation of the probe count must
// land on the count we configured. Nanosecond arithmetic cannot show this —
// Go rounds the schedule up to whole seconds on the way in, so the values the
// kernel holds are not the values we computed with.
func TestTheKernelAgreesOnTheProbeCount(t *testing.T) {
	// 80s divides evenly and 90s does not, which is the whole point: an
	// earlier version of this passed on 80 and was wrong on 90.
	for _, d := range []time.Duration{
		tcpkeep.MinTimeout,
		45 * time.Second,
		80 * time.Second,
		90 * time.Second,
		7 * time.Minute,
	} {
		t.Run(d.String(), func(t *testing.T) {
			live := tcpkeep.For(d)

			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer lis.Close()

			conn, err := net.Dial("tcp", lis.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			tcp := conn.(*net.TCPConn)

			if err := tcp.SetKeepAliveConfig(live.KeepAlive); err != nil {
				t.Fatalf("SetKeepAliveConfig: %v", err)
			}

			raw, err := tcp.SyscallConn()
			if err != nil {
				t.Fatalf("SyscallConn: %v", err)
			}

			if err := tcpkeep.SetUserTimeout(raw, live.UserTimeout); err != nil {
				t.Fatalf("SetUserTimeout: %v", err)
			}

			read := func(opt int) int {
				t.Helper()

				var (
					v    int
					rerr error
				)

				if err := raw.Control(func(fd uintptr) {
					v, rerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, opt)
				}); err != nil {
					t.Fatalf("Control: %v", err)
				}
				if rerr != nil {
					t.Fatalf("GetsockoptInt(%d): %v", opt, rerr)
				}

				return v
			}

			var (
				idle     = read(unix.TCP_KEEPIDLE)
				interval = read(unix.TCP_KEEPINTVL)
				count    = read(unix.TCP_KEEPCNT)
				timeout  = read(unix.TCP_USER_TIMEOUT)
			)

			// What we asked for is what it holds.
			if want := int(live.KeepAlive.Idle.Seconds()); idle != want {
				t.Errorf("TCP_KEEPIDLE = %d s, want %d", idle, want)
			}
			if want := int(live.KeepAlive.Interval.Seconds()); interval != want {
				t.Errorf("TCP_KEEPINTVL = %d s, want %d", interval, want)
			}
			if want := int(live.UserTimeout.Milliseconds()); timeout != want {
				t.Errorf("TCP_USER_TIMEOUT = %d ms, want %d", timeout, want)
			}

			// And its own arithmetic over those values agrees with it.
			if got := (timeout/1000 - idle) / interval; got != count {
				t.Errorf("kernel computes %d probes from idle=%ds interval=%ds timeout=%dms, but holds a count of %d",
					got, idle, interval, timeout, count)
			}
		})
	}
}
