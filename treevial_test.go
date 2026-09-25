package treevial_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zonque/treevial"
)

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
	err := &treevial.Error{Code: treevial.CodeInvalid, Message: "ref \"heads/main\" does not start with refs/"}

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

func TestValidateRefAcceptsAnyWellFormedRef(t *testing.T) {
	// A server's namespace is its own business, so any well-formed ref is
	// acceptable; nothing about one shape is privileged.
	for _, ref := range []string{
		"refs/heads/main",
		"refs/devices/hall-a/row-3/seat-9",
		"refs/tags/v1.2.3",
	} {
		if err := treevial.ValidateRef(ref); err != nil {
			t.Errorf("ValidateRef(%q): %v", ref, err)
		}
	}
}

func TestValidateRefRejectsWhatGitWouldRefuse(t *testing.T) {
	// The ref arrives from the client and is taken verbatim, so this is the
	// boundary that keeps a malformed one out of whatever the server keys
	// on it.
	for _, ref := range []string{
		"",                         // nothing to serve
		"heads/main",               // not a ref
		"refs",                     // no name under refs/
		"refs/",                    // empty name
		"refs/heads/",              // trailing slash
		"refs/heads//main",         // empty component
		"refs/heads/../../etc",     // climbs out
		"refs/heads/.hidden",       // component starting with a dot
		"refs/heads/with space",    // git forbids
		"refs/heads/tilde~1",       // git revision syntax
		"refs/heads/caret^",        // git revision syntax
		"refs/heads/colon:",        // git refspec syntax
		"refs/heads/question?",     // git forbids
		"refs/heads/star*",         // git forbids
		"refs/heads/open[",         // git forbids
		"refs/heads/back\\slash",   // git forbids
		"refs/heads/main.lock",     // git reserves the suffix
		"refs/heads/x.lock/config", // reserved on any component, not just the last
		"refs/heads/at@{1}",        // git revision syntax
		"refs/heads/new\nline",     // control character
		"refs/heads/tab\there",     // control character
	} {
		if err := treevial.ValidateRef(ref); err == nil {
			t.Errorf("ValidateRef(%q) accepted an unusable ref", ref)
		}
	}
}

func TestValidateRefRejectsOverlongRefs(t *testing.T) {
	if err := treevial.ValidateRef("refs/heads/" + strings.Repeat("a", treevial.MaxRefLength)); err == nil {
		t.Error("ValidateRef accepted an unbounded ref")
	}
}

func TestValidateClientIDAcceptsALabelAClientMightPick(t *testing.T) {
	for _, id := range []string{
		"",
		"printer-7",
		"hall-a/row-3/seat-9",
		"11111111-2222-3333-4444-555555555555",
		"Drucker_Halle-A.7",
		"ünïcödé-7",
	} {
		if err := treevial.ValidateClientID(id); err != nil {
			t.Errorf("ValidateClientID(%q) = %v, want it accepted", id, err)
		}
	}
}

func TestValidateClientIDRejectsWhatWouldNotSurviveTheLine(t *testing.T) {
	for _, id := range []string{
		"printer 7",
		"printer\n7",
		"printer\t7",
		"printer\x007",
		"printer\x7f",
	} {
		if err := treevial.ValidateClientID(id); err == nil {
			t.Errorf("ValidateClientID(%q) accepted it, want an error", id)
		}
	}
}

func TestValidateClientIDRejectsOverlongIDs(t *testing.T) {
	id := strings.Repeat("a", treevial.MaxClientIDLength+1)

	if err := treevial.ValidateClientID(id); err == nil {
		t.Errorf("ValidateClientID accepted %d bytes, want the %d-byte limit enforced",
			len(id), treevial.MaxClientIDLength)
	}
}

func TestValidateServerID(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		ok   bool
	}{
		{"empty is allowed", "", true},
		{"a plain name", "node-3", true},
		{"at the limit", strings.Repeat("n", treevial.MaxServerIDLength), true},
		{"one byte over", strings.Repeat("n", treevial.MaxServerIDLength+1), false},
		{"a space", "node 3", false},
		{"a tab", "node\t3", false},
		{"a newline", "node\n3", false},
		{"delete", "node\x7f3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := treevial.ValidateServerID(tc.id)
			if tc.ok && err != nil {
				t.Errorf("ValidateServerID(%q) = %v, want nil", tc.id, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("ValidateServerID(%q) = nil, want an error", tc.id)
			}
		})
	}
}

// The two rules are one implementation, so a server ID is refused for exactly
// the reasons a client ID is.
func TestServerAndClientIDsShareOneRule(t *testing.T) {
	for _, id := range []string{"", "node-3", "node 3", "node\n3", strings.Repeat("n", 129)} {
		server := treevial.ValidateServerID(id) == nil
		client := treevial.ValidateClientID(id) == nil

		if server != client {
			t.Errorf("%q: server accepted=%v, client accepted=%v", id, server, client)
		}
	}
}
