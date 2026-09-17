package structtree

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
)

// Decoder fills v, which is settable, from the bytes stored in a leaf's blob.
// It is the inverse of an Encoder, and must be paired with the one that wrote
// the tree.
type Decoder func(data []byte, v reflect.Value) error

// DefaultDecoder reads what DefaultEncoder wrote: protobuf wire bytes into
// protobuf messages, JSON into everything else.
func DefaultDecoder(data []byte, v reflect.Value) error {
	if msg, ok := protoTarget(v); ok {
		return proto.Unmarshal(data, msg)
	}

	return json.Unmarshal(data, v.Addr().Interface())
}

// protoTarget returns v as a protobuf message to decode into, allocating it if
// the field is a nil pointer.
func protoTarget(v reflect.Value) (proto.Message, bool) {
	t := v.Type()

	if t.Implements(protoMessage) {
		if v.IsNil() {
			v.Set(reflect.New(t.Elem()))
		}

		return v.Interface().(proto.Message), true
	}

	if reflect.PointerTo(t).Implements(protoMessage) {
		return v.Addr().Interface().(proto.Message), true
	}

	return nil, false
}

// Apply fills dst from leaves, using the default rules. It is shorthand for a
// zero [Mapper]'s Apply.
func Apply(dst any, leaves map[string][]byte) error {
	return Mapper{}.Apply(dst, leaves)
}

// Apply fills dst from leaves, which maps the paths [Mapper.Walk] yields to the
// bytes stored at them — exactly what Graph.Leaves returns on the receiving
// side.
//
// Every leaf is decoded. [Mapper.ApplySince] does the same job incrementally,
// decoding only what moved, when the baseline the value is already at is known.
//
// The tree is the source of truth, so applying it settles every field dst has:
// a field whose path the tree does not carry is zeroed, and a pointer to a
// struct with no paths beneath it is set to nil. Applying the same leaves
// repeatedly therefore leaves dst in the same state, and a value that stops
// being sent is cleared rather than left stale.
//
// Paths that dst has no field for are ignored, so a server may add fields
// before its clients know about them. A client that wants to notice them can
// compare the keys of leaves against the paths [Walk] yields for its own type.
//
// dst must be a non-nil pointer to a struct.
func (m Mapper) Apply(dst any, leaves map[string][]byte) error {
	v, err := destination(dst)
	if err != nil {
		return err
	}

	return m.apply(v, "", mapView{leaves: leaves, subtrees: subtrees(leaves)})
}

// destination checks that dst is something whose fields can be written.
func destination(dst any) (reflect.Value, error) {
	v := reflect.ValueOf(dst)

	if v.Kind() != reflect.Pointer || v.IsNil() {
		return reflect.Value{}, fmt.Errorf("structtree: destination must be a non-nil pointer to a struct")
	}

	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("structtree: destination points at %s, not a struct", v.Kind())
	}

	return v, nil
}

// state says what a view holds for one field.
type state int

const (
	// missing means the view does not carry the field at all, so it is
	// cleared.
	missing state = iota
	// changed means the field has to be read.
	changed
	// unchanged means the field is identical to the baseline and can be
	// left as it is. Only a view with a baseline ever reports it.
	unchanged
)

// view is one node of whatever a value is being applied from: a flat map of
// leaves, or a pair of trees being compared. Having both behind one interface
// is what keeps a single copy of the rules about which fields to visit, what
// counts as a leaf, and when to clear or allocate.
type view interface {
	// leaf returns the bytes stored for a named leaf field.
	leaf(name string) ([]byte, state, error)
	// subtree returns the view for a named struct field.
	subtree(name string) (view, state, error)
	// children names what the view holds at this level. A struct's fields
	// come from its type, but a map's keys can only come from the tree.
	children() ([]string, error)
}

// apply writes the fields of v from vw. prefix is carried for error messages
// only; each view resolves names itself.
func (m Mapper) apply(v reflect.Value, prefix string, vw view) error {
	// Reaching NumField with anything but a struct takes the process down,
	// which is how a missing dereference in one caller announced itself
	// once already. Say it instead.
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("structtree: %s at %q is not a struct", v.Type(), prefix)
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

		target := v.Field(i)

		if m.isLeaf(field) {
			if err := m.unusableMap(field); err != nil {
				return fmt.Errorf("structtree: %s: %w", path, err)
			}

			if err := m.applyLeaf(target, path, vw); err != nil {
				return err
			}

			continue
		}

		sub, st, err := vw.subtree(field.Name)
		if err != nil {
			return fmt.Errorf("structtree: %s: %w", path, err)
		}

		switch st {
		case unchanged:
			continue
		case missing:
			// Nothing beneath this path any more.
			target.SetZero()

			continue
		}

		if err := m.applyInto(target, path, sub); err != nil {
			return err
		}
	}

	return nil
}

// applyLeaf decodes one leaf into its field, clearing the field first so that
// what the view carries is all that ends up there.
func (m Mapper) applyLeaf(target reflect.Value, path string, vw view) error {
	name := path
	if i := lastSlash(path); i >= 0 {
		name = path[i+1:]
	}

	data, st, err := vw.leaf(name)
	if err != nil {
		return fmt.Errorf("structtree: %s: %w", path, err)
	}

	if st == unchanged {
		return nil
	}

	target.SetZero()

	if st == missing {
		return nil
	}

	if err := m.decode(data, target); err != nil {
		return fmt.Errorf("structtree: decode %s: %w", path, err)
	}

	return nil
}

func lastSlash(path string) int {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return i
		}
	}

	return -1
}

// mapView reads from a flat map of leaf paths. It has no baseline, so it never
// reports a field as unchanged.
type mapView struct {
	leaves   map[string][]byte
	subtrees map[string]bool
	prefix   string
}

func (m mapView) path(name string) string {
	if m.prefix == "" {
		return name
	}

	return m.prefix + "/" + name
}

func (m mapView) leaf(name string) ([]byte, state, error) {
	data, ok := m.leaves[m.path(name)]
	if !ok {
		return nil, missing, nil
	}

	return data, changed, nil
}

func (m mapView) subtree(name string) (view, state, error) {
	path := m.path(name)

	if !m.subtrees[path] {
		return nil, missing, nil
	}

	return mapView{leaves: m.leaves, subtrees: m.subtrees, prefix: path}, changed, nil
}

// subtrees collects every path prefix the leaves sit under, so a node can be
// told apart from one the tree no longer carries at all.
func subtrees(leaves map[string][]byte) map[string]bool {
	out := make(map[string]bool, len(leaves))

	for path := range leaves {
		for i, r := range path {
			if r == '/' {
				out[path[:i]] = true
			}
		}
	}

	return out
}

// applyMap fills a map from a subtree, one entry per child. The map is edited
// in place rather than rebuilt, so an entry the view reports as unchanged keeps
// whatever it already holds — which is what lets ApplySince skip work here too.
// Keys the view no longer carries are deleted, since the tree is the source of
// truth.
func (m Mapper) applyMap(target reflect.Value, path string, vw view) error {
	t := target.Type()

	if t.Key().Kind() != reflect.String {
		return fmt.Errorf("structtree: %s at %q cannot be addressed by path: its keys are %s, not strings",
			t, path, t.Key())
	}

	names, err := vw.children()
	if err != nil {
		return fmt.Errorf("structtree: %s: %w", path, err)
	}

	if target.IsNil() {
		target.Set(reflect.MakeMapWithSize(t, len(names)))
	}

	elem := reflect.StructField{Type: t.Elem()}
	seen := make(map[string]bool, len(names))

	for _, name := range names {
		seen[name] = true

		key := reflect.ValueOf(name).Convert(t.Key())
		elem.Name = name

		if m.isLeaf(elem) {
			data, st, err := vw.leaf(name)
			if err != nil {
				return fmt.Errorf("structtree: %s/%s: %w", path, name, err)
			}

			switch st {
			case unchanged:
				continue
			case missing:
				target.SetMapIndex(key, reflect.Value{})

				continue
			}

			value := reflect.New(t.Elem()).Elem()
			if err := m.decode(data, value); err != nil {
				return fmt.Errorf("structtree: decode %s/%s: %w", path, name, err)
			}

			target.SetMapIndex(key, value)

			continue
		}

		sub, st, err := vw.subtree(name)
		if err != nil {
			return fmt.Errorf("structtree: %s/%s: %w", path, name, err)
		}

		switch st {
		case unchanged:
			continue
		case missing:
			target.SetMapIndex(key, reflect.Value{})

			continue
		}

		// Start from what is there, so parts of an entry the view calls
		// unchanged survive a change elsewhere in it.
		value := reflect.New(t.Elem()).Elem()
		if existing := target.MapIndex(key); existing.IsValid() {
			value.Set(existing)
		}

		if err := m.applyInto(value, path+"/"+name, sub); err != nil {
			return err
		}

		target.SetMapIndex(key, value)
	}

	for _, key := range target.MapKeys() {
		if !seen[key.String()] {
			target.SetMapIndex(key, reflect.Value{})
		}
	}

	return nil
}

// children implements view.
func (m mapView) children() ([]string, error) {
	seen := map[string]bool{}

	collect := func(path string) {
		if m.prefix != "" {
			if !strings.HasPrefix(path, m.prefix+"/") {
				return
			}
			path = path[len(m.prefix)+1:]
		}

		if i := strings.IndexByte(path, '/'); i >= 0 {
			path = path[:i]
		}

		if path != "" {
			seen[path] = true
		}
	}

	for path := range m.leaves {
		collect(path)
	}
	for path := range m.subtrees {
		collect(path)
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)

	return out, nil
}

// applyInto fills v from vw, whatever shape v is, allocating through pointers
// on the way. v must be settable.
//
// Having this in one place is the point: a map of pointers used to reach the
// struct case with the pointer still in hand, and panic on it.
func (m Mapper) applyInto(v reflect.Value, path string, vw view) error {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}

		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Struct:
		return m.apply(v, path, vw)
	case reflect.Map:
		return m.applyMap(v, path, vw)
	case reflect.Slice, reflect.Array:
		return m.applySlice(v, path, vw)
	default:
		return fmt.Errorf("structtree: %s at %q has no fields to fill", v.Type(), path)
	}
}

// applySlice fills a slice or array from a subtree, one element per child.
//
// Children are named by index, and tree entries sort as text, so the indices
// are read as numbers: otherwise ten elements in and the order would come back
// scrambled. They must run from zero without a gap, since a slice has no way to
// hold one.
func (m Mapper) applySlice(target reflect.Value, path string, vw view) error {
	names, err := vw.children()
	if err != nil {
		return fmt.Errorf("structtree: %s: %w", path, err)
	}

	indices := make([]int, 0, len(names))

	for _, name := range names {
		i, err := strconv.Atoi(name)
		if err != nil || i < 0 {
			return fmt.Errorf("structtree: %s/%s is not an index", path, name)
		}

		indices = append(indices, i)
	}

	sort.Ints(indices)

	for i, index := range indices {
		if index != i {
			return fmt.Errorf("structtree: %s has no element %d", path, i)
		}
	}

	n := len(indices)

	switch target.Kind() {
	case reflect.Slice:
		if target.Len() != n {
			// Copy what is there, so elements the view calls
			// unchanged keep what they hold.
			fresh := reflect.MakeSlice(target.Type(), n, n)
			reflect.Copy(fresh, target)
			target.Set(fresh)
		}
	default:
		if n > target.Len() {
			return fmt.Errorf("structtree: %s has %d elements, more than %s holds",
				path, n, target.Type())
		}
	}

	elem := reflect.StructField{Type: target.Type().Elem()}

	for i := range n {
		name := strconv.Itoa(i)
		elem.Name = name

		item := target.Index(i)

		if m.isLeaf(elem) {
			data, st, err := vw.leaf(name)
			if err != nil {
				return fmt.Errorf("structtree: %s/%s: %w", path, name, err)
			}

			switch st {
			case unchanged:
				continue
			case missing:
				item.SetZero()

				continue
			}

			item.SetZero()

			if err := m.decode(data, item); err != nil {
				return fmt.Errorf("structtree: decode %s/%s: %w", path, name, err)
			}

			continue
		}

		sub, st, err := vw.subtree(name)
		if err != nil {
			return fmt.Errorf("structtree: %s/%s: %w", path, name, err)
		}

		switch st {
		case unchanged:
			continue
		case missing:
			item.SetZero()

			continue
		}

		if err := m.applyInto(item, path+"/"+name, sub); err != nil {
			return err
		}
	}

	// An array keeps its length, so anything past the end is cleared.
	for i := n; i < target.Len(); i++ {
		target.Index(i).SetZero()
	}

	return nil
}
