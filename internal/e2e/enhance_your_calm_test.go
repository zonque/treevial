package e2e

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// TestServerNeverAnswersPingsWithEnhanceYourCalm speaks HTTP/2 to the gats
// server directly and pings it as fast as it can.
//
// gRPC's default enforcement policy allows one ping every five minutes and
// answers anything faster with GOAWAY ENHANCE_YOUR_CALM, closing the
// connection after three strikes. A gats connection has to survive any ping
// rate: the server relies on it being there to push at a moment of its own
// choosing, so no amount of client chatter may be grounds for hanging up.
//
// Raw frames are used rather than a gats client because grpc-go clamps a
// client's ping interval to ten seconds, which would make this test take the
// best part of a minute to reach the strike threshold.
func TestServerNeverAnswersPingsWithEnhanceYourCalm(t *testing.T) {
	h := newHarness(t)

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatalf("write preface: %v", err)
	}

	framer := http2.NewFramer(conn, conn)
	if err := framer.WriteSettings(); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	const pings = 12

	for i := range pings {
		if err := framer.WritePing(false, [8]byte{byte(i)}); err != nil {
			t.Fatalf("write ping %d: %v", i, err)
		}
	}

	acked := 0

	for acked < pings {
		frame, err := framer.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatalf("server closed the connection after %d of %d pings", acked, pings)
			}

			t.Fatalf("read frame after %d acks: %v", acked, err)
		}

		switch f := frame.(type) {
		case *http2.GoAwayFrame:
			if f.ErrCode == http2.ErrCodeEnhanceYourCalm {
				t.Fatalf("server sent GOAWAY ENHANCE_YOUR_CALM (%q) after %d pings", f.DebugData(), acked)
			}

			t.Fatalf("server sent GOAWAY %v after %d pings", f.ErrCode, acked)

		case *http2.SettingsFrame:
			if f.IsAck() {
				continue
			}
			if err := framer.WriteSettingsAck(); err != nil {
				t.Fatalf("write settings ack: %v", err)
			}

		case *http2.PingFrame:
			if f.IsAck() {
				acked++
			}
		}
	}
}
