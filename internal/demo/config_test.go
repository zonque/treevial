package demo_test

import (
	"slices"
	"testing"

	"github.com/zonque/treevial/internal/demo"
	"github.com/zonque/treevial/structtree"
)

func TestExampleHasTenLeavesMirroringTheStruct(t *testing.T) {
	var got []string
	for leaf := range structtree.Walk(demo.Example("printer-7")) {
		got = append(got, leaf.Path)
	}

	want := []string{
		"Device/Name",
		"Device/Serial",
		"Device/Location/Room",
		"Device/Location/Row",
		"Network/Hostname",
		"Network/Primary/Address",
		"Network/Primary/MTU",
		"Network/DNS",
		"Audio/Gain",
		"Audio/Delay",
	}

	if !slices.Equal(got, want) {
		t.Errorf("got %v,\nwant %v", got, want)
	}
	if len(want) != demo.LeafCount {
		t.Errorf("LeafCount is %d, but the example has %d leaves", demo.LeafCount, len(want))
	}
}

func TestExampleIsPersonalisedPerClient(t *testing.T) {
	a := demo.Example("printer-7")
	b := demo.Example("sensor-3")

	if a.Device.Name == b.Device.Name {
		t.Error("two clients were given the same device name")
	}
	if a.Network.Hostname == b.Network.Hostname {
		t.Error("two clients were given the same hostname")
	}
}

func TestExampleLeavesTheSecondaryInterfaceUnset(t *testing.T) {
	// A nil pointer contributes no path at all, which is what makes a
	// field appearing later read as an addition.
	if demo.Example("printer-7").Network.Secondary != nil {
		t.Error("Secondary is set, so the nil case is no longer demonstrated")
	}

	for leaf := range structtree.Walk(demo.Example("printer-7")) {
		if leaf.Path == "Network/Secondary" || leaf.Path == "Network/Secondary/Address" {
			t.Errorf("nil interface produced %q", leaf.Path)
		}
	}
}
