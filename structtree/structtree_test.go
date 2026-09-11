package structtree_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/holoplot/gats/objects"
	"github.com/holoplot/gats/receive"
	"github.com/holoplot/gats/structtree"
)

type location struct {
	Room string
	Row  int
}

type device struct {
	Name     string
	Location location
}

type netIface struct {
	Address string
	MTU     int
}

type config struct {
	Device  device
	Primary *netIface
	Backup  *netIface
	Delay   *durationpb.Duration
	Tags    []string
	Limits  map[string]int
}

// paths collects the leaf paths a walk yields, in order.
func paths(v any) []string {
	var out []string
	for leaf := range structtree.Walk(v) {
		out = append(out, leaf.Path)
	}

	return out
}

func sampleConfig() *config {
	return &config{
		Device:  device{Name: "panel", Location: location{Room: "hall", Row: 3}},
		Primary: &netIface{Address: "10.0.0.1", MTU: 1500},
		Delay:   durationpb.New(250),
		Tags:    []string{"front", "left"},
		Limits:  map[string]int{"gain": 6},
	}
}

func TestWalkMirrorsFieldNamesAsSlashSeparatedPaths(t *testing.T) {
	got := paths(sampleConfig())

	want := []string{
		"Device/Name",
		"Device/Location/Room",
		"Device/Location/Row",
		"Primary/Address",
		"Primary/MTU",
		"Delay",
		"Tags",
		"Limits",
	}

	if !slices.Equal(got, want) {
		t.Errorf("got %v,\nwant %v", got, want)
	}
}

func TestWalkVisitsFieldsInDeclarationOrder(t *testing.T) {
	type ordered struct {
		Zulu  int
		Alpha int
		Mike  int
	}

	got := paths(&ordered{})

	if want := []string{"Zulu", "Alpha", "Mike"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkTreatsEveryNonStructFieldAsALeaf(t *testing.T) {
	n := 7

	type kinds struct {
		Number    int
		Text      string
		Flag      bool
		Slice     []string
		Map       map[string]int
		Pointer   *int
		Interface any
		Array     [2]int
	}

	got := paths(&kinds{Pointer: &n, Interface: "x"})

	want := []string{"Number", "Text", "Flag", "Slice", "Map", "Pointer", "Interface", "Array"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkTreatsProtoMessagesAsLeavesRatherThanDescending(t *testing.T) {
	type withProto struct {
		Pointer *timestamppb.Timestamp
		Value   durationpb.Duration
	}

	// Built through a pointer so the proto value is never copied.
	v := &withProto{Pointer: timestamppb.New(time.Unix(1700000000, 0))}
	v.Value.Seconds = 5

	got := paths(v)

	// Without the proto rule these would descend into the generated
	// message's own fields.
	if want := []string{"Pointer", "Value"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkDescendsThroughPointersToStructs(t *testing.T) {
	got := paths(&config{Primary: &netIface{}})

	if !slices.Contains(got, "Primary/Address") {
		t.Errorf("got %v, want a path through the pointer field", got)
	}
}

func TestWalkSkipsNilPointers(t *testing.T) {
	got := paths(sampleConfig())

	for _, p := range got {
		if p == "Backup" || p == "Backup/Address" {
			t.Errorf("nil pointer field produced %q", p)
		}
	}
}

func TestWalkSkipsUnexportedFields(t *testing.T) {
	type mixed struct {
		Exported   int
		unexported int
		Nested     struct{ Inner int }
	}

	m := &mixed{}
	m.unexported = 1

	got := paths(m)

	if want := []string{"Exported", "Nested/Inner"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkYieldsNothingForAStructWithNoSyncableFields(t *testing.T) {
	type hidden struct{ secret int }

	type outer struct {
		Empty   hidden
		Present int
	}

	o := &outer{}
	o.Empty.secret = 1

	got := paths(o)

	if want := []string{"Present"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// storedLeaves builds v into a store and reads the tree back the way a client
// would: through a packfile.
func storedLeaves(t *testing.T, v any) (plumbing.Hash, map[string][]byte) {
	t.Helper()

	store := objects.NewStore()

	root, err := structtree.Build(store, v)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	hashes, err := store.SelectSince(plumbing.ZeroHash, root)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	var buf bytes.Buffer
	if _, err := store.EncodePack(&buf, hashes); err != nil {
		t.Fatalf("EncodePack: %v", err)
	}

	g := receive.NewGraph()
	if err := receive.Interpret(&buf, g); err != nil {
		t.Fatalf("Interpret: %v", err)
	}

	leaves, err := g.Leaves(root)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}

	return root, leaves
}

func TestBuildStoresEachLeafAtItsPath(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())

	for _, want := range paths(sampleConfig()) {
		if _, ok := leaves[want]; !ok {
			t.Errorf("no blob at %q", want)
		}
	}
	if len(leaves) != len(paths(sampleConfig())) {
		t.Errorf("got %d blobs, want %d", len(leaves), len(paths(sampleConfig())))
	}
}

func TestBuildEncodesOrdinaryValuesAsJSON(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())

	if got, want := string(leaves["Device/Name"]), `"panel"`; got != want {
		t.Errorf("Device/Name: got %s, want %s", got, want)
	}
	if got, want := string(leaves["Primary/MTU"]), "1500"; got != want {
		t.Errorf("Primary/MTU: got %s, want %s", got, want)
	}

	var tags []string
	if err := json.Unmarshal(leaves["Tags"], &tags); err != nil {
		t.Fatalf("Tags is not JSON: %v", err)
	}
	if !slices.Equal(tags, []string{"front", "left"}) {
		t.Errorf("Tags: got %v", tags)
	}
}

func TestBuildEncodesProtoMessagesAsProtoBytes(t *testing.T) {
	_, leaves := storedLeaves(t, sampleConfig())

	var got durationpb.Duration
	if err := proto.Unmarshal(leaves["Delay"], &got); err != nil {
		t.Fatalf("Delay is not a proto message: %v", err)
	}
	if !proto.Equal(&got, durationpb.New(250)) {
		t.Errorf("Delay: got %v, want %v", &got, durationpb.New(250))
	}
}

func TestBuildIsStableAcrossEqualValues(t *testing.T) {
	first, _ := storedLeaves(t, sampleConfig())
	second, _ := storedLeaves(t, sampleConfig())

	if first != second {
		t.Errorf("equal structs produced different roots: %s and %s", first, second)
	}
}

func TestBuildChangesOnlyThePathToAChangedField(t *testing.T) {
	store := objects.NewStore()

	before, err := structtree.Build(store, sampleConfig())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	changed := sampleConfig()
	changed.Device.Location.Row = 4

	after, err := structtree.Build(store, changed)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, err := store.SelectSince(before, after)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	// The rewritten blob plus the Location, Device and root trees above it.
	if want := 4; len(got) != want {
		t.Errorf("got %d objects, want %d", len(got), want)
	}
}

func TestBuildRejectsValuesThatAreNotStructs(t *testing.T) {
	store := objects.NewStore()

	if _, err := structtree.Build(store, 42); err == nil {
		t.Error("Build accepted a non-struct")
	}

	var nilConfig *config
	if _, err := structtree.Build(store, nilConfig); err == nil {
		t.Error("Build accepted a nil pointer")
	}
}
