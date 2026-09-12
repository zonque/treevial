package main

import (
	"testing"

	"github.com/zonque/treevial"
)

func TestRefForBuildsAHeadFromTheClientID(t *testing.T) {
	if got, want := refFor("printer-7"), "refs/heads/printer-7/config"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRefForProducesARefTheServerWillAccept ties this program's convention to
// the rule the server applies to any ref it is sent.
func TestRefForProducesARefTheServerWillAccept(t *testing.T) {
	for _, id := range []string{"a", "printer-7", "node_12", "v1.2", "ABC-xyz-0"} {
		if err := treevial.ValidateRef(refFor(id)); err != nil {
			t.Errorf("ValidateRef(refFor(%q)): %v", id, err)
		}
	}
}

func TestRefForTurnsAnUnusableIDIntoARefTheServerRefuses(t *testing.T) {
	// The convention needs no validation of its own: an ID that cannot make
	// a sane ref makes one the shared rule rejects.
	for _, id := range []string{"", "..", ".hidden", "with space", "tilde~1", "x.lock"} {
		if err := treevial.ValidateRef(refFor(id)); err == nil {
			t.Errorf("refFor(%q) = %q, which the server would accept", id, refFor(id))
		}
	}
}
