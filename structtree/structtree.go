// Package structtree maps a Go struct onto a git tree.
//
// The hierarchy of the struct becomes the hierarchy of the tree: each field
// name is a path element, joined with "/", so a value at cfg.Network.Primary.MTU
// is stored as the blob "Network/Primary/MTU". That is what lets the rest of
// gats work unchanged — the same path handling, the same subtree pruning, the
// same diffs — while the thing being synchronised is an ordinary Go value.
//
// A field is a leaf if it is not a struct, or if it is a struct that
// implements proto.Message. Everything else is descended into. So a slice, a
// map and an int are each stored whole in one blob, a generated protobuf
// message is stored as one blob of its own wire bytes, and a plain nested
// struct becomes a subtree.
//
// Because a leaf's blob changes only when that leaf's bytes change, and a
// subtree's hash changes only when something beneath it changes, updating one
// deep field costs one blob plus the trees on its path — however large the
// rest of the struct is.
package structtree

import (
	"encoding/json"
	"fmt"
	"iter"
	"reflect"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"google.golang.org/protobuf/proto"

	"github.com/holoplot/gats/objects"
)

// protoMessage is the interface a struct must implement to be treated as a
// leaf rather than descended into.
var protoMessage = reflect.TypeOf((*proto.Message)(nil)).Elem()

// Leaf is one value of a walked struct, together with the path its blob is
// stored at.
type Leaf struct {
	// Path is slash-separated and mirrors the field names leading to the
	// value, for example "Network/Primary/MTU".
	Path string
	// Value is the field's value.
	Value reflect.Value
}

// Walk returns an iterator over the leaves of v, in the order the fields are
// declared, descending depth-first.
//
// v may be a struct or a pointer to one. Unexported fields are skipped,
// because they cannot be read, and nil pointers are skipped, because there is
// nothing to store; a field that becomes nil therefore reads as a deletion and
// one that stops being nil as an addition.
func Walk(v any) iter.Seq[Leaf] {
	return func(yield func(Leaf) bool) {
		root := reflect.ValueOf(v)
		if !root.IsValid() {
			return
		}

		walk(root, "", yield)
	}
}

// walk yields the leaves of v under prefix, reporting whether iteration should
// carry on.
func walk(v reflect.Value, prefix string, yield func(Leaf) bool) bool {
	v, ok := deref(v)
	if !ok {
		return true
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

		if isLeaf(field.Type) {
			inner, ok := deref(value)
			if !ok {
				continue
			}

			if !yield(Leaf{Path: path, Value: inner}) {
				return false
			}

			continue
		}

		if !walk(value, path, yield) {
			return false
		}
	}

	return true
}

// isLeaf reports whether a field of type t is stored as one blob rather than
// descended into: everything that is not a struct, plus the structs that are
// protobuf messages.
func isLeaf(t reflect.Type) bool {
	if isProtoMessage(t) {
		return true
	}

	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t.Kind() != reflect.Struct
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
// using DefaultEncoder.
func Build(store *objects.Store, v any) (plumbing.Hash, error) {
	return BuildWith(store, v, DefaultEncoder)
}

// BuildWith is Build with a chosen encoding for leaf values.
func BuildWith(store *objects.Store, v any, encode Encoder) (plumbing.Hash, error) {
	value := reflect.ValueOf(v)

	inner, ok := deref(value)
	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("structtree: value is nil")
	}
	if inner.Kind() != reflect.Struct {
		return plumbing.ZeroHash, fmt.Errorf("structtree: %s is not a struct", inner.Kind())
	}

	root := &node{children: map[string]*node{}}

	for leaf := range Walk(v) {
		content, err := encode(leaf.Value)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("structtree: encode %s: %w", leaf.Path, err)
		}

		hash, err := store.AddBlob(content)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("structtree: store %s: %w", leaf.Path, err)
		}

		root.insert(strings.Split(leaf.Path, "/"), hash)
	}

	return root.store(store)
}

// node is a tree under construction, built from the leaf paths the walk
// yields so that the iterator and the stored tree cannot disagree about the
// shape.
type node struct {
	children map[string]*node
	order    []string
	blob     plumbing.Hash
}

func (n *node) insert(path []string, blob plumbing.Hash) {
	name := path[0]

	child, ok := n.children[name]
	if !ok {
		child = &node{children: map[string]*node{}}
		n.children[name] = child
		n.order = append(n.order, name)
	}

	if len(path) == 1 {
		child.blob = blob

		return
	}

	child.insert(path[1:], blob)
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
