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
const LeafCount = 11

// Config is the value being synchronised.
//
// Every field that is not a struct is a leaf stored in one blob; every plain
// nested struct becomes a subtree; Audio.Delay is a leaf despite being a
// struct, because a protobuf message implements proto.Message; and
// Device.Installed is a leaf because it says so.
//
//	Device/Name                Network/Hostname          Audio/Gain
//	Device/Serial              Network/Primary/Address   Audio/Delay
//	Device/Installed           Network/Primary/MTU
//	Device/Location/Room       Network/DNS
//	Device/Location/Row
type Config struct {
	Device  Device
	Network Network
	Audio   Audio
}

// Device describes the unit itself.
//
// Installed shows why the tag matters. time.Time is a struct that is not a
// protobuf message and has only unexported fields, so without the tag the
// walker would descend into it, find nothing it may read, and the field would
// vanish from the tree altogether. Tagged, it is stored whole — and JSON
// already knows how to write a time, so no encoding of our own is needed.
type Device struct {
	Name      string
	Serial    string
	Installed time.Time `treevial:"leaf"`
	Location  Location
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

// Example returns a configuration personalised with label — whatever the
// provider chose to key on, usually the ref — so two subscribers never hold the
// same tree.
func Example(label string) *Config {
	return &Config{
		Device: Device{
			Name:      label,
			Serial:    fmt.Sprintf("SN-%s-0001", label),
			Installed: time.Unix(1700000000, 0).UTC(),
			Location:  Location{Room: "hall-a", Row: 3},
		},
		Network: Network{
			Hostname: label + ".local",
			Primary:  &Interface{Address: "10.0.0.7", MTU: 1500},
			DNS:      []string{"10.0.0.1", "10.0.0.2"},
		},
		Audio: Audio{
			Gain:  -6.5,
			Delay: durationpb.New(12 * time.Millisecond),
		},
	}
}

// BuildTree stores the example configuration for label and returns the root
// tree hash.
func BuildTree(store *objects.Store, label string) (plumbing.Hash, error) {
	return structtree.Build(store, Example(label))
}
