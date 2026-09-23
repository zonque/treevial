package receive

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Listing renders the objects reachable from root the way "git ls-tree -r -t"
// would, one line per object, depth-first and in the order the trees store
// them:
//
//	040000 tree 0d3b4a…	Device
//	040000 tree 7c1f92…	Device/Location
//	100644 blob 60c9f7…	Device/Location/Room
//
// It is a diagnostic: the type column says which paths ended up as blobs and
// which as trees, and the hashes say which objects a later update replaced. For
// a value mapped from a struct, that is where the leaf rules become visible —
// a protobuf message or a slice of scalars appears as one blob, a nested struct
// or a map as a tree.
//
// The root is not listed, since a listing names what is in a tree.
// [Graph.ListingSince] gives the same thing for what changed between two trees
// rather than for all of one.
func (g *Graph) Listing(root plumbing.Hash) (string, error) {
	var b strings.Builder

	if err := g.listing(root, "", &b); err != nil {
		return "", err
	}

	return b.String(), nil
}

func (g *Graph) listing(h plumbing.Hash, prefix string, b *strings.Builder) error {
	entries, err := g.entries(h)
	if err != nil {
		return err
	}

	for _, e := range entries {
		path := prefix + e.Name

		row(b, "", e, path)

		if e.Mode != filemode.Dir {
			continue
		}

		if err := g.listing(e.Hash, path+"/", b); err != nil {
			return err
		}
	}

	return nil
}

// row writes one object the way git ls-tree would, after whatever mark it was
// given. Both a listing and a changeset come through here, so the two cannot
// drift apart.
func row(b *strings.Builder, mark string, e object.TreeEntry, path string) {
	kind := "blob"
	if e.Mode == filemode.Dir {
		kind = "tree"
	}

	// Six octal digits, as git writes them.
	fmt.Fprintf(b, "%s%06o %s %s\t%s\n", mark, uint32(e.Mode), kind, e.Hash, path)
}

// ListingSince renders the objects that differ between two trees, in the format
// [Graph.Listing] uses for a whole one, each row marked with how it changed:
//
//	~ 040000 tree 40018139…	Network
//	~ 040000 tree 9d078488…	Network/Primary
//	~ 100644 blob bc5d0b77…	Network/Primary/MTU
//	+ 040000 tree 7f2a91c4…	Network/Secondary
//	+ 100644 blob 0ed01311…	Network/Secondary/Address
//	- 100644 blob 4b34d5e6…	Device/Serial
//
// Trees are included, not only the blobs, because that is what shows a change
// dragging every tree above it along: one blob moved, and three trees had to
// follow. A subtree whose hash has not moved is skipped whole, so the output is
// proportional to the change rather than to the tree.
//
// A removed object is shown with the hash it had. A path that was a blob and is
// now a tree, or the reverse, is shown as the old one going and a new one
// arriving, since nothing about it survived. A zero old hash means everything
// is new, which prints the whole tree as additions.
func (g *Graph) ListingSince(old, new plumbing.Hash) (string, error) {
	var b strings.Builder

	if err := g.listingSince(old, new, "", &b); err != nil {
		return "", err
	}

	return b.String(), nil
}

func (g *Graph) listingSince(old, new plumbing.Hash, prefix string, b *strings.Builder) error {
	if old == new {
		return nil
	}

	was, err := g.entriesByName(old)
	if err != nil {
		return err
	}

	now, err := g.entriesByName(new)
	if err != nil {
		return err
	}

	// The new tree's own order, which is git's, for everything still there.
	for _, e := range g.ordered(new) {
		path := prefix + e.Name

		before, existed := was[e.Name]

		switch {
		case existed && before.Hash == e.Hash && before.Mode == e.Mode:
			continue

		case existed && (before.Mode == filemode.Dir) != (e.Mode == filemode.Dir):
			// Nothing about it survived, so it goes and another comes.
			if err := g.whole(before, path, Deleted, b); err != nil {
				return err
			}

			if err := g.whole(e, path, Added, b); err != nil {
				return err
			}

		case !existed:
			if err := g.whole(e, path, Added, b); err != nil {
				return err
			}

		default:
			row(b, Modified.Symbol()+" ", e, path)

			if e.Mode == filemode.Dir {
				if err := g.listingSince(before.Hash, e.Hash, path+"/", b); err != nil {
					return err
				}
			}
		}
	}

	// Then what has gone, in the order the old tree held it.
	for _, e := range g.ordered(old) {
		if _, still := now[e.Name]; still {
			continue
		}

		if err := g.whole(e, prefix+e.Name, Deleted, b); err != nil {
			return err
		}
	}

	return nil
}

// whole marks one entry and, if it is a tree, everything beneath it.
func (g *Graph) whole(e object.TreeEntry, path string, kind ChangeKind, b *strings.Builder) error {
	row(b, kind.Symbol()+" ", e, path)

	if e.Mode != filemode.Dir {
		return nil
	}

	entries, err := g.entries(e.Hash)
	if err != nil {
		return err
	}

	for _, child := range entries {
		if err := g.whole(child, path+"/"+child.Name, kind, b); err != nil {
			return err
		}
	}

	return nil
}

// ordered returns a tree's entries in the order it holds them, and nothing for
// the zero hash.
func (g *Graph) ordered(h plumbing.Hash) []object.TreeEntry {
	if h.IsZero() {
		return nil
	}

	return g.trees[h]
}
