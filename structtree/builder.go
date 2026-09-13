package structtree

import (
	"fmt"
	"reflect"
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
// a hash it may already have known. A Builder remembers the hashes from its
// last build and reuses them, so only what was declared is encoded and hashed
// again. On a value of some 97 MiB across ten thousand leaves, measured here, a
// full build takes about 270ms and a one-field build about 7ms.
//
// What remains is the walk: the value is traversed in full every time, which
// costs about a millisecond at that size and is why a field that has come into
// existence since the last build is picked up whether or not it was declared.
// A Builder holds one hash per node, not a copy of the data.
//
// Say what changed by passing a pointer to it. A pointer stands for everything
// beneath it, so the same rule covers one field, a subtree, and the whole
// value:
//
//	b, err := structtree.NewBuilder(store, cfg)
//	root, err := b.Build()                        // everything, the first time
//
//	cfg.Network.Primary.MTU = 9000
//	root, err = b.Build(&cfg.Network.Primary.MTU) // that leaf and its path
//	root, err = b.Build(&cfg.Network)             // everything under Network
//	root, err = b.Build(cfg)                      // everything
//	root, err = b.Build()                         // everything
//
// A Builder is not safe for concurrent use.
type Builder struct {
	mapper Mapper
	store  *objects.Store
	value  any

	// cached is the tree as it was last built, keyed by path, holding the
	// hash of each leaf and of each subtree.
	cached map[string]plumbing.Hash
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

	return &Builder{mapper: m, store: store, value: v, cached: map[string]plumbing.Hash{}}, nil
}

// Build stores the value's tree and returns the root hash, recomputing the
// parts reached by the given pointers and reusing the hashes it already has for
// everything else. With no pointers it recomputes everything.
//
// Each pointer must address a field of the value, or the value itself;
// anything else is an error, because quietly ignoring it would publish a tree
// missing the change it was meant to carry.
//
// A change that is neither declared nor beneath something declared is not
// picked up — that is the bargain. A field that has come into existence since
// the last build is picked up regardless, since there is no hash on file to
// reuse for it.
func (b *Builder) Build(changed ...any) (plumbing.Hash, error) {
	dirty, err := b.dirty(changed)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	root := &node{children: map[string]*node{}}

	for leaf := range b.mapper.Walk(b.value) {
		hash, known := b.cached[leaf.Path]

		if !known || dirty.covers(leaf.Path) {
			content, err := b.mapper.encode(leaf.Value)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("structtree: encode %s: %w", leaf.Path, err)
			}

			if hash, err = b.store.AddBlob(content); err != nil {
				return plumbing.ZeroHash, fmt.Errorf("structtree: store %s: %w", leaf.Path, err)
			}
		}

		root.insertPath(leaf.Path, hash)
	}

	fresh := make(map[string]plumbing.Hash, len(b.cached))

	hash, err := b.storeNode(root, "", fresh)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	// Paths that have gone are dropped with the old map rather than left to
	// be reused by something that takes their name later.
	b.cached = fresh

	return hash, nil
}

// storeNode writes a node and everything beneath it, reusing the tree hash it
// had last time when its entries are unchanged, and recording what it wrote.
func (b *Builder) storeNode(n *node, path string, fresh map[string]plumbing.Hash) (plumbing.Hash, error) {
	entries := make([]object.TreeEntry, 0, len(n.order))

	for _, name := range n.order {
		child := n.children[name]

		childPath := name
		if path != "" {
			childPath = path + "/" + name
		}

		if child.blob != plumbing.ZeroHash {
			fresh[childPath] = child.blob
			entries = append(entries, object.TreeEntry{
				Name: name,
				Mode: filemode.Regular,
				Hash: child.blob,
			})

			continue
		}

		hash, err := b.storeNode(child, childPath, fresh)
		if err != nil {
			return plumbing.ZeroHash, err
		}

		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
	}

	hash, err := b.store.AddTree(entries)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	fresh[path] = hash

	return hash, nil
}

// paths is a set of declared paths. The empty path stands for the whole value.
type paths map[string]bool

// covers reports whether path is one of the declared paths or lies beneath one.
func (p paths) covers(path string) bool {
	if p[""] {
		return true
	}

	for declared := range p {
		if path == declared || strings.HasPrefix(path, declared+"/") {
			return true
		}
	}

	return false
}

// target is a pointer the caller declared: where it points, and the type of
// what it points at. The type is needed because a struct and its first field
// share an address, so &cfg.Device and &cfg.Device.Name cannot be told apart by
// address alone.
type target struct {
	addr uintptr
	typ  reflect.Type
}

// dirty resolves the pointers to the paths they address. No pointers means the
// whole value.
func (b *Builder) dirty(changed []any) (paths, error) {
	if len(changed) == 0 {
		return paths{"": true}, nil
	}

	wanted := make([]target, len(changed))

	for i, c := range changed {
		v := reflect.ValueOf(c)
		if v.Kind() != reflect.Pointer || v.IsNil() {
			return nil, fmt.Errorf("structtree: %T is not a pointer into the value", c)
		}
		wanted[i] = target{addr: v.Pointer(), typ: v.Type().Elem()}
	}

	var (
		found   = paths{}
		matched = make([]bool, len(wanted))
	)

	b.locate(reflect.ValueOf(b.value), "", wanted, matched, found)

	for i, ok := range matched {
		if !ok {
			return nil, fmt.Errorf("structtree: %s addresses no field of the value",
				reflect.PointerTo(wanted[i].typ))
		}
	}

	return found, nil
}

// mark records path as declared if v is one of the addresses asked about,
// following a pointer so that both spellings of a pointer field work: the
// address of the field, and the pointer it holds.
func mark(v reflect.Value, path string, wanted []target, matched []bool, found paths) {
	for step := 0; step < 2; step++ {
		if v.CanAddr() {
			for i, w := range wanted {
				if w.addr == v.Addr().Pointer() && w.typ == v.Type() {
					matched[i] = true
					found[path] = true
				}
			}
		}

		if v.Kind() != reflect.Pointer || v.IsNil() {
			return
		}

		v = v.Elem()
	}
}

// locate walks the value looking for the addresses it was asked about,
// recording the path of each field that matches.
func (b *Builder) locate(v reflect.Value, path string, wanted []target, matched []bool, found paths) {
	mark(v, path, wanted, matched, found)

	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return
	}

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

		value := v.Field(i)

		if b.mapper.isLeaf(field) {
			mark(value, childPath, wanted, matched, found)

			continue
		}

		b.locate(value, childPath, wanted, matched, found)
	}
}
