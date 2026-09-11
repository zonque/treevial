// Package gats implements a reversed-role git object transfer.
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
//   - [github.com/holoplot/gats/client] dials, subscribes and interprets.
//   - [github.com/holoplot/gats/server] serves clients and pushes to them.
//   - [github.com/holoplot/gats/objects] builds and packs the object graph a
//     server serves.
//   - [github.com/holoplot/gats/receive] interprets an arriving packfile
//     without storing it.
//
// This package holds what both sides must agree on: how a client identifies
// itself, and which ref that identity is served at.
//
// The wire format is defined by proto/gats.proto in the repository. Its
// generated Go bindings are internal, because the supported surface is the Go
// API in these packages; anyone implementing another language's client works
// from the .proto file.
package gats

import (
	"fmt"
	"strings"
)

// IDHeader is the gRPC request header a client states its ID in. The value
// decides which ref the client is served, so both sides agree on it here.
const IDHeader = "gats-client-id"

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
