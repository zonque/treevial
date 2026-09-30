package member

import (
	"encoding/json"
	"fmt"

	"github.com/zonque/treevial/demo/shared"
)

// The two things a node may propose.
const (
	// KindLoad carries the whole database. It is proposed once, by
	// whichever node wins the first election, and is how the bulk of the
	// state reaches every other node.
	KindLoad = "load"
	// KindMTU carries one field of one device's configuration. It is what
	// a message bus would deliver: frequent, and touching almost nothing.
	KindMTU = "mtu"
)

// Command is one entry in the replicated log, in JSON so that a person reading
// a node's output can see what was applied.
//
// A real member would carry whatever its bus delivered, which means a path and
// a value of some encoded type. Naming one concrete field instead keeps the
// incremental build that follows — one blob and the trees above it — visible as
// itself rather than behind a layer of reflection.
type Command struct {
	Kind string `json:"kind"`

	// Devices is the whole database, set on a load.
	Devices map[string]*shared.Config `json:"devices,omitempty"`

	// Ref and MTU name the field to change, set on an mtu.
	Ref string `json:"ref,omitempty"`
	MTU int    `json:"mtu,omitempty"`
}

// Load encodes an entry carrying the whole database.
func Load(set map[string]*shared.Config) ([]byte, error) {
	return json.Marshal(Command{Kind: KindLoad, Devices: set})
}

// SetMTU encodes an entry that changes one device's MTU.
//
// The value is decided by the node proposing it and carried in the entry, so no
// node has to agree with another about what the next one should be: a follower
// applies what it is given.
func SetMTU(ref string, mtu int) ([]byte, error) {
	return json.Marshal(Command{Kind: KindMTU, Ref: ref, MTU: mtu})
}

// Decode reads an entry back. An unknown kind is an error rather than an entry
// that quietly does nothing, since a node that cannot apply what its peers
// have committed is not a member of anything.
func Decode(data []byte) (Command, error) {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return Command{}, fmt.Errorf("decode entry: %w", err)
	}

	switch cmd.Kind {
	case KindLoad, KindMTU:
		return cmd, nil
	default:
		return Command{}, fmt.Errorf("entry of unknown kind %q", cmd.Kind)
	}
}
