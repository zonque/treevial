package wire_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"math"
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

func TestRegisterCarriesTheRefAndState(t *testing.T) {
	client, server := pair(t)

	ref := "refs/heads/printer-7/config"

	writing(t, func() error { return client.WriteRegister(ref, someHash, "printer-7") })

	msg, err := server.ReadClientMessage()
	if err != nil {
		t.Fatalf("ReadClientMessage: %v", err)
	}

	if msg.Type != wire.Register {
		t.Errorf("type %v, want Register", msg.Type)
	}
	if msg.Ref != ref {
		t.Errorf("ref %q, want %q", msg.Ref, ref)
	}
	if msg.Hash != someHash {
		t.Errorf("synced %s, want %s", msg.Hash, someHash)
	}
}

func TestRegisterCarriesTheNameTheClientGaveItself(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error {
		return client.WriteRegister("refs/heads/printer-7/config", someHash, "press-hall-a-7")
	})

	msg, err := server.ReadClientMessage()
	if err != nil {
		t.Fatalf("ReadClientMessage: %v", err)
	}

	if msg.ClientID != "press-hall-a-7" {
		t.Errorf("client ID %q, want %q", msg.ClientID, "press-hall-a-7")
	}
}

func TestRegisterWithoutANameCarriesNone(t *testing.T) {
	client, server := pair(t)

	// A client that does not name itself sends the same line it always
	// did, so the field is genuinely optional rather than an empty one.
	done := make(chan error, 1)
	go func() {
		done <- client.WriteRegister("refs/heads/printer-7/config", someHash, "")
	}()

	msg, err := server.ReadClientMessage()
	if err != nil {
		t.Fatalf("ReadClientMessage: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteRegister: %v", err)
	}

	if msg.ClientID != "" {
		t.Errorf("client ID %q, want none", msg.ClientID)
	}
	// The registration PROTOCOL.md documents, to the byte.
	if got := client.BytesWritten(); got != 0x52 {
		t.Errorf("an unnamed registration took %d bytes, want the %d it always took", got, 0x52)
	}
}

func TestRegisterCarriesTheRefVerbatim(t *testing.T) {
	client, server := pair(t)

	// The server reads whatever the client asked for; deciding whether it
	// is acceptable is a separate job from carrying it.
	ref := "refs/devices/hall-a/row-3/seat-9"

	writing(t, func() error { return client.WriteRegister(ref, plumbing.ZeroHash, "") })

	msg, err := server.ReadClientMessage()
	if err != nil {
		t.Fatalf("ReadClientMessage: %v", err)
	}

	if msg.Ref != ref {
		t.Errorf("ref %q, want %q", msg.Ref, ref)
	}
}

func TestRegisterCarriesTheZeroHashForAFreshClient(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error { return client.WriteRegister("refs/heads/printer-7/config", plumbing.ZeroHash, "") })

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

	if msg.Type != wire.Ack {
		t.Errorf("type %v, want Ack", msg.Type)
	}
	if msg.Hash != someHash {
		t.Errorf("hash %s, want %s", msg.Hash, someHash)
	}
}

func TestUpdateCarriesTheHashAndObjectCount(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error { return server.WriteUpdate(someHash, 16, 0) })

	msg, err := client.ReadServerMessage()
	if err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
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
		if err := server.WriteUpdate(someHash, 3, 0); err != nil {
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
		if err := server.WriteUpdate(someHash, 0, 0); err != nil {
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
			if err := server.WriteUpdate(h, 1, 0); err != nil {
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
		"register refs/heads/printer-7/config",
		"register refs/heads/printer-7/config not-a-hash",
		"register refs/heads/printer-7/config df0e0e1cd7146ab580338deb67e66bd05d42c1e8 printer-7 extra",
		"ack",
		"ack not-a-hash",
		"greetings refs/heads/printer-7/config",
	} {
		client, server := pair(t)

		writing(t, func() error { return client.WriteLine(line) })

		_, err := server.ReadClientMessage()
		if err == nil {
			t.Errorf("line %q was accepted", line)

			continue
		}
		// Classified, so the server can tell the client why rather than
		// just hanging up on it.
		if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
			t.Errorf("line %q: code %q, want %q", line, got, treevial.CodeInvalid)
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

		_, err := client.ReadServerMessage()
		if err == nil {
			t.Errorf("line %q was accepted", line)

			continue
		}
		if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
			t.Errorf("line %q: code %q, want %q", line, got, treevial.CodeInvalid)
		}
	}
}

func TestServerLineRoundTrips(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error { return server.WriteServerID("node-3") })

	msg, err := client.ReadServerMessage()
	if err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}

	if msg.Type != wire.Announce {
		t.Errorf("type = %v, want Announce", msg.Type)
	}
	if msg.ServerID != "node-3" {
		t.Errorf("ServerID = %q, want %q", msg.ServerID, "node-3")
	}
}

func TestAnUpdateCarriesItsOptionalSequence(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  uint64
	}{
		{"unsequenced", 0},
		{"sequenced", 98},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := pair(t)

			writing(t, func() error { return server.WriteUpdate(someHash, 17, tc.seq) })

			msg, err := client.ReadServerMessage()
			if err != nil {
				t.Fatalf("ReadServerMessage: %v", err)
			}

			if msg.Type != wire.Update {
				t.Errorf("type = %v, want Update", msg.Type)
			}
			if msg.Hash != someHash {
				t.Errorf("Hash = %s, want %s", msg.Hash, someHash)
			}
			if msg.ObjectCount != 17 {
				t.Errorf("ObjectCount = %d, want 17", msg.ObjectCount)
			}
			if msg.Sequence != tc.seq {
				t.Errorf("Sequence = %d, want %d", msg.Sequence, tc.seq)
			}
		})
	}
}

func TestTheLargestSequenceSurvivesTheRoundTrip(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error { return server.WriteUpdate(someHash, 4, math.MaxUint64) })

	msg, err := client.ReadServerMessage()
	if err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}
	if msg.Sequence != math.MaxUint64 {
		t.Errorf("Sequence = %d, want %d", msg.Sequence, uint64(math.MaxUint64))
	}
}

func TestMalformedUpdateTrailersAreRefused(t *testing.T) {
	for _, line := range []string{
		"update " + someHash.String() + " 4 seq=98 seq=99",            // a repeat
		"update " + someHash.String() + " 4 what=98",                  // an unknown key
		"update " + someHash.String() + " 4 node-5",                   // a value without a key
		"update " + someHash.String() + " 4 seq",                      // no value at all
		"update " + someHash.String() + " 4 seq=",                     // an empty value
		"update " + someHash.String() + " 4 seq=0",                    // absent is how unsequenced is said
		"update " + someHash.String() + " 4 seq=+98",                  // not canonical decimal
		"update " + someHash.String() + " 4 seq=-1",                   // not a uint64
		"update " + someHash.String() + " 4 seq=0x5",                  // not decimal
		"update " + someHash.String() + " 4 seq=9_8",                  // not decimal
		"update " + someHash.String() + " 4 seq=18446744073709551616", // past the uint64 ceiling
	} {
		t.Run(line, func(t *testing.T) {
			client, server := pair(t)

			writing(t, func() error { return server.WriteLine(line) })

			if _, err := client.ReadServerMessage(); err == nil {
				t.Errorf("ReadServerMessage accepted %q", line)
			}
		})
	}
}

// A leading zero is accepted, exactly as the object count's own Atoi accepts
// one. Pinned so the leniency is a decision rather than an accident.
func TestASequenceWithALeadingZeroIsAccepted(t *testing.T) {
	client, server := pair(t)

	writing(t, func() error {
		return server.WriteLine("update " + someHash.String() + " 4 seq=098")
	})

	msg, err := client.ReadServerMessage()
	if err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}
	if msg.Sequence != 98 {
		t.Errorf("Sequence = %d, want 98", msg.Sequence)
	}
}

func TestMalformedServerAndUpdateLinesAreRefused(t *testing.T) {
	for _, line := range []string{
		"server",                              // no id
		"server ",                             // an empty id is not a name
		"server node-3 extra",                 // one field only
		"update " + someHash.String(),         // no count
		"update " + someHash.String() + " 4 ", // empty trailing field
	} {
		t.Run(line, func(t *testing.T) {
			client, server := pair(t)

			writing(t, func() error { return server.WriteLine(line) })

			if _, err := client.ReadServerMessage(); err == nil {
				t.Errorf("ReadServerMessage accepted %q", line)
			}
		})
	}
}

// TestPackWriterReportsEverythingItTook is the io.Writer contract: a writer
// that takes the whole slice has to say so, or anything writing through it —
// io.Copy, the pack encoder — reads a short count as a failure.
func TestPackWriterReportsEverythingItTook(t *testing.T) {
	for _, size := range []int{1, wire.ChunkSize - 1, wire.ChunkSize, wire.ChunkSize + 1, 3*wire.ChunkSize + 7} {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		// Drained while the write happens, since a pipe blocks.
		go io.Copy(io.Discard, client)

		w := wire.NewConn(server).PackWriter()

		n, err := w.Write(make([]byte, size))
		if err != nil {
			t.Fatalf("size %d: Write: %v", size, err)
		}
		if n != size {
			t.Errorf("size %d: Write returned %d", size, n)
		}
	}
}
