package structtree

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Source reads the objects of a tree. *receive.Graph implements it, so a
// client can hand over what it has been pushed.
type Source interface {
	// Tree returns the entries of the tree at h.
	Tree(h plumbing.Hash) ([]object.TreeEntry, bool)
	// Blob returns the content of the blob at h.
	Blob(h plumbing.Hash) ([]byte, bool)
}

// ApplySince brings dst from the tree at old to the tree at new, using the
// default rules. It is shorthand for a zero [Mapper]'s ApplySince.
func ApplySince(dst any, src Source, old, new plumbing.Hash) error {
	return Mapper{}.ApplySince(dst, src, old, new)
}

// ApplySince brings dst from the tree at old to the tree at new, decoding only
// what moved between them.
//
// A subtree whose hash is unchanged is skipped whole, however many leaves are
// under it — the mirror of the pruning the sending side does to decide what to
// transmit. Changing one field of a large struct therefore costs one decode.
//
// dst must already hold the value at old. That is the price of the shortcut:
// the hashes say what moved between the two trees, not what dst contains, so
// applying against a baseline dst is not at will leave the untouched parts of
// dst as they were. Pass plumbing.ZeroHash as old to decode everything, which
// is always safe; a baseline the source cannot resolve is treated the same
// way.
func (m Mapper) ApplySince(dst any, src Source, old, new plumbing.Hash) error {
	v, err := destination(dst)
	if err != nil {
		return err
	}

	if new.IsZero() {
		return fmt.Errorf("structtree: target tree is the zero hash")
	}

	if old == new {
		return nil
	}

	root, err := newTreeView(src, old, new)
	if err != nil {
		return err
	}

	return m.apply(v, "", root)
}

// treeView reads from a tree, comparing it against a baseline tree so that
// anything whose hash has not moved can be skipped.
type treeView struct {
	src Source
	now map[string]object.TreeEntry
	// was is the baseline's entries, or nil when there is no usable
	// baseline — in which case nothing is ever reported as unchanged.
	was map[string]object.TreeEntry
}

func newTreeView(src Source, old, new plumbing.Hash) (treeView, error) {
	entries, ok := src.Tree(new)
	if !ok {
		return treeView{}, fmt.Errorf("structtree: tree %s is not in the source", new)
	}

	return treeView{src: src, now: byName(entries), was: baseline(src, old)}, nil
}

// baseline indexes the old tree, tolerating a hash the source cannot resolve:
// without it every field simply counts as changed.
func baseline(src Source, old plumbing.Hash) map[string]object.TreeEntry {
	if old.IsZero() {
		return nil
	}

	entries, ok := src.Tree(old)
	if !ok {
		return nil
	}

	return byName(entries)
}

func byName(entries []object.TreeEntry) map[string]object.TreeEntry {
	out := make(map[string]object.TreeEntry, len(entries))
	for _, e := range entries {
		out[e.Name] = e
	}

	return out
}

// entry looks up a field, reporting whether the baseline had it at the same
// hash. wantDir tells a subtree apart from a leaf, so a path whose kind has
// changed reads as missing rather than being decoded as the wrong thing.
func (t treeView) entry(name string, wantDir bool) (object.TreeEntry, state) {
	e, ok := t.now[name]
	if !ok || (e.Mode == filemode.Dir) != wantDir {
		return object.TreeEntry{}, missing
	}

	if was, had := t.was[name]; had && was.Hash == e.Hash && was.Mode == e.Mode {
		return e, unchanged
	}

	return e, changed
}

func (t treeView) leaf(name string) ([]byte, state, error) {
	e, st := t.entry(name, false)
	if st != changed {
		return nil, st, nil
	}

	data, ok := t.src.Blob(e.Hash)
	if !ok {
		return nil, missing, fmt.Errorf("blob %s is not in the source", e.Hash)
	}

	return data, changed, nil
}

func (t treeView) subtree(name string) (view, state, error) {
	e, st := t.entry(name, true)
	if st != changed {
		return nil, st, nil
	}

	entries, ok := t.src.Tree(e.Hash)
	if !ok {
		return nil, missing, fmt.Errorf("tree %s is not in the source", e.Hash)
	}

	sub := treeView{src: t.src, now: byName(entries)}

	// Only compare against a baseline subtree of the same name.
	if was, had := t.was[name]; had && was.Mode == filemode.Dir {
		sub.was = baseline(t.src, was.Hash)
	}

	return sub, changed, nil
}
