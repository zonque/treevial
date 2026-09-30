package server_test

import (
	"errors"
	"testing"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
)

const someRef = "refs/heads/printer-7/config"

func TestAServerNeedsNoProvider(t *testing.T) {
	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	head := server.Head{Hash: someHash, Sequence: 1}

	if err := srv.Publish(someRef, objects.NewStore(), head); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if got := srv.Head(someRef); got != head {
		t.Errorf("Head() = %+v, want the published %+v", got, head)
	}
}

// The point of publishing: the ref exists because this node holds it, so its
// head moves whether or not anybody is connected.
func TestAPublishedRefMovesWithNobodyConnected(t *testing.T) {
	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Publish(someRef, objects.NewStore(), server.Head{Hash: someHash, Sequence: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	next := server.Head{Hash: otherHash, Sequence: 2}

	if err := srv.SetHead(someRef, next); err != nil {
		t.Errorf("SetHead on a published ref: %v", err)
	}
	if got := srv.Head(someRef); got != next {
		t.Errorf("Head() = %+v, want %+v", got, next)
	}
}

// A published ref moves forward and nowhere else, by the same rule as any
// other: publishing decides the kind, and the compare-and-set does the rest.
func TestAPublishedRefStillOnlyMovesForward(t *testing.T) {
	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	head := server.Head{Hash: someHash, Sequence: 5}

	if err := srv.Publish(someRef, objects.NewStore(), head); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	behind := server.Head{Hash: otherHash, Sequence: 4}

	if err := srv.SetHead(someRef, behind); !errors.Is(err, server.ErrNotAdvancing) {
		t.Errorf("SetHead = %v, want ErrNotAdvancing", err)
	}
	if got := srv.Head(someRef); got != head {
		t.Errorf("head moved to %+v, want it left at %+v", got, head)
	}
}

func TestPublishRefusesARefTheServerAlreadyHolds(t *testing.T) {
	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Publish(someRef, objects.NewStore(), server.Head{Hash: someHash, Sequence: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	err = srv.Publish(someRef, objects.NewStore(), server.Head{Hash: otherHash, Sequence: 2})
	if !errors.Is(err, server.ErrRefHeld) {
		t.Errorf("a second Publish = %v, want ErrRefHeld", err)
	}
}

// A ref may be published unsequenced, exactly as one may be prepared that way.
func TestARefMayBePublishedUnsequenced(t *testing.T) {
	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Publish(someRef, objects.NewStore(), server.Head{Hash: someHash}); err != nil {
		t.Fatalf("publishing an unsequenced ref: %v", err)
	}

	if err := srv.Unpublish(someRef); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
}

// A refused Publish leaves nothing behind, so the ref is free for the next
// attempt rather than half-held by the one that failed.
func TestARefusedPublishLeavesTheRefUnheld(t *testing.T) {
	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Publish(someRef, nil, server.Head{Hash: someHash, Sequence: 1}); err == nil {
		t.Fatal("Publish accepted a nil store")
	}

	if got := srv.Head(someRef); got != (server.Head{}) {
		t.Errorf("Head() = %+v after a refused Publish, want the zero Head", got)
	}

	if err := srv.Publish(someRef, objects.NewStore(), server.Head{Hash: someHash, Sequence: 1}); err != nil {
		t.Errorf("Publish after a refused one: %v", err)
	}
}

func TestPublishRefusesAnUnusableRef(t *testing.T) {
	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Publish("not-a-ref", objects.NewStore(), server.Head{Hash: someHash, Sequence: 1}); err == nil {
		t.Error("Publish accepted a ref name the protocol would refuse")
	}
}

func TestUnpublishRefusesWhatTheServerDoesNotHold(t *testing.T) {
	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Unpublish(someRef); !errors.Is(err, server.ErrUnknownRef) {
		t.Errorf("Unpublish of an unheld ref = %v, want ErrUnknownRef", err)
	}
}

func TestSetHeadRefusesWhatTheServerDoesNotHold(t *testing.T) {
	srv, err := server.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = srv.SetHead(someRef, server.Head{Hash: someHash, Sequence: 1})
	if !errors.Is(err, server.ErrUnknownRef) {
		t.Errorf("SetHead on an unheld ref = %v, want ErrUnknownRef", err)
	}
}

func TestWithProviderRefusesANilOne(t *testing.T) {
	if _, err := server.New(server.WithProvider(nil)); err == nil {
		t.Error("New accepted a nil provider")
	}
}
