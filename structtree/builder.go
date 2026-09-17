package structtree

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/zonque/treevial/objects"
)

// Builder rebuilds a value's tree without redoing the parts that did not
// change.
//
// [Mapper.Build] encodes and hashes every leaf, which is what it costs to learn
// a hash it may already have known. A Builder keeps the tree it last built and
// touches only what you tell it has moved: one field of a value with fifty
// thousand leaves in it costs a handful of objects and a traversal of nothing
// but the path to them.
//
// Say what changed by passing a pointer to it. A pointer stands for everything
// beneath it, so the same rule covers one field, a subtree, an entry of a map,
// and the whole value:
//
//	b, err := structtree.NewBuilder(store, cfg)
//	root, err := b.Build()                          // everything, the first time
//
//	cfg.Network.Primary.MTU = 9000
//	root, err = b.Build(&cfg.Network.Primary.MTU)   // that leaf and its path
//	root, err = b.Build(&cfg.Network)               // everything under Network
//	root, err = b.Build(cfg.Ports["eth0"])          // one entry of a map
//	root, err = b.Build(cfg)                        // everything
//	root, err = b.Build()                           // everything
//
// # What it will not notice
//
// Only what you declare, and what lies beneath it, is looked at. A value
// changed elsewhere keeps the hash it had, and so does a member added or
// removed elsewhere: adding a key to a map changes that map's shape, and a
// declaration naming something else cannot know about it.
//
// Declare the thing that changed shape — the map, the struct — and its whole
// subtree is walked afresh, which picks up members coming and going within it.
// Or call Build with no arguments, which walks everything and is always right.
//
// # Declaring by pointer
//
// A pointer is resolved against an index of addresses the last build recorded,
// costing one lookup rather than a search, and then checked by descending the
// path it names to confirm the address is still that field's. A pointer the
// last build never saw is refused: attributing it to whatever used to live at
// that address would be worse than saying so.
//
// The index holds a couple of entries per node, so it is some megabytes for a
// value with tens of thousands of leaves. It holds no copy of the data.
//
// A map of values hands out copies, so its entries have no address to take —
// Go will not let a field inside one be assigned to either. Keep pointers in a
// map whose entries you mean to change one at a time.
//
// A Builder is not safe for concurrent use.
type Builder struct {
	mapper Mapper
	store  *objects.Store
	value  any

	// root is the tree as last built, each node holding its hash.
	root *node
	// index says which path a declared pointer refers to, and fields says
	// what sort of thing lives at a path, so the leaf rule can be asked
	// about it without walking to find it.
	index  map[located]string
	fields map[string]reflect.StructField
}

// located is where a declaration points: an address and the type of what sits
// there. The type is needed because a struct and its first field share an
// address, so &cfg.Device and &cfg.Device.Name cannot be told apart by address
// alone.
type located struct {
	addr uintptr
	typ  reflect.Type
}

// NewBuilder returns a Builder over v, using the default rules. v must be a
// non-nil pointer to a struct, because a Builder works in terms of the
// addresses of its fields.
func NewBuilder(store *objects.Store, v any) (*Builder, error) {
	return Mapper{}.NewBuilder(store, v)
}

// NewBuilder returns a Builder over v.
func (m Mapper) NewBuilder(store *objects.Store, v any) (*Builder, error) {
	value := reflect.ValueOf(v)

	if value.Kind() != reflect.Pointer || value.IsNil() {
		return nil, fmt.Errorf("structtree: a builder needs a non-nil pointer to a struct")
	}
	if value.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("structtree: a builder needs a pointer to a struct, not to %s",
			value.Elem().Kind())
	}

	return &Builder{mapper: m, store: store, value: v}, nil
}

// Build stores the value's tree and returns the root hash, redoing the parts
// reached by the given pointers and leaving the rest as it was. With no
// pointers it redoes everything, which is always correct.
//
// Each pointer must address a field the last build saw, or the value itself;
// anything else is an error. Nothing outside the declared paths is encoded,
// hashed or even traversed, so a member added or removed outside them is not
// noticed either — see [Builder] for what that means and what to declare
// instead.
func (b *Builder) Build(changed ...any) (plumbing.Hash, error) {
	if len(changed) == 0 || b.root == nil {
		return b.buildAll()
	}

	declared, err := b.resolve(changed)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	for _, path := range declared {
		// The whole value was declared, so there is nothing to spare.
		if path == "" {
			return b.buildAll()
		}
	}

	for _, path := range declared {
		if err := b.refresh(path); err != nil {
			return plumbing.ZeroHash, err
		}
	}

	for _, path := range declared {
		if err := b.rehash(path); err != nil {
			return plumbing.ZeroHash, err
		}
	}

	return b.root.tree, nil
}

// buildAll walks the whole value, stores every object, and records the index a
// later targeted build resolves against.
func (b *Builder) buildAll() (plumbing.Hash, error) {
	root := &node{}

	var failure error

	walked := func(yield func(Leaf) bool) {
		b.mapper.walk(reflect.ValueOf(b.value), "", yield, &failure)
	}

	for leaf := range walked {
		hash, err := b.blob(leaf)
		if err != nil {
			return plumbing.ZeroHash, err
		}

		root.insertPath(leaf.Path, hash)
	}

	if failure != nil {
		return plumbing.ZeroHash, failure
	}

	b.root = root
	b.index = map[located]string{}
	b.fields = map[string]reflect.StructField{}
	b.indexValue(reflect.ValueOf(b.value), "")

	return b.storeSubtree(root, "")
}

// blob encodes a leaf and stores it.
func (b *Builder) blob(leaf Leaf) (plumbing.Hash, error) {
	content, err := b.mapper.encode(leaf.Value)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("structtree: encode %s: %w", leaf.Path, err)
	}

	hash, err := b.store.AddBlob(content)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("structtree: store %s: %w", leaf.Path, err)
	}

	return hash, nil
}

// refresh redoes the subtree at path from the value as it stands now.
func (b *Builder) refresh(path string) error {
	value, ok := b.descend(path)
	if !ok {
		// Whatever was there has gone, which is a change of shape.
		b.detach(path)

		return nil
	}

	field := b.fields[path]

	if b.mapper.isLeaf(field) {
		inner, ok := deref(value)
		if !ok {
			b.detach(path)

			return nil
		}

		hash, err := b.blob(Leaf{Path: path, Value: inner})
		if err != nil {
			return err
		}

		target, ok := b.nodeAt(path)
		if !ok {
			return fmt.Errorf("structtree: %s is not in the tree", path)
		}

		target.blob = hash
		target.children = nil
		target.order = nil

		return nil
	}

	fresh := &node{}

	var failure error

	walked := func(yield func(Leaf) bool) {
		b.mapper.walk(value, "", yield, &failure)
	}

	for leaf := range walked {
		// The walk names leaves relative to what it was given; an empty
		// name is the value itself, which cannot happen for a subtree.
		hash, err := b.blob(Leaf{Path: path + "/" + leaf.Path, Value: leaf.Value})
		if err != nil {
			return err
		}

		fresh.insertPath(leaf.Path, hash)
	}

	if failure != nil {
		return failure
	}

	if err := b.attach(path, fresh); err != nil {
		return err
	}

	if _, err := b.storeSubtree(fresh, path); err != nil {
		return err
	}

	// The subtree may have changed shape, so what is where has to be
	// recorded again.
	b.indexValue(value, path)

	return nil
}

// storeSubtree writes a node and everything beneath it, recording each hash on
// the node that owns it.
func (b *Builder) storeSubtree(n *node, path string) (plumbing.Hash, error) {
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

		childPath := name
		if path != "" {
			childPath = path + "/" + name
		}

		hash, err := b.storeSubtree(child, childPath)
		if err != nil {
			return plumbing.ZeroHash, err
		}

		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
	}

	hash, err := b.store.AddTree(entries)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	n.tree = hash

	return hash, nil
}

// rehash recomputes the tree objects from path's parent up to the root, which
// is all that a change at path can have moved.
func (b *Builder) rehash(path string) error {
	chain, names := b.chain(path)

	for i := len(chain) - 1; i >= 0; i-- {
		if err := b.restore(chain[i], strings.Join(names[:i], "/")); err != nil {
			return err
		}
	}

	return nil
}

// restore stores one node from the hashes its children already hold.
func (b *Builder) restore(n *node, path string) error {
	entries := make([]object.TreeEntry, 0, len(n.order))

	for _, name := range n.order {
		child := n.children[name]

		entry := object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: child.blob}
		if child.blob == plumbing.ZeroHash {
			entry = object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: child.tree}
		}

		entries = append(entries, entry)
	}

	hash, err := b.store.AddTree(entries)
	if err != nil {
		return err
	}

	n.tree = hash

	return nil
}

// chain returns the nodes from the root down to path's parent, and the names
// making up path.
func (b *Builder) chain(path string) ([]*node, []string) {
	names := strings.Split(path, "/")

	chain := []*node{b.root}
	n := b.root

	for _, name := range names[:len(names)-1] {
		child, ok := n.children[name]
		if !ok {
			break
		}

		chain = append(chain, child)
		n = child
	}

	return chain, names
}

// nodeAt returns the node at path.
func (b *Builder) nodeAt(path string) (*node, bool) {
	n := b.root

	for _, name := range strings.Split(path, "/") {
		child, ok := n.children[name]
		if !ok {
			return nil, false
		}

		n = child
	}

	return n, true
}

// attach puts fresh in place of whatever was at path.
func (b *Builder) attach(path string, fresh *node) error {
	chain, names := b.chain(path)

	parent := chain[len(chain)-1]
	if len(chain) != len(names) {
		return fmt.Errorf("structtree: %s is not in the tree", path)
	}

	name := names[len(names)-1]

	if _, ok := parent.children[name]; !ok {
		parent.put(name, fresh)

		return nil
	}

	parent.children[name] = fresh

	return nil
}

// detach removes whatever is at path.
func (b *Builder) detach(path string) {
	chain, names := b.chain(path)
	if len(chain) != len(names) {
		return
	}

	parent := chain[len(chain)-1]
	name := names[len(names)-1]

	if _, ok := parent.children[name]; !ok {
		return
	}

	delete(parent.children, name)

	for i, have := range parent.order {
		if have == name {
			parent.order = append(parent.order[:i], parent.order[i+1:]...)

			break
		}
	}
}

// resolve turns the declared pointers into the paths they name.
func (b *Builder) resolve(changed []any) ([]string, error) {
	out := make([]string, 0, len(changed))

	for _, c := range changed {
		v := reflect.ValueOf(c)
		if v.Kind() != reflect.Pointer || v.IsNil() {
			return nil, fmt.Errorf("structtree: %T is not a pointer into the value", c)
		}

		key := located{addr: v.Pointer(), typ: v.Type().Elem()}

		path, ok := b.index[key]
		if !ok {
			return nil, fmt.Errorf("structtree: %s addresses nothing the last build saw", v.Type())
		}

		// The index was recorded then and this is now, so confirm the
		// address is still that field's rather than trusting it.
		if !b.stillThere(path, key) {
			return nil, fmt.Errorf("structtree: %s no longer addresses %s", v.Type(), path)
		}

		out = append(out, path)
	}

	return out, nil
}

// stillThere reports whether key really is the address of what lives at path.
func (b *Builder) stillThere(path string, key located) bool {
	if path == "" {
		v := reflect.ValueOf(b.value)

		return v.Pointer() == key.addr && v.Type().Elem() == key.typ
	}

	v, ok := b.descend(path)
	if !ok {
		return false
	}

	if v.CanAddr() && v.Addr().Pointer() == key.addr && v.Type() == key.typ {
		return true
	}

	return v.Kind() == reflect.Pointer && !v.IsNil() &&
		v.Pointer() == key.addr && v.Type().Elem() == key.typ
}

// descend returns the value at path, following pointers and map keys as the
// path requires.
func (b *Builder) descend(path string) (reflect.Value, bool) {
	v := reflect.ValueOf(b.value)

	if path == "" {
		return v, true
	}

	for _, name := range strings.Split(path, "/") {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}

		switch v.Kind() {
		case reflect.Struct:
			field := v.FieldByName(name)
			if !field.IsValid() {
				return reflect.Value{}, false
			}
			v = field

		case reflect.Map:
			if v.IsNil() {
				return reflect.Value{}, false
			}

			entry := v.MapIndex(reflect.ValueOf(name).Convert(v.Type().Key()))
			if !entry.IsValid() {
				return reflect.Value{}, false
			}
			v = entry

		case reflect.Slice, reflect.Array:
			i, err := strconv.Atoi(name)
			if err != nil || i < 0 || i >= v.Len() {
				return reflect.Value{}, false
			}
			v = v.Index(i)

		default:
			return reflect.Value{}, false
		}
	}

	return v, true
}

// indexValue records where everything beneath v lives, so a later declaration
// can be resolved by lookup.
func (b *Builder) indexValue(v reflect.Value, path string) {
	b.record(v, path, reflect.StructField{Type: v.Type()})

	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()

		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}

			childPath := field.Name
			if path != "" {
				childPath = path + "/" + field.Name
			}

			b.fields[childPath] = field

			value := v.Field(i)

			if b.mapper.isLeaf(field) {
				b.record(value, childPath, field)

				continue
			}

			b.indexValue(value, childPath)
		}

	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return
		}

		elem := reflect.StructField{Type: v.Type().Elem()}

		for _, key := range v.MapKeys() {
			name := key.String()
			if validKey(name) != nil {
				continue
			}

			childPath := path + "/" + name
			elem.Name = name

			b.fields[childPath] = elem

			value := v.MapIndex(key)

			if b.mapper.isLeaf(elem) {
				b.record(value, childPath, elem)

				continue
			}

			b.indexValue(value, childPath)
		}

	case reflect.Slice, reflect.Array:
		elem := reflect.StructField{Type: v.Type().Elem()}

		for i := range v.Len() {
			name := strconv.Itoa(i)

			childPath := path + "/" + name
			elem.Name = name

			b.fields[childPath] = elem

			// Unlike a map entry, a slice element has an address of
			// its own, so it can be declared directly.
			value := v.Index(i)

			if b.mapper.isLeaf(elem) {
				b.record(value, childPath, elem)

				continue
			}

			b.indexValue(value, childPath)
		}
	}
}

// record notes the addresses by which a value may be declared: its own, and
// what it points at, so both spellings of a pointer work.
func (b *Builder) record(v reflect.Value, path string, field reflect.StructField) {
	if v.CanAddr() {
		b.index[located{addr: v.Addr().Pointer(), typ: v.Type()}] = path
	}

	if v.Kind() == reflect.Pointer && !v.IsNil() {
		b.index[located{addr: v.Pointer(), typ: v.Type().Elem()}] = path
	}

	if _, ok := b.fields[path]; !ok {
		b.fields[path] = field
	}
}
