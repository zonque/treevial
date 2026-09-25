package server_test

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
)

type stubProvider struct{}

func (stubProvider) Prepare(string) (*objects.Store, plumbing.Hash, error) {
	return objects.NewStore(), plumbing.ZeroHash, nil
}

func (stubProvider) Release(string) {}

func TestNewRefusesAnUnusableID(t *testing.T) {
	for _, id := range []string{
		"node 3",
		"node\n3",
		strings.Repeat("n", treevial.MaxServerIDLength+1),
	} {
		t.Run(id, func(t *testing.T) {
			srv, err := server.New(stubProvider{}, server.WithID(id))
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
	srv, err := server.New(stubProvider{}, server.WithID("node-3"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := srv.ID(); got != "node-3" {
		t.Errorf("ID() = %q, want %q", got, "node-3")
	}
}

func TestAServerNeedNotNameItself(t *testing.T) {
	srv, err := server.New(stubProvider{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := srv.ID(); got != "" {
		t.Errorf("ID() = %q, want empty", got)
	}
}
