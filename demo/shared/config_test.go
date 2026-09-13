package shared_test

import (
	"slices"
	"testing"

	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/structtree"
)

func TestExampleLeavesMirrorTheStruct(t *testing.T) {
	var got []string
	for leaf := range structtree.Walk(shared.Example("printer-7")) {
		got = append(got, leaf.Path)
	}

	want := []string{
		"Device/Name",
		"Device/Serial",
		"Device/Installed",
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
	if len(want) != shared.LeafCount {
		t.Errorf("LeafCount is %d, but the example has %d leaves", shared.LeafCount, len(want))
	}
}

func TestExampleIsPersonalisedPerLabel(t *testing.T) {
	a := shared.Example("printer-7")
	b := shared.Example("sensor-3")

	if a.Device.Name == b.Device.Name {
		t.Error("two labels produced the same device name")
	}
	if a.Network.Hostname == b.Network.Hostname {
		t.Error("two labels produced the same hostname")
	}
}

func TestExampleLeavesTheSecondaryInterfaceUnset(t *testing.T) {
	// A nil pointer contributes no path at all, which is what makes a
	// field appearing later read as an addition.
	if shared.Example("printer-7").Network.Secondary != nil {
		t.Error("Secondary is set, so the nil case is no longer demonstrated")
	}

	for leaf := range structtree.Walk(shared.Example("printer-7")) {
		if leaf.Path == "Network/Secondary" || leaf.Path == "Network/Secondary/Address" {
			t.Errorf("nil interface produced %q", leaf.Path)
		}
	}
}
