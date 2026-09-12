package client_test

import (
	"strings"
	"testing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/client"
)

// TestRefForProducesARefTheServerWillAccept ties the two halves together: the
// convention this package uses has to satisfy the rule the server applies to
// whatever ref it is sent.
func TestRefForProducesARefTheServerWillAccept(t *testing.T) {
	for _, id := range []string{"a", "printer-7", "node_12", "v1.2", "ABC-xyz-0"} {
		if err := treevial.ValidateRef(client.RefFor(id)); err != nil {
			t.Errorf("ValidateRef(RefFor(%q)): %v", id, err)
		}
	}
}

func TestRefForBuildsTheRefFromTheClientID(t *testing.T) {
	got := client.RefFor("printer-7")

	if want := "refs/heads/printer-7/config"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestValidateIDAcceptsOrdinaryIDs(t *testing.T) {
	for _, id := range []string{"a", "printer-7", "node_12", "v1.2", "ABC-xyz-0"} {
		if err := client.ValidateID(id); err != nil {
			t.Errorf("client.ValidateID(%q): %v", id, err)
		}
	}
}

func TestValidateIDRejectsIDsThatWouldEscapeTheRefPath(t *testing.T) {
	// An ID is interpolated into a ref path, so anything that could climb
	// out of it, name a different ref, or produce a ref git refuses has to
	// be turned away.
	for _, id := range []string{
		"",              // no ref to build
		"..",            // parent
		"a/../b",        // climbs out
		"with/slash",    // extra path component
		"with space",    // git forbids
		"tilde~1",       // git revision syntax
		"caret^",        // git revision syntax
		"colon:",        // git refspec syntax
		"question?",     // git forbids
		"star*",         // git forbids
		"open[",         // git forbids
		"back\\slash",   // git forbids
		".hidden",       // component may not start with a dot
		"-dash",         // reads as a flag
		"trailing.lock", // git reserves the .lock suffix
		"dot..dot",      // git forbids
		"new\nline",
	} {
		if err := client.ValidateID(id); err == nil {
			t.Errorf("client.ValidateID(%q) accepted an unusable client ID", id)
		}
	}
}

func TestValidateIDRejectsOverlongIDs(t *testing.T) {
	if err := client.ValidateID(strings.Repeat("a", 256)); err == nil {
		t.Error("ValidateID accepted an unbounded client ID")
	}
}
