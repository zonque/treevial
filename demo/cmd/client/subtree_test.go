package main

import (
	"testing"

	"github.com/zonque/treevial/demo/shared"
)

func TestChangedSubtreeIsTheParentOfALoneChange(t *testing.T) {
	got := changedSubtree([]string{"Network/Primary/MTU"})

	if want := "Network/Primary"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestChangedSubtreeIsTheSharedParentOfSiblings(t *testing.T) {
	got := changedSubtree([]string{"Device/Location/Room", "Device/Location/Row"})

	if want := "Device/Location"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestChangedSubtreeClimbsToWhatTheChangesShare(t *testing.T) {
	got := changedSubtree([]string{"Network/Hostname", "Network/Primary/MTU"})

	if want := "Network"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestChangedSubtreeIsTheRootWhenChangesAreUnrelated(t *testing.T) {
	got := changedSubtree([]string{"Device/Name", "Audio/Gain"})

	if got != "" {
		t.Errorf("got %q, want the root", got)
	}
}

func TestChangedSubtreeOfATopLevelLeafIsTheRoot(t *testing.T) {
	if got := changedSubtree([]string{"Gain"}); got != "" {
		t.Errorf("got %q, want the root", got)
	}
}

func TestValueAtReachesNestedFieldsThroughPointers(t *testing.T) {
	config := shared.Example("printer-7")

	got, err := valueAt(config, "Network/Primary")
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}

	iface, ok := got.(shared.Interface)
	if !ok {
		t.Fatalf("got %T, want shared.Interface", got)
	}
	if iface.MTU != 1500 {
		t.Errorf("MTU: got %d, want 1500", iface.MTU)
	}
}

func TestValueAtReturnsTheWholeValueForTheRoot(t *testing.T) {
	config := shared.Example("printer-7")

	got, err := valueAt(config, "")
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}

	if _, ok := got.(shared.Config); !ok {
		t.Errorf("got %T, want shared.Config", got)
	}
}

func TestValueAtReportsAnUnsetBranch(t *testing.T) {
	config := shared.Example("printer-7")

	// Secondary is nil, so there is nothing to show under it.
	if _, err := valueAt(config, "Network/Secondary"); err == nil {
		t.Error("valueAt resolved a nil branch")
	}
}

func TestValueAtReportsAPathTheStructDoesNotHave(t *testing.T) {
	config := shared.Example("printer-7")

	if _, err := valueAt(config, "Network/Telemetry"); err == nil {
		t.Error("valueAt resolved a path the struct does not have")
	}
}
