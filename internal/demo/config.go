// Package demo holds the example value that the treevial example programs
// synchronise: a deeply nested Go struct, mapped onto a git tree by
// structtree. It is not part of the library's API.
package demo

import (
	"fmt"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

// LeafCount is the number of blobs Example produces. Secondary is nil, so it
// contributes nothing.
const LeafCount = 10

// Config is the value being synchronised.
//
// Every field that is not a struct is a leaf stored in one blob; every plain
// nested struct becomes a subtree; and Audio.Delay is a leaf despite being a
// struct, because a protobuf message implements proto.Message.
//
//	Device/Name                Network/Hostname          Audio/Gain
//	Device/Serial              Network/Primary/Address   Audio/Delay
//	Device/Location/Room       Network/Primary/MTU
//	Device/Location/Row        Network/DNS
type Config struct {
	Device  Device
	Network Network
	Audio   Audio
}

// Device describes the unit itself.
type Device struct {
	Name     string
	Serial   string
	Location Location
}

// Location is nested one level deeper, to show the path building up.
type Location struct {
	Room string
	Row  int
}

// Network holds the addressing. Secondary is left nil, so nothing is stored
// for it at all.
type Network struct {
	Hostname  string
	Primary   *Interface
	Secondary *Interface
	DNS       []string
}

// Interface is reached through a pointer, which structtree follows.
type Interface struct {
	Address string
	MTU     int
}

// Audio carries a protobuf message, which is a leaf rather than a subtree.
type Audio struct {
	Gain  float64
	Delay *durationpb.Duration
}

// Example returns a configuration personalised for one client, so two clients
// never hold the same tree.
func Example(clientID string) *Config {
	return &Config{
		Device: Device{
			Name:     clientID,
			Serial:   fmt.Sprintf("SN-%s-0001", clientID),
			Location: Location{Room: "hall-a", Row: 3},
		},
		Network: Network{
			Hostname: clientID + ".local",
			Primary:  &Interface{Address: "10.0.0.7", MTU: 1500},
			DNS:      []string{"10.0.0.1", "10.0.0.2"},
		},
		Audio: Audio{
			Gain:  -6.5,
			Delay: durationpb.New(12 * time.Millisecond),
		},
	}
}

// BuildTree stores the example configuration for a client and returns the root
// tree hash.
func BuildTree(store *objects.Store, clientID string) (plumbing.Hash, error) {
	return structtree.Build(store, Example(clientID))
}
