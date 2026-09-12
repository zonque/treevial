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
// This package holds what both sides must agree on: how a client identifies
// itself, which ref that identity is served at, and how the server reports a
// refusal.
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
	// CodeInvalid means the request was malformed: an unusable client ID,
	// a hash that is not a hash, a state the server cannot resolve.
	CodeInvalid ErrorCode = "invalid"
	// CodeAlreadyExists means another connection is already serving that
	// client ID.
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

// MaxIDLength bounds a client ID, so a ref name cannot be grown without limit
// by whatever a client puts in its header.
const MaxIDLength = 255

// RefFor returns the ref a client's data is published at.
func RefFor(clientID string) string {
	return fmt.Sprintf("refs/heads/%s/config", clientID)
}

// ValidateID reports whether clientID may be interpolated into a ref path. The
// ID arrives from the client, so this is the boundary that keeps it from
// naming a ref other than its own or one git would refuse.
func ValidateID(clientID string) error {
	if clientID == "" {
		return fmt.Errorf("client ID is empty")
	}

	if len(clientID) > MaxIDLength {
		return fmt.Errorf("client ID is %d bytes, limit is %d", len(clientID), MaxIDLength)
	}

	for _, r := range clientID {
		if !allowed(r) {
			return fmt.Errorf("client ID %q contains %q", clientID, r)
		}
	}

	switch {
	case strings.HasPrefix(clientID, "."):
		return fmt.Errorf("client ID %q starts with a dot", clientID)
	case strings.HasPrefix(clientID, "-"):
		return fmt.Errorf("client ID %q starts with a dash", clientID)
	case strings.Contains(clientID, ".."):
		return fmt.Errorf("client ID %q contains %q", clientID, "..")
	case strings.HasSuffix(clientID, ".lock"):
		return fmt.Errorf("client ID %q ends with the reserved %q suffix", clientID, ".lock")
	}

	return nil
}

// allowed reports whether r may appear in a client ID. The set is deliberately
// narrower than git's own rules: letters, digits, and the three separators.
func allowed(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-', r == '_', r == '.':
		return true
	default:
		return false
	}
}
