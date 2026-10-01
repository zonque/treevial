package receive

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// visitor is told about one entry the comparison or the walk below reached: how
// it changed, the entry itself, and the slash-separated path leading to it. A
// directory entry arrives before anything beneath it.
//
// Whether a tree is interesting is the visitor's business: [Graph.Diff] reports
// paths and so ignores them, while [Graph.ListingSince] names them precisely
// because that is what shows a change dragging every tree above it along.
type visitor func(kind ChangeKind, e object.TreeEntry, path string) error

// compare walks two trees side by side and reports every entry that differs,
// in the order the newer tree holds them, followed by what has gone in the
// order the older one held it.
//
// A subtree whose hash and mode have not moved is skipped whole, so the work is
// proportional to the change rather than to the tree. That is the same pruning
// the sending side does to decide what to transmit, and it is why reporting a
// small change is cheap however large the whole is.
//
// A zero hash on either side stands for an empty tree, so comparing against one
// reports the other whole.
//
// Both [Graph.Diff] and [Graph.ListingSince] are this walk with a visitor of
// their own. They used to be two copies of it, which had to be kept agreeing by
// hand.
func (g *Graph) compare(old, new plumbing.Hash, prefix string, visit visitor) error {
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

	for _, e := range g.ordered(new) {
		path := prefix + e.Name

		before, existed := was[e.Name]

		switch {
		case existed && before.Hash == e.Hash && before.Mode == e.Mode:
			continue

		case existed && (before.Mode == filemode.Dir) != (e.Mode == filemode.Dir):
			// A path that swapped between file and directory kept
			// nothing, so it reads most clearly as the old thing
			// going away and the new one arriving.
			if err := g.whole(Deleted, before, path, visit); err != nil {
				return err
			}

			if err := g.whole(Added, e, path, visit); err != nil {
				return err
			}

		case !existed:
			if err := g.whole(Added, e, path, visit); err != nil {
				return err
			}

		default:
			if err := visit(Modified, e, path); err != nil {
				return err
			}

			if e.Mode == filemode.Dir {
				if err := g.compare(before.Hash, e.Hash, path+"/", visit); err != nil {
					return err
				}
			}
		}
	}

	for _, e := range g.ordered(old) {
		if _, still := now[e.Name]; still {
			continue
		}

		if err := g.whole(Deleted, e, prefix+e.Name, visit); err != nil {
			return err
		}
	}

	return nil
}

// whole reports one entry as kind and, if it is a tree, everything beneath it
// as the same thing: a subtree that has arrived or gone did so with all of its
// contents.
func (g *Graph) whole(kind ChangeKind, e object.TreeEntry, path string, visit visitor) error {
	if err := visit(kind, e, path); err != nil {
		return err
	}

	if e.Mode != filemode.Dir {
		return nil
	}

	return g.descend(e.Hash, path+"/", func(p string, child object.TreeEntry) (bool, error) {
		return true, visit(kind, child, p)
	})
}

// entriesByName indexes a tree's entries. The zero hash indexes as an empty
// tree, which is how an added or removed subtree is expanded.
func (g *Graph) entriesByName(h plumbing.Hash) (map[string]object.TreeEntry, error) {
	if h.IsZero() {
		return map[string]object.TreeEntry{}, nil
	}

	entries, err := g.entries(h)
	if err != nil {
		return nil, err
	}

	out := make(map[string]object.TreeEntry, len(entries))
	for _, e := range entries {
		out[e.Name] = e
	}

	return out, nil
}

// ordered returns a tree's entries in the order it holds them, which is git's,
// and nothing for the zero hash.
func (g *Graph) ordered(h plumbing.Hash) []object.TreeEntry {
	if h.IsZero() {
		return nil
	}

	return g.trees[h]
}
