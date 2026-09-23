package structtree_test

import (
	"fmt"
	"testing"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

// benchEntries is the scale these benchmarks are measured at: a map of ten
// thousand entries, each a device of three leaves — Name, Location/Room and
// Location/Row — so thirty thousand leaves in all.
const benchEntries = 10000

// benchValue keeps its entries behind pointers, which is what lets a Builder
// be handed the address of one of them.
type benchValue struct {
	Ports map[string]*device
}

func benchSample() *benchValue {
	ports := make(map[string]*device, benchEntries)

	for i := range benchEntries {
		ports[fmt.Sprintf("eth%05d", i)] = &device{
			Name:     fmt.Sprintf("port-%d", i),
			Location: location{Room: fmt.Sprintf("hall-%d", i%8), Row: i % 64},
		}
	}

	return &benchValue{Ports: ports}
}

// benchTarget is the entry the targeted builds declare, somewhere in the
// middle of the map so nothing about its position is special.
const benchTarget = "eth04212"

// BenchmarkBuild is a full build: every leaf encoded and hashed, which is what
// it costs to learn hashes the value may already have had.
func BenchmarkBuild(b *testing.B) {
	v := benchSample()

	b.ReportAllocs()

	for b.Loop() {
		if _, err := structtree.Build(objects.NewStore(), v); err != nil {
			b.Fatalf("Build: %v", err)
		}
	}
}

// BenchmarkBuilderEverything is the same work through a Builder that has been
// told nothing about what moved, so it walks the whole value again.
func BenchmarkBuilderEverything(b *testing.B) {
	v := benchSample()

	builder, err := structtree.NewBuilder(objects.NewStore(), v)
	if err != nil {
		b.Fatalf("NewBuilder: %v", err)
	}
	if _, err := builder.Build(); err != nil {
		b.Fatalf("Build: %v", err)
	}

	b.ReportAllocs()

	for b.Loop() {
		if _, err := builder.Build(); err != nil {
			b.Fatalf("Build: %v", err)
		}
	}
}

// BenchmarkBuilderOneField declares a single leaf. Nothing outside the path to
// it is encoded, hashed or even looked at; what is left is mostly the map's
// own tree object, which has ten thousand entries and has to be written again
// whenever any one of them moves.
func BenchmarkBuilderOneField(b *testing.B) {
	v := benchSample()

	builder, err := structtree.NewBuilder(objects.NewStore(), v)
	if err != nil {
		b.Fatalf("NewBuilder: %v", err)
	}
	if _, err := builder.Build(); err != nil {
		b.Fatalf("Build: %v", err)
	}

	target := &v.Ports[benchTarget].Location.Row

	b.ReportAllocs()

	for b.Loop() {
		if _, err := builder.Build(target); err != nil {
			b.Fatalf("Build: %v", err)
		}
	}
}

// BenchmarkBuilderOneEntry declares a whole entry of the map instead of one
// field of it, which is the coarsest a targeted build gets before it is a
// full one.
func BenchmarkBuilderOneEntry(b *testing.B) {
	v := benchSample()

	builder, err := structtree.NewBuilder(objects.NewStore(), v)
	if err != nil {
		b.Fatalf("NewBuilder: %v", err)
	}
	if _, err := builder.Build(); err != nil {
		b.Fatalf("Build: %v", err)
	}

	target := v.Ports[benchTarget]

	b.ReportAllocs()

	for b.Loop() {
		if _, err := builder.Build(target); err != nil {
			b.Fatalf("Build: %v", err)
		}
	}
}
