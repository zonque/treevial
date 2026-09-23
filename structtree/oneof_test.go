package structtree_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/zonque/treevial/internal/testpb"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

// A oneof is the case that catches a message being stored as JSON: the
// generated code holds it in a wrapper, and encoding/json can write that but
// cannot read it back.
type everyShape struct {
	Pointer  *testpb.Setting
	Value    testpb.Setting
	Slice    []*testpb.Setting
	ValSlice []testpb.Setting
	Keyed    map[string]*testpb.Setting
	Tagged   *testpb.Setting `treevial:"leaf"`
	Nested   struct{ S *testpb.Setting }
}

func setting(text string) *testpb.Setting {
	return &testpb.Setting{Name: text, Value: &testpb.Setting_Text{Text: text}}
}

func sampleShapes() *everyShape {
	v := &everyShape{
		Pointer:  setting("p"),
		Slice:    []*testpb.Setting{setting("s0"), setting("s1")},
		ValSlice: []testpb.Setting{*setting("vs")},
		Keyed:    map[string]*testpb.Setting{"k": setting("m")},
		Tagged:   setting("t"),
	}
	v.Value.Name = "v"
	v.Value.Value = &testpb.Setting_Text{Text: "v"}
	v.Nested.S = setting("n")

	return v
}

// TestTheFixtureWouldCatchTheMistake guards the guard: if this message ever
// grew a MarshalJSON, the tests below would pass whether or not messages were
// being stored properly.
func TestTheFixtureWouldCatchTheMistake(t *testing.T) {
	m := setting("x")

	if reflect.TypeFor[*testpb.Setting]().Implements(reflect.TypeFor[json.Marshaler]()) {
		t.Fatal("testpb.Setting carries its own JSON, so it can no longer expose the mistake")
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	if err := json.Unmarshal(data, &testpb.Setting{}); err == nil {
		t.Fatal("encoding/json read the oneof back, so it can no longer expose the mistake")
	}
}

func TestEveryShapeStoresAMessageAsAMessage(t *testing.T) {
	_, leaves := storedLeaves(t, sampleShapes())

	for _, path := range []string{
		"Pointer", "Value", "Tagged", "Nested/S",
		"Keyed/k", "Slice/0", "Slice/1", "ValSlice/0",
	} {
		content, ok := leaves[path]
		if !ok {
			t.Errorf("%s: no blob", path)

			continue
		}

		var got testpb.Setting
		if err := proto.Unmarshal(content, &got); err != nil {
			t.Errorf("%s: not a proto message: %q", path, content)
		}
	}
}

func TestEveryShapeRoundTripsAOneof(t *testing.T) {
	want := sampleShapes()

	_, leaves := storedLeaves(t, want)

	var got everyShape
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for what, pair := range map[string][2]*testpb.Setting{
		"Pointer":    {got.Pointer, want.Pointer},
		"Tagged":     {got.Tagged, want.Tagged},
		"Nested/S":   {got.Nested.S, want.Nested.S},
		"Keyed/k":    {got.Keyed["k"], want.Keyed["k"]},
		"Slice/0":    {got.Slice[0], want.Slice[0]},
		"Slice/1":    {got.Slice[1], want.Slice[1]},
		"Value":      {&got.Value, &want.Value},
		"ValSlice/0": {&got.ValSlice[0], &want.ValSlice[0]},
	} {
		if !proto.Equal(pair[0], pair[1]) {
			t.Errorf("%s: got %v, want %v", what, pair[0], pair[1])
		}
	}
}

func TestAOneofSurvivesAllThreeVariants(t *testing.T) {
	want := &everyShape{Slice: []*testpb.Setting{
		{Name: "a", Value: &testpb.Setting_Text{Text: "hello"}},
		{Name: "b", Value: &testpb.Setting_Number{Number: 42}},
		{Name: "c", Value: &testpb.Setting_Flag{Flag: true}},
	}}

	_, leaves := storedLeaves(t, want)

	var got everyShape
	if err := structtree.Apply(&got, leaves); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.Slice[0].GetText() != "hello" {
		t.Errorf("text variant: got %v", got.Slice[0])
	}
	if got.Slice[1].GetNumber() != 42 {
		t.Errorf("number variant: got %v", got.Slice[1])
	}
	if !got.Slice[2].GetFlag() {
		t.Errorf("flag variant: got %v", got.Slice[2])
	}
}

func TestAMessageInsideATaggedStructIsRefusedWithAOneof(t *testing.T) {
	type config struct {
		Whole struct{ S *testpb.Setting } `treevial:"leaf"`
	}

	v := &config{}
	v.Whole.S = setting("x")

	if _, err := structtree.Build(objects.NewStore(), v); err == nil {
		t.Error("Build accepted a tagged struct holding a message with a oneof")
	}
}
