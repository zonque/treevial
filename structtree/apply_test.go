package structtree_test

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/holoplot/gats/objects"
	"github.com/holoplot/gats/structtree"
)

// rebuilt stores v and returns its root hash, so two values can be compared by
// the tree they produce. Comparing roots avoids reflect.DeepEqual, which is
// unreliable on protobuf messages because of their internal state.
func rebuilt(t *testing.T, v any) plumbing.Hash {
	t.Helper()

	root, err := structtree.Build(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return root
}

func TestApplyRebuildsTheStructItWasBuiltFrom(t *testing.T) {
	want := sampleConfig()

	_, leaves := storedLeaves(t, want)

	var got config
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if rebuilt(t, &got) != rebuilt(t, want) {
		t.Errorf("round trip changed the value:\n got %+v\nwant %+v", &got, want)
	}
}

func TestApplyFillsNestedFieldsAtTheirPaths(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())

	var got config
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Device.Location.Room != "hall" {
		t.Errorf("Device/Location/Room: got %q, want %q", got.Device.Location.Room, "hall")
	}
	if got.Device.Location.Row != 3 {
		t.Errorf("Device/Location/Row: got %d, want 3", got.Device.Location.Row)
	}
	if got.Limits["gain"] != 6 {
		t.Errorf("Limits: got %v, want gain=6", got.Limits)
	}
}

func TestApplyAllocatesPointersToStructsItNeeds(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())

	var got config
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Primary == nil {
		t.Fatal("Primary was left nil")
	}
	if got.Primary.MTU != 1500 {
		t.Errorf("Primary/MTU: got %d, want 1500", got.Primary.MTU)
	}
}

func TestApplyDecodesProtoMessageLeaves(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())

	var got config
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !proto.Equal(got.Delay, durationpb.New(250)) {
		t.Errorf("Delay: got %v, want %v", got.Delay, durationpb.New(250))
	}
}

func TestApplyZeroesFieldsTheTreeDoesNotCarry(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())
	delete(leaves, "Device/Name")
	delete(leaves, "Primary/MTU")

	got := sampleConfig()

	if err := structtree.Apply(got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The tree is the source of truth, so a path it no longer carries
	// leaves its field zeroed rather than stale.
	if got.Device.Name != "" {
		t.Errorf("Device/Name: got %q, want it zeroed", got.Device.Name)
	}
	if got.Primary.MTU != 0 {
		t.Errorf("Primary/MTU: got %d, want it zeroed", got.Primary.MTU)
	}
	if got.Device.Location.Room != "hall" {
		t.Errorf("an untouched field was zeroed: %q", got.Device.Location.Room)
	}
}

func TestApplyNilsPointersWhoseSubtreeIsGone(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())

	got := sampleConfig()
	got.Backup = &netIface{Address: "10.0.0.2", MTU: 9000}

	if err := structtree.Apply(got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// sampleConfig leaves Backup nil, so the tree has no Backup/ paths at
	// all and the pointer has to go back to nil.
	if got.Backup != nil {
		t.Errorf("Backup: got %+v, want nil", got.Backup)
	}
}

func TestApplyIgnoresPathsTheStructDoesNotKnow(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())
	leaves["Device/FirmwareRevision"] = []byte(`"2.1"`)
	leaves["Telemetry/Endpoint"] = []byte(`"https://example.invalid"`)

	var got config
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply rejected a path it does not know: %v", err)
	}

	if got.Device.Name != "panel" {
		t.Errorf("Device/Name: got %q, want %q", got.Device.Name, "panel")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())

	var got config
	for i := range 3 {
		if err := structtree.Apply(&got, leaves); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
	}

	// Applying repeatedly must not grow the slice or the map.
	if len(got.Tags) != 2 {
		t.Errorf("Tags grew to %v", got.Tags)
	}
	if len(got.Limits) != 1 {
		t.Errorf("Limits grew to %v", got.Limits)
	}
	if rebuilt(t, &got) != rebuilt(t, sampleConfig()) {
		t.Error("repeated application changed the value")
	}
}

func TestApplyReportsWhichPathFailedToDecode(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())
	leaves["Primary/MTU"] = []byte("not a number")

	var got config

	err := structtree.Apply(&got, leaves)
	if err == nil {
		t.Fatal("Apply accepted undecodable bytes")
	}
	if !contains(err.Error(), "Primary/MTU") {
		t.Errorf("error %q does not name the failing path", err)
	}
}

func TestApplyRejectsDestinationsItCannotWriteTo(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())

	if err := structtree.Apply(config{}, leaves); err == nil {
		t.Error("Apply accepted a non-pointer destination")
	}

	var nilConfig *config
	if err := structtree.Apply(nilConfig, leaves); err == nil {
		t.Error("Apply accepted a nil pointer")
	}

	n := 3
	if err := structtree.Apply(&n, leaves); err == nil {
		t.Error("Apply accepted a pointer to a non-struct")
	}
}

func TestApplySkipsUnexportedFields(t *testing.T) {
	type mixed struct {
		Exported   string
		unexported int
	}

	m := &mixed{}
	m.unexported = 1

	if err := structtree.Apply(m, map[string][]byte{"Exported": []byte(`"set"`)}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if m.Exported != "set" {
		t.Errorf("Exported: got %q, want %q", m.Exported, "set")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}
