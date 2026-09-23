package objects

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// SelectSince returns the objects a client needs in order to move from the tree
// it already holds to the tree at "to". A zero "from" means the client holds
// nothing, so the whole graph comes back.
//
// One hash on each side is all it takes: a subtree reachable from "from" is
// pruned whole, because holding a tree means holding everything under it. That
// is what makes an update after a small change cost a few objects rather than
// the entire graph, without either side keeping an inventory.
//
// The returned slice lists parents before children, but the pack encoder is
// free to reorder objects on the wire, so a receiver must not rely on seeing a
// tree's children first.
func (s *Store) SelectSince(from, to plumbing.Hash) ([]plumbing.Hash, error) {
	held, err := s.reachable(from)
	if err != nil {
		return nil, err
	}

	var (
		out  []plumbing.Hash
		seen = map[plumbing.Hash]bool{}
	)

	err = s.walk(to, func(h plumbing.Hash) (bool, error) {
		if held[h] || seen[h] {
			return false, nil
		}
		seen[h] = true
		out = append(out, h)

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// reachable collects every object reachable from the tree at root. A zero root
// stands for an empty graph.
func (s *Store) reachable(root plumbing.Hash) (map[plumbing.Hash]bool, error) {
	out := map[plumbing.Hash]bool{}

	if root.IsZero() {
		return out, nil
	}

	err := s.walk(root, func(h plumbing.Hash) (bool, error) {
		if out[h] {
			return false, nil
		}
		out[h] = true

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// walk visits the graph rooted at the tree root, parents before children.
// visit reports whether to carry on into the object's children; returning false
// prunes it, which for a tree prunes everything beneath it.
func (s *Store) walk(root plumbing.Hash, visit func(plumbing.Hash) (bool, error)) error {
	var descend func(h plumbing.Hash, isTree bool) error

	descend = func(h plumbing.Hash, isTree bool) error {
		carryOn, err := visit(h)
		if err != nil || !carryOn || !isTree {
			return err
		}

		entries, err := s.entries(h)
		if err != nil {
			return err
		}

		for _, e := range entries {
			if err := descend(e.Hash, e.Mode == filemode.Dir); err != nil {
				return err
			}
		}

		return nil
	}

	return descend(root, true)
}
