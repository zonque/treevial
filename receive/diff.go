package receive

import (
	"fmt"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ChangeKind says how a path differs between two trees.
type ChangeKind int

const (
	// Added means the path exists only in the newer tree.
	Added ChangeKind = iota
	// Modified means the path exists in both but its content differs.
	Modified
	// Deleted means the path exists only in the older tree.
	Deleted
)

// String implements fmt.Stringer.
func (k ChangeKind) String() string {
	switch k {
	case Added:
		return "added"
	case Modified:
		return "modified"
	case Deleted:
		return "deleted"
	default:
		return fmt.Sprintf("ChangeKind(%d)", int(k))
	}
}

// Symbol returns a one-character marker for listings.
func (k ChangeKind) Symbol() string {
	switch k {
	case Added:
		return "+"
	case Modified:
		return "~"
	case Deleted:
		return "-"
	default:
		return "?"
	}
}

// Change is one differing path. Content is the new content, and is nil for a
// deletion.
type Change struct {
	Kind    ChangeKind
	Path    string
	Hash    plumbing.Hash
	Content []byte
}

// String implements fmt.Stringer, so a change reads well in test failures.
func (c Change) String() string {
	return fmt.Sprintf("%s %s", c.Kind.Symbol(), c.Path)
}

// Diff compares two trees the graph holds and returns the paths that differ,
// sorted by path. Subtrees whose hashes match are skipped whole, which is what
// makes reporting a small change cheap no matter how large the tree is.
//
// Only paths are reported, not the trees above them: a change arrives as one
// blob moved and every tree on its path rewritten, and it is the blob that is
// the change. [Graph.ListingSince] is the same comparison with the trees left
// in.
//
// A zero hash stands for an empty tree, so Diff(plumbing.ZeroHash, root) lists
// every leaf as added.
func (g *Graph) Diff(old, new plumbing.Hash) ([]Change, error) {
	var out []Change

	err := g.compare(old, new, "", func(kind ChangeKind, e object.TreeEntry, path string) error {
		// A tree is the carriage, not the cargo.
		if e.Mode == filemode.Dir {
			return nil
		}

		change := Change{Kind: kind, Path: path, Hash: e.Hash}

		// A deletion has no new content; the hash it had is what names
		// it.
		if kind != Deleted {
			content, err := g.content(e.Hash, path)
			if err != nil {
				return err
			}
			change.Content = content
		}

		out = append(out, change)

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(out, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })

	return out, nil
}
