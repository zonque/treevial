package receive

import (
	"fmt"
	"sort"

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
// A zero hash stands for an empty tree, so Diff(plumbing.ZeroHash, root) lists
// every leaf as added.
func (g *Graph) Diff(old, new plumbing.Hash) ([]Change, error) {
	var out []Change

	if err := g.diffTrees(old, new, "", &out); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	return out, nil
}

func (g *Graph) diffTrees(old, new plumbing.Hash, prefix string, out *[]Change) error {
	if old == new {
		return nil
	}

	oldEntries, err := g.entriesByName(old)
	if err != nil {
		return err
	}

	newEntries, err := g.entriesByName(new)
	if err != nil {
		return err
	}

	for name, ne := range newEntries {
		path := prefix + name

		oe, existed := oldEntries[name]
		if existed && oe.Hash == ne.Hash && oe.Mode == ne.Mode {
			continue
		}

		// A path that swapped between file and directory reads most
		// clearly as the old thing going away and the new one arriving.
		if existed && oe.Mode != ne.Mode {
			if err := g.report(Deleted, oe, path, out); err != nil {
				return err
			}
			existed = false
		}

		if ne.Mode == filemode.Dir {
			from := plumbing.ZeroHash
			if existed {
				from = oe.Hash
			}

			if err := g.diffTrees(from, ne.Hash, path+"/", out); err != nil {
				return err
			}

			continue
		}

		kind := Added
		if existed {
			kind = Modified
		}

		if err := g.report(kind, ne, path, out); err != nil {
			return err
		}
	}

	for name, oe := range oldEntries {
		if _, ok := newEntries[name]; ok {
			continue
		}

		if err := g.report(Deleted, oe, prefix+name, out); err != nil {
			return err
		}
	}

	return nil
}

// report appends a change for e, expanding a directory into one change per leaf
// beneath it.
func (g *Graph) report(kind ChangeKind, e object.TreeEntry, path string, out *[]Change) error {
	if e.Mode != filemode.Dir {
		change := Change{Kind: kind, Path: path, Hash: e.Hash}

		if kind != Deleted {
			content, ok := g.blobs[e.Hash]
			if !ok {
				return fmt.Errorf("blob %s (%s) missing from graph", e.Hash, path)
			}
			change.Content = content
		}

		*out = append(*out, change)

		return nil
	}

	leaves := map[string][]byte{}
	if err := g.walk(e.Hash, path+"/", leaves); err != nil {
		return err
	}

	for leafPath, content := range leaves {
		change := Change{Kind: kind, Path: leafPath}
		if kind != Deleted {
			change.Content = content
		}

		*out = append(*out, change)
	}

	return nil
}

// entriesByName indexes a tree's entries. The zero hash indexes as an empty
// tree, which is how an added or removed subtree is expanded.
func (g *Graph) entriesByName(h plumbing.Hash) (map[string]object.TreeEntry, error) {
	if h.IsZero() {
		return map[string]object.TreeEntry{}, nil
	}

	entries, ok := g.trees[h]
	if !ok {
		return nil, fmt.Errorf("tree %s missing from graph", h)
	}

	out := make(map[string]object.TreeEntry, len(entries))
	for _, e := range entries {
		out[e.Name] = e
	}

	return out, nil
}
