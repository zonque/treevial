// Package treevial implements a reversed-role git object transfer.
//
// The client dials the server and keeps one long-lived stream open, but it
// never asks for anything: it identifies itself, states which tree it already
// holds, and from then on the server pushes objects down that stream whenever
// the client's ref moves. Neither side touches the filesystem — the server
// keeps its objects in memory and the client interprets each one as it is
// inflated off the wire.
//
// The two sides are separate packages, so a client repository and a server
// repository can each depend on only what it needs:
//
//   - [github.com/zonque/treevial/client] dials, subscribes and interprets.
//   - [github.com/zonque/treevial/server] serves clients and pushes to them.
//   - [github.com/zonque/treevial/objects] builds and packs the object graph
//     a server serves.
//   - [github.com/zonque/treevial/receive] interprets an arriving packfile
//     without storing it.
//
// This package holds what both sides must agree on: what counts as a usable
// ref, and how the server reports a refusal. How a client decides which ref to
// ask for is the client's own business — see
// [github.com/zonque/treevial/client.RefFor].
//
// The wire format is described by PROTOCOL.md in the repository: pkt-line
// framed messages over a plain TCP connection. Its Go implementation is
// internal, because the supported surface is the Go API in these packages;
// anyone implementing another language's client works from PROTOCOL.md.
package treevial

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorCode classifies a protocol error, so a client can tell why the server
// turned it away without matching on message text.
type ErrorCode string

const (
	// CodeUnknown is what any error that did not come from the other side
	// classifies as: a dropped connection, a local failure.
	CodeUnknown ErrorCode = "unknown"
	// CodeInvalid means the request was malformed: an unusable ref, a hash
	// that is not a hash, a state the server cannot resolve.
	CodeInvalid ErrorCode = "invalid"
	// CodeAlreadyExists means another connection is already subscribed to
	// that ref.
	CodeAlreadyExists ErrorCode = "exists"
	// CodeInternal means the server failed on its own account.
	CodeInternal ErrorCode = "internal"
)

// Error is a failure the server reported over the connection. It travels as
// one line, so the code is a short token rather than prose.
type Error struct {
	Code    ErrorCode
	Message string
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("treevial: %s: %s", e.Code, e.Message)
}

// Errorf builds an Error with a formatted message.
func Errorf(code ErrorCode, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// CodeOf reports the code of err, unwrapping as it goes. Anything that is not
// an Error — including nil — is CodeUnknown.
func CodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}

	return CodeUnknown
}

// MaxRefLength bounds a ref, so a server cannot be handed an unbounded name to
// key on.
const MaxRefLength = 512

// ValidateRef reports whether ref is a usable git ref name.
//
// The server takes the ref a client sends verbatim, so this is the boundary
// that keeps a malformed one out of whatever it is keyed on. The rules are a
// conservative subset of git's own: under refs/, no empty or dot-leading
// component, no component ending in the reserved .lock suffix, none of the
// characters git's revision and refspec syntax claims, and no control
// characters.
func ValidateRef(ref string) error {
	switch {
	case ref == "":
		return fmt.Errorf("ref is empty")
	case len(ref) > MaxRefLength:
		return fmt.Errorf("ref is %d bytes, limit is %d", len(ref), MaxRefLength)
	case !strings.HasPrefix(ref, "refs/"):
		return fmt.Errorf("ref %q does not start with refs/", ref)
	case strings.Contains(ref, ".."):
		return fmt.Errorf("ref %q contains %q", ref, "..")
	case strings.Contains(ref, "@{"):
		return fmt.Errorf("ref %q contains %q", ref, "@{")
	}

	for _, r := range ref {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return fmt.Errorf("ref %q contains %q", ref, r)
		}
	}

	components := strings.Split(ref, "/")
	if len(components) < 3 {
		return fmt.Errorf("ref %q names nothing under refs/", ref)
	}

	for _, c := range components {
		switch {
		case c == "":
			return fmt.Errorf("ref %q has an empty component", ref)
		case strings.HasPrefix(c, "."):
			return fmt.Errorf("ref %q has a component starting with a dot", ref)
		case strings.HasSuffix(c, ".lock"):
			// Reserved on every component, not just the last.
			return fmt.Errorf("ref %q has a component ending in %q", ref, ".lock")
		}
	}

	return nil
}
