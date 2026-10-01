// Package structtree maps a Go struct onto a git tree.
//
// The hierarchy of the struct becomes the hierarchy of the tree: each field
// name is a path element, joined with "/", so a value at
// cfg.Network.Primary.MTU is stored as the blob "Network/Primary/MTU". That is
// what lets the rest of treevial work unchanged — the same path handling, the
// same subtree pruning, the same diffs — while the thing being synchronised is
// an ordinary Go value.
//
// # What becomes a leaf
//
// This is the decision the whole mapping turns on, because it is what decides
// the shape of the tree — and therefore the paths, what a diff reports, and how
// little has to move when one field changes.
//
// A field is a leaf if it is tagged:
//
//	type Config struct {
//		Installed time.Time `treevial:"leaf"`
//	}
//
// Tagging is the primary way to say so. It is honoured whatever rule is in
// force, and it keeps the decision in the type itself, next to the fields,
// where a reader of the struct will look for it.
//
// Failing a tag, a field is a leaf if it has no structure to descend into: an
// int, a string, a []byte, a []string. A generated protobuf message is a leaf
// too, stored as one blob of its own wire bytes.
//
// Everything with structure becomes a subtree:
//
//   - a nested struct, one child per field;
//   - a map, one child per key, so changing one entry of a large map costs that
//     entry rather than the whole of it. Keys must be strings, since they
//     become path elements; a map keyed by anything else is reported as an
//     error unless it is tagged as a leaf;
//   - a slice or array whose elements have structure of their own, one child
//     per element, named by index. A run of scalars stays one blob, since
//     splitting a []byte into a blob apiece would serve nobody.
//
// The slice rule is not only about granularity. A message stored on its own
// goes as its own wire bytes; one buried inside a leaf would go as JSON, which
// writes a oneof as the wrapper the generated code uses and then has nothing to
// unmarshal it back into. So a leaf that merely contains a message is refused
// when the tree is built, rather than written and found unreadable later: leave
// it untagged and each message gets a blob of its own, or give a [Mapper] an
// Encode and Decode that know what to do with it.
//
// An interface is treated as scalar, since what it holds is not known from the
// type. A protobuf message in one is refused for the same reason: nothing on
// the far side would say which message to unmarshal.
//
// The default rule matters most for what it gets wrong. A time.Time is a
// struct, is not a protobuf message, and has only unexported fields — so the
// walker descends into it, finds nothing it may read, and the field vanishes
// from the tree. Tag it and it is stored whole. Any struct of that shape needs
// the same treatment, and the tag is how to give it.
//
// For types you do not own, and so cannot tag, a [Mapper] carries a rule of
// your own alongside the encoding that serves it.
//
// # What a change costs
//
// Because a leaf's blob changes only when that leaf's bytes change, and a
// subtree's hash changes only when something beneath it changes, updating one
// deep field produces one blob plus the trees on its path — however large the
// rest of the value is. That is what the receiving side is sent, and what
// [Mapper.ApplySince] has to decode.
//
// Producing them is another matter. [Mapper.Build] encodes and hashes every
// leaf, so it pays for the whole value however little of it moved.
//
// A [Builder] pays only for what changed. It keeps the tree it last built and
// looks at nothing but the paths you declare, which on a map of ten thousand
// entries is some thirty times cheaper than a full build — most of what is
// left being the map's own tree object, which has to be written again whenever
// any one of its entries moves. The benchmarks in bench_test.go are where that
// figure comes from. The price is that it believes you: see [Builder] for what
// it will not notice, and when to hand it the whole value instead.
package structtree

import (
	"encoding/json"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"google.golang.org/protobuf/proto"

	"github.com/zonque/treevial/objects"
)

// protoMessage is the interface a struct must implement to be treated as a
// leaf rather than descended into.
var protoMessage = reflect.TypeFor[proto.Message]()

// Leaf is one value of a walked struct, together with the path its blob is
// stored at.
type Leaf struct {
	// Path is slash-separated and mirrors the field names leading to the
	// value, for example "Network/Primary/MTU".
	Path string
	// Value is the field's value.
	Value reflect.Value
}

// Walk returns an iterator over the leaves of v, using the default rules. It is
// shorthand for a zero [Mapper]'s Walk.
func Walk(v any) iter.Seq[Leaf] {
	return Mapper{}.Walk(v)
}

// Walk returns an iterator over the leaves of v, in the order the fields are
// declared, descending depth-first.
//
// v may be a struct or a pointer to one. Unexported fields are skipped,
// because they cannot be read, and nil pointers are skipped, because there is
// nothing to store; a field that becomes nil therefore reads as a deletion and
// one that stops being nil as an addition.
func (m Mapper) Walk(v any) iter.Seq[Leaf] {
	return func(yield func(Leaf) bool) {
		root := reflect.ValueOf(v)
		if !root.IsValid() {
			return
		}

		var err error

		m.walk(root, "", yield, &err)
	}
}

// walk yields the leaves of v under prefix, reporting whether iteration should
// carry on. The first thing it cannot map is recorded in failure, which Build
// reports and Walk ignores.
func (m Mapper) walk(v reflect.Value, prefix string, yield func(Leaf) bool, failure *error) bool {
	v, ok := deref(v)
	if !ok {
		return true
	}

	// Nothing to descend into, so the value itself is the leaf. Only the
	// root can reach this with an empty prefix, and a root with no members
	// is refused before the walk starts.
	if !hasMembers(v) {
		return yield(Leaf{Path: prefix, Value: v})
	}

	return m.eachMember(v, prefix, failure, func(mem member) bool {
		path := join(prefix, mem.name)

		if !m.isLeaf(mem.field) {
			return m.walk(mem.value, path, yield, failure)
		}

		inner, err := m.leafValue(mem)
		if err != nil {
			if *failure == nil {
				*failure = fmt.Errorf("structtree: %s: %w", path, err)
			}

			return true
		}

		// Nothing to store: a nil pointer, which reads as a deletion.
		if !inner.IsValid() {
			return true
		}

		return yield(Leaf{Path: path, Value: inner})
	})
}

// member is one thing a value holds: the name it is addressed by, the field the
// leaf rule is asked about, and the value itself.
type member struct {
	name  string
	field reflect.StructField
	value reflect.Value
	// declared says this is a struct field written out in a type, so a tag
	// could have been put on it. A map's values and a slice's elements are
	// not: the rule is asked about a synthesised field, and advice to tag
	// something has nowhere to go.
	declared bool
}

// hasMembers reports whether v is a value this package descends into rather
// than stores whole. It is the structural half of the leaf rule, asked of a
// value rather than of a field.
func hasMembers(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return true
	default:
		return false
	}
}

// eachMember calls yield for everything v holds, in the order a tree should
// carry it: a struct's exported fields as they are declared, a map's entries by
// sorted key, a slice's or array's elements by index. It reports whether
// iteration should carry on.
//
// A map keyed by anything but a string has no path elements to offer and yields
// nothing; a key that cannot stand as one path element is skipped. Either way
// the reason is recorded in failure, which Build reports.
//
// Having one enumeration is what keeps the walk, the index a [Builder] records
// and the leaf rule itself from disagreeing about what a value holds. They were
// three copies of this, each with its own struct, map and slice arm.
func (m Mapper) eachMember(v reflect.Value, prefix string, failure *error, yield func(member) bool) bool {
	note := func(err error) {
		if *failure == nil {
			*failure = err
		}
	}

	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()

		for i := range t.NumField() {
			field := t.Field(i)

			// Skipped because they cannot be read.
			if !field.IsExported() {
				continue
			}

			if !yield(member{
				name:     field.Name,
				field:    field,
				value:    v.Field(i),
				declared: true,
			}) {
				return false
			}
		}

	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			note(fmt.Errorf("structtree: %s at %q cannot be addressed by path: its keys are %s, not strings",
				v.Type(), prefix, v.Type().Key()))

			return true
		}

		// Sorted, so a walk does not depend on Go's map order.
		keys := make([]string, 0, v.Len())
		for _, key := range v.MapKeys() {
			keys = append(keys, key.String())
		}
		slices.Sort(keys)

		field := reflect.StructField{Type: v.Type().Elem()}

		for _, key := range keys {
			if err := validKey(key); err != nil {
				note(fmt.Errorf("structtree: %s at %q: %w", v.Type(), prefix, err))

				continue
			}

			field.Name = key

			if !yield(member{
				name:  key,
				field: field,
				value: v.MapIndex(reflect.ValueOf(key).Convert(v.Type().Key())),
			}) {
				return false
			}
		}

	case reflect.Slice, reflect.Array:
		field := reflect.StructField{Type: v.Type().Elem()}

		for i := range v.Len() {
			name := strconv.Itoa(i)
			field.Name = name

			if !yield(member{name: name, field: field, value: v.Index(i)}) {
				return false
			}
		}
	}

	return true
}

// join puts a member's name under a prefix, which is empty at the root.
func join(prefix, name string) string {
	if prefix == "" {
		return name
	}

	return prefix + "/" + name
}

// leafValue resolves a leaf to the value its blob is written from, refusing one
// that cannot be stored usefully: a map no path can address, or a value the
// default encoding would write and then be unable to read back again. The three
// rules are distinct — addressability, interface erasure, and what the encoder
// can carry — and none subsumes another; gathering them here is only so that a
// caller has one failure to report rather than three.
//
// An invalid value with a nil error means there is nothing to store at all: a
// nil pointer, which reads as a deletion rather than as a failure.
func (m Mapper) leafValue(mem member) (reflect.Value, error) {
	// Asked before the dereference, so it still fires for a nil map, and
	// only of a field that could have carried the tag it recommends.
	if mem.declared {
		if err := m.unusableMap(mem.field); err != nil {
			return reflect.Value{}, err
		}
	}

	inner, ok := deref(mem.value)
	if !ok {
		return reflect.Value{}, nil
	}

	// Readability is owed to every leaf, however it is addressed: a blob
	// nobody can read back is no better under a map key than under a field
	// name.
	if err := unreadableLeaf(mem.field, inner); err != nil {
		return reflect.Value{}, err
	}

	if err := m.unreadableBlob(mem.field, inner.Type()); err != nil {
		return reflect.Value{}, err
	}

	return inner, nil
}

// validKey reports whether a map key can stand as one path element.
func validKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("a key is empty")
	case strings.Contains(key, "/"):
		return fmt.Errorf("the key %q contains a slash", key)
	}

	return nil
}

// isLeafType is the default rule, expressed over a type: everything that is not
// a struct or a map, plus the structs that are protobuf messages.
func isLeafType(t reflect.Type) bool {
	if isProtoMessage(t) {
		return true
	}

	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Struct:
		return false
	case reflect.Map:
		// Only a map that can be addressed by path becomes a subtree.
		// One that cannot stays a leaf, so that Build can refuse it by
		// name rather than quietly leaving it out.
		return t.Key().Kind() != reflect.String
	case reflect.Slice, reflect.Array:
		// A run of scalars is one blob: splitting a []byte or a
		// []string into a blob apiece would serve nobody. A run of
		// anything with structure becomes a subtree, so that one
		// element of it can change on its own — and so that a protobuf
		// message in one is encoded as a message rather than as
		// whatever JSON makes of its fields, which cannot put a oneof
		// back together.
		return isScalar(t.Elem())
	default:
		return true
	}
}

// isScalar reports whether t is a value with no structure to descend into.
// An interface counts as one: what it holds is not known from the type, so
// there would be no way to read back what was written.
func isScalar(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return false
	default:
		return true
	}
}

// isProtoMessage reports whether t, or a pointer to it, is a protobuf message.
// Generated messages implement the interface on their pointer type, so a field
// declared as a value still counts.
func isProtoMessage(t reflect.Type) bool {
	return t.Implements(protoMessage) ||
		(t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(protoMessage))
}

// deref follows pointers and interfaces to the value underneath, reporting
// false if there is nothing there.
func deref(v reflect.Value) (reflect.Value, bool) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}

	return v, v.IsValid()
}

// Encoder turns a leaf value into the bytes stored in its blob. An encoding
// must be deterministic: a value that has not changed has to produce the same
// bytes, or it will look like a change to everyone downstream.
type Encoder func(reflect.Value) ([]byte, error)

// DefaultEncoder stores protobuf messages as their deterministic wire bytes
// and everything else as JSON.
func DefaultEncoder(v reflect.Value) ([]byte, error) {
	if msg, ok := protoValue(v); ok {
		return proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	}

	return json.Marshal(v.Interface())
}

// protoValue returns v as a proto.Message, taking its address when the field
// is declared as a value rather than a pointer.
func protoValue(v reflect.Value) (proto.Message, bool) {
	if msg, ok := v.Interface().(proto.Message); ok {
		return msg, true
	}

	if !reflect.PointerTo(v.Type()).Implements(protoMessage) {
		return nil, false
	}

	if v.CanAddr() {
		return v.Addr().Interface().(proto.Message), true
	}

	// Not addressable, so copy into somewhere that is.
	addressable := reflect.New(v.Type())
	addressable.Elem().Set(v)

	return addressable.Interface().(proto.Message), true
}

// Build writes v into store as a nested tree and returns the root tree hash,
// using the default rules. It is shorthand for a zero [Mapper]'s Build.
//
// Every leaf is encoded and hashed. [NewBuilder] returns something that redoes
// only what you tell it has changed, which is worth having once a value is
// large enough for the difference to matter.
func Build(store *objects.Store, v any) (plumbing.Hash, error) {
	return Mapper{}.Build(store, v)
}

// Build writes v into store as a nested tree and returns the root tree hash.
func (m Mapper) Build(store *objects.Store, v any) (plumbing.Hash, error) {
	value := reflect.ValueOf(v)

	inner, ok := deref(value)
	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("structtree: value is nil")
	}
	if inner.Kind() != reflect.Struct {
		return plumbing.ZeroHash, fmt.Errorf("structtree: %s is not a struct", inner.Kind())
	}

	root := &node{}

	var failure error

	walked := func(yield func(Leaf) bool) {
		m.walk(reflect.ValueOf(v), "", yield, &failure)
	}

	for leaf := range walked {
		content, err := m.encode(leaf.Value)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("structtree: encode %s: %w", leaf.Path, err)
		}

		hash, err := store.AddBlob(content)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("structtree: store %s: %w", leaf.Path, err)
		}

		root.insertPath(leaf.Path, hash)
	}

	if failure != nil {
		return plumbing.ZeroHash, failure
	}

	return root.store(store)
}

// node is a tree under construction, built from the leaf paths the walk
// yields so that the iterator and the stored tree cannot disagree about the
// shape.
type node struct {
	children map[string]*node
	order    []string
	// entries is what this node was last stored as, kept so that a
	// targeted build can rewrite one hash in it rather than collecting
	// every child again. It is dropped whenever a child comes or goes,
	// since the entries then no longer describe the node.
	entries []object.TreeEntry
	// blob is set on a leaf, tree on a subtree.
	blob plumbing.Hash
	tree plumbing.Hash
}

// insertPath places a blob at a slash-separated path, creating the nodes above
// it, and walks the path in place rather than allocating a slice of its parts
// for every leaf.
func (n *node) insertPath(path string, blob plumbing.Hash) {
	for {
		slash := strings.IndexByte(path, '/')
		if slash < 0 {
			break
		}

		name := path[:slash]

		child, ok := n.children[name]
		if !ok {
			child = &node{children: map[string]*node{}}
			n.put(name, child)
		}

		n = child
		path = path[slash+1:]
	}

	child, ok := n.children[path]
	if !ok {
		// A leaf holds nothing, so it is not given a map to hold it in.
		child = &node{}
		n.put(path, child)
	}

	child.blob = blob
}

// put adds a child under name, remembering the order they arrived in.
func (n *node) put(name string, child *node) {
	if n.children == nil {
		n.children = map[string]*node{}
	}

	n.children[name] = child
	n.order = append(n.order, name)
	n.entries = nil
}

// store writes the node and everything beneath it, returning the tree hash and
// recording it on the node. A Builder reads those hashes back when it rebuilds
// the trees above a change; a plain Build discards the nodes and ignores them.
func (n *node) store(s *objects.Store) (plumbing.Hash, error) {
	entries := make([]object.TreeEntry, 0, len(n.order))

	for _, name := range n.order {
		child := n.children[name]

		if child.blob != plumbing.ZeroHash {
			entries = append(entries, object.TreeEntry{
				Name: name,
				Mode: filemode.Regular,
				Hash: child.blob,
			})

			continue
		}

		hash, err := child.store(s)
		if err != nil {
			return plumbing.ZeroHash, err
		}

		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
	}

	return n.write(s, entries)
}

// write stores the node as the entries given and remembers them, so that a
// later build which moves one child can hand back the same slice with that
// one hash changed.
func (n *node) write(s *objects.Store, entries []object.TreeEntry) (plumbing.Hash, error) {
	hash, err := s.AddTree(entries)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	n.entries = entries
	n.tree = hash

	return hash, nil
}

// moved rewrites the entry for name from the hash its child now holds, and
// reports whether the entries describe this node well enough to do so. A node
// whose children have come or gone has to be collected again.
func (n *node) moved(name string) bool {
	if n.entries == nil {
		return false
	}

	child, ok := n.children[name]
	if !ok {
		return false
	}

	entry := object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: child.tree}
	if child.blob != plumbing.ZeroHash {
		entry = object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: child.blob}
	}

	// Scanned rather than looked up: one pass of string comparisons over
	// the entries replaces a lookup for every one of them, which is what
	// collecting them again would cost.
	for i := range n.entries {
		if n.entries[i].Name != name {
			continue
		}

		if n.entries[i].Mode != entry.Mode {
			// A child that changed kind changes where it sorts, so
			// the entries have to be put in order again.
			return false
		}

		n.entries[i].Hash = entry.Hash

		return true
	}

	return false
}
