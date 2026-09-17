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
// The slice rule is not only about granularity. A leaf that is not itself a
// message but merely contains some is encoded as JSON, and JSON cannot put a
// protobuf oneof back together: it writes the wrapper the generated code uses
// and then has nothing to unmarshal it into. Making such a slice a subtree
// gives each message a blob of its own and the wire encoding it deserves.
//
// An interface is treated as scalar, since what it holds is not known from the
// type. A protobuf message in one is refused rather than written, because
// nothing on the far side would say which message to unmarshal.
//
// That fallback matters most for what it gets wrong. A time.Time is a struct,
// is not a protobuf message, and has only unexported fields — so the walker
// descends into it, finds nothing it may read, and the field vanishes from the
// tree. Tag it and it is stored whole. Any struct of that shape needs the same
// treatment, and the tag is how to give it.
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
// leaf, so it pays for the whole value however little of it moved: on ten
// thousand map entries, fifty thousand leaves in all, some 213ms for a change
// to one field.
//
// A [Builder] pays only for what changed. It keeps the tree it last built and
// looks at nothing but the paths you declare — the same change costs about 5ms,
// most of which is the map's own tree object being written again. The price is
// that it believes you: see [Builder] for what it will not notice, and when to
// hand it the whole value instead.
package structtree

import (
	"encoding/json"
	"fmt"
	"iter"
	"reflect"
	"sort"
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

	if v.Kind() == reflect.Map {
		return m.walkMap(v, prefix, yield, failure)
	}

	if v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
		return m.walkSlice(v, prefix, yield, failure)
	}

	if v.Kind() != reflect.Struct {
		return yield(Leaf{Path: prefix, Value: v})
	}

	t := v.Type()

	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		path := field.Name
		if prefix != "" {
			path = prefix + "/" + field.Name
		}

		value := v.Field(i)

		if m.isLeaf(field) {
			if err := m.unusableMap(field); err != nil {
				if *failure == nil {
					*failure = fmt.Errorf("structtree: %s: %w", path, err)
				}

				continue
			}

			inner, ok := deref(value)
			if !ok {
				continue
			}

			if err := unreadableLeaf(field, inner); err != nil {
				if *failure == nil {
					*failure = fmt.Errorf("structtree: %s: %w", path, err)
				}

				continue
			}

			if !yield(Leaf{Path: path, Value: inner}) {
				return false
			}

			continue
		}

		if !m.walk(value, path, yield, failure) {
			return false
		}
	}

	return true
}

// walkMap yields the leaves of a map, one subtree per key. Keys are sorted, so
// a walk does not depend on Go's map order.
//
// A map value has no struct field of its own, so the leaf rule is asked about a
// synthesised one carrying the key as its name and the map's element type. A
// rule of your own therefore still decides for map values, though a tag cannot:
// there is nowhere to write one.
func (m Mapper) walkMap(v reflect.Value, prefix string, yield func(Leaf) bool, failure *error) bool {
	if v.Type().Key().Kind() != reflect.String {
		if *failure == nil {
			*failure = fmt.Errorf("structtree: %s at %q cannot be addressed by path: its keys are %s, not strings",
				v.Type(), prefix, v.Type().Key())
		}

		return true
	}

	keys := make([]string, 0, v.Len())
	for _, key := range v.MapKeys() {
		keys = append(keys, key.String())
	}
	sort.Strings(keys)

	elem := reflect.StructField{Type: v.Type().Elem()}

	for _, key := range keys {
		if err := validKey(key); err != nil {
			if *failure == nil {
				*failure = fmt.Errorf("structtree: %s at %q: %w", v.Type(), prefix, err)
			}

			continue
		}

		value := v.MapIndex(reflect.ValueOf(key).Convert(v.Type().Key()))

		path := key
		if prefix != "" {
			path = prefix + "/" + key
		}

		elem.Name = key

		if m.isLeaf(elem) {
			inner, ok := deref(value)
			if !ok {
				continue
			}

			if !yield(Leaf{Path: path, Value: inner}) {
				return false
			}

			continue
		}

		if !m.walk(value, path, yield, failure) {
			return false
		}
	}

	return true
}

// walkSlice yields the leaves of a slice or array, one subtree per element,
// named by its index.
//
// An element has no struct field of its own, so the leaf rule is asked about a
// synthesised one carrying the index as its name and the element type, the same
// way a map's values are handled.
func (m Mapper) walkSlice(v reflect.Value, prefix string, yield func(Leaf) bool, failure *error) bool {
	elem := reflect.StructField{Type: v.Type().Elem()}

	for i := range v.Len() {
		name := strconv.Itoa(i)

		path := name
		if prefix != "" {
			path = prefix + "/" + name
		}

		value := v.Index(i)
		elem.Name = name

		if m.isLeaf(elem) {
			inner, ok := deref(value)
			if !ok {
				continue
			}

			if !yield(Leaf{Path: path, Value: inner}) {
				return false
			}

			continue
		}

		if !m.walk(value, path, yield, failure) {
			return false
		}
	}

	return true
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
}

// store writes the node and everything beneath it, returning the tree hash.
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

	return s.AddTree(entries)
}
