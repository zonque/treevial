package server_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
)

var (
	someHash  = plumbing.NewHash("35ae729ecbb6c621dc5bc6ac2d8efec6f83c2805")
	otherHash = plumbing.NewHash("30e5ce8082820717b3fb5fec3e962c1d62103e14")
)

type stubProvider struct{}

func (stubProvider) Prepare(string) (*objects.Store, server.Head, error) {
	return objects.NewStore(), server.Head{}, nil
}

func (stubProvider) Release(string) {}

func TestHeadIsUnknownForARefNobodyFollows(t *testing.T) {
	srv, err := server.New(server.WithProvider(stubProvider{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := srv.Head("refs/heads/nobody/config"); got != (server.Head{}) {
		t.Errorf("Head() = %+v, want the zero Head", got)
	}
}

// A node applies entries for every ref it replicates while it may hold only
// some of them, so this is ordinary traffic and has to be testable without
// matching on the text of an error.
func TestSetHeadReportsARefTheServerDoesNotHold(t *testing.T) {
	srv, err := server.New(server.WithProvider(stubProvider{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = srv.SetHead("refs/heads/nobody/config", server.Head{Hash: someHash, Sequence: 1})
	if !errors.Is(err, server.ErrUnknownRef) {
		t.Errorf("SetHead = %v, want ErrUnknownRef", err)
	}
}

func TestNewRefusesAnUnusableID(t *testing.T) {
	for _, id := range []string{
		"node 3",
		"node\n3",
		strings.Repeat("n", treevial.MaxServerIDLength+1),
	} {
		t.Run(id, func(t *testing.T) {
			srv, err := server.New(server.WithProvider(stubProvider{}), server.WithID(id))
			if err == nil {
				t.Fatalf("New accepted the ID %q", id)
			}
			if srv != nil {
				t.Error("New returned a server alongside an error")
			}
			if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
				t.Errorf("code = %s, want %s", got, treevial.CodeInvalid)
			}
		})
	}
}

func TestAServerReportsTheIDItWasGiven(t *testing.T) {
	srv, err := server.New(server.WithProvider(stubProvider{}), server.WithID("node-3"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := srv.ID(); got != "node-3" {
		t.Errorf("ID() = %q, want %q", got, "node-3")
	}
}

func TestAServerNeedNotNameItself(t *testing.T) {
	srv, err := server.New(server.WithProvider(stubProvider{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := srv.ID(); got != "" {
		t.Errorf("ID() = %q, want empty", got)
	}
}

func TestNewRefusesADeadPeerTimeoutTooSmallToEnforce(t *testing.T) {
	srv, err := server.New(server.WithProvider(stubProvider{}), server.WithDeadPeerTimeout(time.Millisecond))
	if err == nil {
		t.Fatal("New accepted a one-millisecond dead-peer timeout")
	}
	if srv != nil {
		t.Error("New returned a server alongside an error")
	}
	if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
		t.Errorf("code = %s, want %s", got, treevial.CodeInvalid)
	}
}

func TestNewAcceptsAUsableDeadPeerTimeout(t *testing.T) {
	if _, err := server.New(server.WithProvider(stubProvider{}), server.WithDeadPeerTimeout(90*time.Second)); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// Zero means unset, not "a timeout of nothing", so it must not be refused the
// way a too-small one is — and it must mean the same on both sides, since a
// caller driving these from configuration will have one zero default for both.
func TestAServerNeedNoDeadPeerTimeout(t *testing.T) {
	if _, err := server.New(server.WithProvider(stubProvider{})); err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := server.New(server.WithProvider(stubProvider{}), server.WithDeadPeerTimeout(0)); err != nil {
		t.Errorf("WithDeadPeerTimeout(0) = %v, want it read as unset, the way the client reads it", err)
	}
}
