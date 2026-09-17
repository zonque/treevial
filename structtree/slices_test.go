package structtree_test

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

type withSlices struct {
	Delays  []*durationpb.Duration
	Values  []*structpb.Value
	Ports   []netIface
	Tags    []string
	Raw     []byte
	Numbers [3]int
	Whole   []netIface `treevial:"leaf"`
	Empty   []string
	Absent  []string
}

func sampleSlices() *withSlices {
	return &withSlices{
		Delays: []*durationpb.Duration{durationpb.New(1), durationpb.New(2)},
		Values: []*structpb.Value{structpb.NewStringValue("a")},
		Ports:  []netIface{{Address: "10.0.0.1", MTU: 1500}},
		Tags:   []string{"front", "left"},
		Raw:    []byte{1, 2, 3},
		Whole:  []netIface{{Address: "10.0.0.9", MTU: 9000}},
		Empty:  []string{},
	}
}

func TestASliceOfMessagesBecomesASubtree(t *testing.T) {
	got := paths(sampleSlices())

	want := []string{
		"Delays/0", "Delays/1",
		"Values/0",
		"Ports/0/Address", "Ports/0/MTU",
		"Tags",
		"Raw",
		"Numbers",
		"Whole",
		// Slices of scalars are leaves, so an empty one and a nil one
		// are both values and both stored.
		"Empty",
		"Absent",
	}

	if !slices.Equal(got, want) {
		t.Errorf("got %v,\nwant %v", got, want)
	}
}

func TestSliceElementsAreStoredAsProtoNotJSON(t *testing.T) {
	_, leaves := storedLeaves(t, sampleSlices())

	// The bug this replaces: the slice was one JSON blob, and JSON cannot
	// put a oneof back together.
	content, ok := leaves["Values/0"]
	if !ok {
		t.Fatal("no blob at Values/0")
	}

	var got structpb.Value
	if err := proto.Unmarshal(content, &got); err != nil {
		t.Fatalf("Values/0 is not a proto message: %v", err)
	}
	if got.GetStringValue() != "a" {
		t.Errorf("Values/0: got %v", &got)
	}
}

func TestAMessageWithAOneofSurvivesASlice(t *testing.T) {
	want := &withSlices{
		Values: []*structpb.Value{
			structpb.NewStringValue("text"),
			structpb.NewNumberValue(42),
			structpb.NewBoolValue(true),
		},
	}

	_, leaves := storedLeaves(t, want)

	var got withSlices
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(got.Values) != 3 {
		t.Fatalf("got %d values, want 3", len(got.Values))
	}
	if got.Values[0].GetStringValue() != "text" {
		t.Errorf("first: got %v", got.Values[0])
	}
	if got.Values[1].GetNumberValue() != 42 {
		t.Errorf("second: got %v", got.Values[1])
	}
	if !got.Values[2].GetBoolValue() {
		t.Errorf("third: got %v", got.Values[2])
	}
}

func TestSlicesOfScalarsStayWhole(t *testing.T) {
	_, leaves := storedLeaves(t, sampleSlices())

	for path, want := range map[string]string{
		"Tags":    `["front","left"]`,
		"Raw":     `"AQID"`,
		"Numbers": `[0,0,0]`,
		"Whole":   `[{"Address":"10.0.0.9","MTU":9000}]`,
	} {
		if got := string(leaves[path]); got != want {
			t.Errorf("%s: got %s, want %s", path, got, want)
		}
	}
}

func TestANilOrEmptySubtreeSliceContributesNothing(t *testing.T) {
	type v struct {
		None  []netIface
		Empty []netIface
		Tags  []string
		Nil   []string
	}

	got := paths(&v{Empty: []netIface{}})

	// A slice of scalars is a leaf, so nil and empty are values in their
	// own right and both are stored. A slice that would have been a
	// subtree has nothing to put in one.
	want := []string{"Tags", "Nil"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSliceOrderSurvivesPastTenElements(t *testing.T) {
	// Tree entries sort as text, so "10" comes before "2". Decoding has to
	// read the indices as numbers or the order comes back scrambled.
	want := &withSlices{}
	for i := range 12 {
		want.Delays = append(want.Delays, durationpb.New(stepped(i)))
	}

	_, leaves := storedLeaves(t, want)

	var got withSlices
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(got.Delays) != 12 {
		t.Fatalf("got %d delays, want 12", len(got.Delays))
	}
	for i := range 12 {
		if got.Delays[i].AsDuration() != stepped(i) {
			t.Errorf("index %d: got %v, want %v", i, got.Delays[i].AsDuration(), stepped(i))
		}
	}
}

func TestApplyShortensASliceTheTreeHasTrimmed(t *testing.T) {
	_, leaves := storedLeaves(t, sampleSlices())
	delete(leaves, "Delays/1")

	got := sampleSlices()

	if err := structtree.Apply(got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(got.Delays) != 1 {
		t.Errorf("got %d delays, want 1", len(got.Delays))
	}
}

func TestABuilderDeclaresOneSliceElement(t *testing.T) {
	v := sampleSlices()

	encoded := 0

	b, err := countingMapper(&encoded).NewBuilder(objects.NewStore(), v)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A slice element does have an address, unlike a map entry.
	encoded = 0
	v.Delays[1] = durationpb.New(99)

	got, err := b.Build(&v.Delays[1])
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if encoded != 1 {
		t.Errorf("encoded %d leaves, want 1", encoded)
	}
	if want := rebuilt(t, v); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestASliceRoundTripsThroughARebuild(t *testing.T) {
	want := sampleSlices()

	_, leaves := storedLeaves(t, want)

	var got withSlices
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if rebuilt(t, &got) != rebuilt(t, want) {
		t.Errorf("the round trip changed the value:\n got %+v\nwant %+v", &got, want)
	}
}

// stepped gives each index a distinguishable duration.
func stepped(i int) time.Duration {
	return time.Duration(i+1) * time.Second
}

func TestAMessageInAnInterfaceFieldIsRefused(t *testing.T) {
	type holder struct {
		Payload any
	}

	// Encoding could write the message, but nothing on the far side says
	// which message to expect, so it could never be read back. Better to
	// say so here than to fail confusingly there.
	_, err := structtree.Build(objects.NewStore(), &holder{Payload: durationpb.New(5)})
	if err == nil {
		t.Error("Build accepted a protobuf message in an interface field")
	}
}

func TestAnInterfaceFieldHoldingAnythingElseStillWorks(t *testing.T) {
	type holder struct {
		Payload any
	}

	want := &holder{Payload: "plain"}

	_, leaves := storedLeaves(t, want)

	var got holder
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Payload != "plain" {
		t.Errorf("got %#v, want %q", got.Payload, "plain")
	}
}
