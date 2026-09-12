package client

import (
	"fmt"
	"strings"
)

// MaxIDLength bounds a client ID, so a ref name cannot be grown without limit
// by whatever a client is configured with.
const MaxIDLength = 255

// RefFor returns the ref a client asks the server for, built from the client's
// own ID.
//
// This convention lives here, on the client side. The server takes whatever ref
// it is sent verbatim and derives nothing from its shape, so a client is free
// to use a different one — this is simply the one the Go client uses.
func RefFor(clientID string) string {
	return fmt.Sprintf("refs/heads/%s/config", clientID)
}

// ValidateID reports whether clientID may be interpolated into a ref by
// [RefFor]. It is checked before the ref is built, so an unusable ID is caught
// here rather than becoming a ref the server refuses.
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
