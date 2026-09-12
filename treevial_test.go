package treevial_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zonque/treevial"
)

func TestRefForBuildsTheRefFromTheClientID(t *testing.T) {
	got := treevial.RefFor("printer-7")

	if want := "refs/heads/printer-7/config"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestValidateIDAcceptsOrdinaryIDs(t *testing.T) {
	for _, id := range []string{"a", "printer-7", "node_12", "v1.2", "ABC-xyz-0"} {
		if err := treevial.ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q): %v", id, err)
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
		if err := treevial.ValidateID(id); err == nil {
			t.Errorf("ValidateID(%q) accepted an unusable client ID", id)
		}
	}
}

func TestValidateIDRejectsOverlongIDs(t *testing.T) {
	if err := treevial.ValidateID(strings.Repeat("a", 256)); err == nil {
		t.Error("ValidateID accepted an unbounded client ID")
	}
}

func TestErrorCarriesItsCodeAndMessage(t *testing.T) {
	err := &treevial.Error{Code: treevial.CodeAlreadyExists, Message: "client \"printer-7\" is already connected"}

	if got := err.Error(); !strings.Contains(got, "already connected") {
		t.Errorf("Error() = %q, want it to carry the message", got)
	}
	if got := err.Error(); !strings.Contains(got, string(treevial.CodeAlreadyExists)) {
		t.Errorf("Error() = %q, want it to carry the code", got)
	}
}

func TestCodeOfClassifiesProtocolErrors(t *testing.T) {
	err := &treevial.Error{Code: treevial.CodeInvalid, Message: "client ID is empty"}

	if got := treevial.CodeOf(err); got != treevial.CodeInvalid {
		t.Errorf("CodeOf = %q, want %q", got, treevial.CodeInvalid)
	}
}

func TestCodeOfFindsAWrappedProtocolError(t *testing.T) {
	err := fmt.Errorf("subscribe: %w", &treevial.Error{Code: treevial.CodeInternal, Message: "boom"})

	if got := treevial.CodeOf(err); got != treevial.CodeInternal {
		t.Errorf("CodeOf = %q, want %q", got, treevial.CodeInternal)
	}
}

func TestCodeOfReportsOtherErrorsAsUnknown(t *testing.T) {
	if got := treevial.CodeOf(errors.New("connection reset")); got != treevial.CodeUnknown {
		t.Errorf("CodeOf = %q, want %q", got, treevial.CodeUnknown)
	}
	if got := treevial.CodeOf(nil); got != treevial.CodeUnknown {
		t.Errorf("CodeOf(nil) = %q, want %q", got, treevial.CodeUnknown)
	}
}
