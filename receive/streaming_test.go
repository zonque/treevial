package receive_test

import (
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/holoplot/gats/receive"
)

// signaller closes first as soon as any object is handed over, so a test can
// tell whether interpretation happened during the read or only after it.
type signaller struct {
	first chan struct{}
	once  bool
}

func (s *signaller) signal() {
	if !s.once {
		s.once = true
		close(s.first)
	}
}

func (s *signaller) OnPackHeader(uint32) error { return nil }

func (s *signaller) OnBlob(plumbing.Hash, []byte) error {
	s.signal()

	return nil
}

func (s *signaller) OnTree(plumbing.Hash, []object.TreeEntry) error {
	s.signal()

	return nil
}

func (s *signaller) OnPackFooter(plumbing.Hash) error { return nil }

func TestInterpretDecodesObjectsWhileTheStreamIsStillArriving(t *testing.T) {
	_, _, pack := demoPack(t)

	pr, pw := io.Pipe()
	sig := &signaller{first: make(chan struct{})}

	done := make(chan error, 1)
	go func() { done <- receive.Interpret(pr, sig) }()

	// Deliver only the front of the pack, mimicking a single gRPC chunk.
	if _, err := pw.Write(pack[:len(pack)/2]); err != nil {
		t.Fatalf("write first half: %v", err)
	}

	select {
	case <-sig.first:
		// An object was interpreted with half the pack still unsent.
	case err := <-done:
		t.Fatalf("Interpret returned before any object was delivered: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no object delivered while the stream was still open; interpretation is not on the fly")
	}

	if _, err := pw.Write(pack[len(pack)/2:]); err != nil {
		t.Fatalf("write second half: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("Interpret: %v", err)
	}
}
