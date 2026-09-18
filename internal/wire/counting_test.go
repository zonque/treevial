package wire_test

import (
	"crypto/rand"
	"io"
	"testing"

	"github.com/zonque/treevial/internal/wire"
)

// The sizes below are the pkt-line sizes PROTOCOL.md documents: four hex
// digits of length, then the payload. They are written out here rather than
// computed, so a test fails if the framing changes rather than following it.
const (
	// 0052register refs/heads/printer-7/config <40 hex>\n
	registerSize = 0x52
	// 0037update <40 hex> 17\n
	updateSize = 0x37
	// The flush-pkt that ends an update: 0000.
	flushSize = 4
	// A pkt-line's own four-digit header.
	headerSize = 4
)

func TestConnCountsWhatAMessageCostOnTheWire(t *testing.T) {
	client, server := pair(t)

	done := make(chan error, 1)
	go func() { done <- client.WriteRegister("refs/heads/printer-7/config", someHash) }()

	if _, err := server.ReadClientMessage(); err != nil {
		t.Fatalf("ReadClientMessage: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteRegister: %v", err)
	}

	if got := client.BytesWritten(); got != registerSize {
		t.Errorf("sender counted %d bytes, want %d", got, registerSize)
	}
	if got := server.BytesRead(); got != registerSize {
		t.Errorf("receiver counted %d bytes, want %d", got, registerSize)
	}

	// Counting is per direction: nothing came back the other way.
	if got := client.BytesRead(); got != 0 {
		t.Errorf("sender counted %d bytes read, want 0", got)
	}
	if got := server.BytesWritten(); got != 0 {
		t.Errorf("receiver counted %d bytes written, want 0", got)
	}
}

func TestConnCountsAPacksFramingAndItsClosingFlush(t *testing.T) {
	client, server := pair(t)

	pack := make([]byte, 1024)
	if _, err := rand.Read(pack); err != nil {
		t.Fatalf("rand: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- writeUpdate(server, pack) }()

	if _, err := client.ReadServerMessage(); err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}

	if _, err := io.Copy(io.Discard, client.PackReader()); err != nil {
		t.Fatalf("draining the pack: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("writing the update: %v", err)
	}

	// The update line, the one pkt-line the pack fits in, and the flush-pkt
	// that ends it.
	want := int64(updateSize + headerSize + len(pack) + flushSize)

	if got := server.BytesWritten(); got != want {
		t.Errorf("sender counted %d bytes, want %d", got, want)
	}
	if got := client.BytesRead(); got != want {
		t.Errorf("receiver counted %d bytes, want %d", got, want)
	}
}

func TestConnCountsAPackSplitOverSeveralLines(t *testing.T) {
	client, server := pair(t)

	// Two full chunks and a remainder, so the framing overhead is three
	// headers rather than one.
	pack := make([]byte, 2*wire.ChunkSize+7)
	if _, err := rand.Read(pack); err != nil {
		t.Fatalf("rand: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- writeUpdate(server, pack) }()

	if _, err := client.ReadServerMessage(); err != nil {
		t.Fatalf("ReadServerMessage: %v", err)
	}
	if _, err := io.Copy(io.Discard, client.PackReader()); err != nil {
		t.Fatalf("draining the pack: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("writing the update: %v", err)
	}

	want := int64(updateSize + 3*headerSize + len(pack) + flushSize)

	if got := client.BytesRead(); got != want {
		t.Errorf("receiver counted %d bytes, want %d", got, want)
	}
	if got := server.BytesWritten(); got != want {
		t.Errorf("sender counted %d bytes, want %d", got, want)
	}
}

func TestConnCountersAccumulateOverTheWholeConnection(t *testing.T) {
	client, server := pair(t)

	pack := make([]byte, 64)
	if _, err := rand.Read(pack); err != nil {
		t.Fatalf("rand: %v", err)
	}

	var running int64

	for push := 1; push <= 3; push++ {
		done := make(chan error, 1)
		go func() { done <- writeUpdate(server, pack) }()

		if _, err := client.ReadServerMessage(); err != nil {
			t.Fatalf("push %d: ReadServerMessage: %v", push, err)
		}
		if _, err := io.Copy(io.Discard, client.PackReader()); err != nil {
			t.Fatalf("push %d: draining the pack: %v", push, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("push %d: writing the update: %v", push, err)
		}

		running += int64(updateSize + headerSize + len(pack) + flushSize)

		if got := client.BytesRead(); got != running {
			t.Errorf("after push %d the receiver counted %d bytes, want %d", push, got, running)
		}
	}
}

// writeUpdate announces an update and sends pack as its payload, the way the
// server does.
func writeUpdate(conn *wire.Conn, pack []byte) error {
	if err := conn.WriteUpdate(someHash, 17); err != nil {
		return err
	}

	w := conn.PackWriter()

	if _, err := w.Write(pack); err != nil {
		return err
	}

	return w.Close()
}
