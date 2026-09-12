package wire_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/internal/wire"
)

var someHash = plumbing.NewHash("df0e0e1cd7146ab580338deb67e66bd05d42c1e8")

// pair returns the two ends of a connection, so a test can write on one and
// read on the other exactly as client and server do.
func pair(t *testing.T) (client, server *wire.Conn) {
	t.Helper()

	c, s := net.Pipe()

	t.Cleanup(func() {
		c.Close()
		s.Close()
	})

	return wire.NewConn(c), wire.NewConn(s)
}

// writing runs fn on the far end, so a blocking pipe write does not deadlock
// the test.
func writing(t *testing.T, fn func() error) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- fn() }()

	t.Cleanup(func() {
		if err := <-done; err != nil {
			t.Errorf("write side: %v", err)
		}
	})
}

func TestRegisterCarriesTheClientIDAndState(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error { return client.WriteRegister("printer-7", someHash) })

	msg, err := server.ReadClientMessage()
	if err != nil {
		t.Fatalf("ReadClientMessage: %v", err)
	}

	if msg.Kind != wire.Register {
		t.Errorf("kind %v, want Register", msg.Kind)
	}
	if msg.ClientID != "printer-7" {
		t.Errorf("client ID %q, want %q", msg.ClientID, "printer-7")
	}
	if msg.Hash != someHash {
		t.Errorf("synced %s, want %s", msg.Hash, someHash)
	}
}

func TestRegisterCarriesTheZeroHashForAFreshClient(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error { return client.WriteRegister("printer-7", plumbing.ZeroHash) })

	msg, err := server.ReadClientMessage()
	if err != nil {
		t.Fatalf("ReadClientMessage: %v", err)
	}

	if !msg.Hash.IsZero() {
		t.Errorf("synced %s, want the zero hash", msg.Hash)
	}
}

func TestAckCarriesTheHash(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error { return client.WriteAck(someHash) })

	msg, err := server.ReadClientMessage()
	if err != nil {
		t.Fatalf("ReadClientMessage: %v", err)
	}

	if msg.Kind != wire.Ack {
		t.Errorf("kind %v, want Ack", msg.Kind)
	}
	if msg.Hash != someHash {
		t.Errorf("hash %s, want %s", msg.Hash, someHash)
	}
}

func TestUpdateCarriesTheHashAndObjectCount(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error { return server.WriteUpdate(someHash, 16) })

	msg, err := client.ReadServerMessage()
	if err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}

	if msg.Kind != wire.Update {
		t.Errorf("kind %v, want Update", msg.Kind)
	}
	if msg.Hash != someHash {
		t.Errorf("hash %s, want %s", msg.Hash, someHash)
	}
	if msg.ObjectCount != 16 {
		t.Errorf("object count %d, want 16", msg.ObjectCount)
	}
}

func TestPackDataSurvivesTheRoundTrip(t *testing.T) {
	client, server := pair(t)

	// Larger than one pkt-line can hold, so it has to be split and put
	// back together.
	payload := make([]byte, 200*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	writing(t, func() error {
		if err := server.WriteUpdate(someHash, 3); err != nil {
			return err
		}

		w := server.PackWriter()
		if _, err := w.Write(payload); err != nil {
			return err
		}

		return w.Close()
	})

	if _, err := client.ReadServerMessage(); err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}

	got, err := io.ReadAll(client.PackReader())
	if err != nil {
		t.Fatalf("PackReader: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("pack data differs: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestAnUpdateWithNoPackEndsImmediately(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error {
		if err := server.WriteUpdate(someHash, 0); err != nil {
			return err
		}

		return server.PackWriter().Close()
	})

	if _, err := client.ReadServerMessage(); err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}

	got, err := io.ReadAll(client.PackReader())
	if err != nil {
		t.Fatalf("PackReader: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("got %d bytes of pack data, want none", len(got))
	}
}

func TestTheConnectionCarriesOneUpdateAfterAnother(t *testing.T) {
	client, server := pair(t)

	second := plumbing.NewHash("b501ed1768b41ecd5086ec43345aece2a0fb5d1c")

	writing(t, func() error {
		for _, h := range []plumbing.Hash{someHash, second} {
			if err := server.WriteUpdate(h, 1); err != nil {
				return err
			}

			w := server.PackWriter()
			if _, err := w.Write([]byte("pack bytes")); err != nil {
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		}

		return nil
	})

	for _, want := range []plumbing.Hash{someHash, second} {
		msg, err := client.ReadServerMessage()
		if err != nil {
			t.Fatalf("ReadServerMessage: %v", err)
		}
		if msg.Hash != want {
			t.Errorf("hash %s, want %s", msg.Hash, want)
		}

		// The pack has to be drained before the next message can be
		// read, which is the one ordering rule the protocol imposes.
		if _, err := io.ReadAll(client.PackReader()); err != nil {
			t.Fatalf("PackReader: %v", err)
		}
	}
}

func TestAnErrorArrivesWithItsCode(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error {
		return server.WriteError(treevial.Errorf(treevial.CodeAlreadyExists, "client %q is already connected", "printer-7"))
	})

	_, err := client.ReadServerMessage()
	if err == nil {
		t.Fatal("ReadServerMessage did not report the error")
	}

	if got := treevial.CodeOf(err); got != treevial.CodeAlreadyExists {
		t.Errorf("code %q, want %q", got, treevial.CodeAlreadyExists)
	}

	var e *treevial.Error
	if !errors.As(err, &e) || e.Message != `client "printer-7" is already connected` {
		t.Errorf("message %q, want the server's message", err)
	}
}

func TestMalformedLinesAreRejected(t *testing.T) {
	for _, line := range []string{
		"",
		"register",
		"register printer-7",
		"register printer-7 not-a-hash",
		"ack",
		"ack not-a-hash",
		"greetings printer-7",
	} {
		client, server := pair(t)

		writing(t, func() error { return client.WriteLine(line) })

		if _, err := server.ReadClientMessage(); err == nil {
			t.Errorf("line %q was accepted", line)
		}
	}
}

func TestMalformedServerLinesAreRejected(t *testing.T) {
	for _, line := range []string{
		"update",
		"update df0e0e1cd7146ab580338deb67e66bd05d42c1e8",
		"update df0e0e1cd7146ab580338deb67e66bd05d42c1e8 lots",
		"update not-a-hash 3",
		"shrug",
	} {
		client, server := pair(t)

		writing(t, func() error { return server.WriteLine(line) })

		if _, err := client.ReadServerMessage(); err == nil {
			t.Errorf("line %q was accepted", line)
		}
	}
}
