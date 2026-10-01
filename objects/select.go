package objects

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// SelectSince returns the objects a client needs in order to move from the tree
// it already holds to the tree at "to". A zero "from" means the client holds
// nothing, so the whole graph comes back.
//
// One hash on each side is all it takes: a subtree whose hash has not moved is
// pruned whole, because holding a tree means holding everything under it. That
// is what makes an update after a small change cost a few objects rather than
// the entire graph, without either side keeping an inventory.
//
// What comes back is what the client needs, and may be more: the two trees are
// compared position by position, so an object the client holds under another
// name is selected again rather than recognised. It is never short, which is
// the direction that matters, and a selection that grows to more than half of
// what the store holds is worked out again the exact way — see
// [Store.selectCompared] and [Store.selectExact] for the bargain between the
// two.
//
// The returned slice lists parents before children, but the pack encoder is
// free to reorder objects on the wire, so a receiver must not rely on seeing a
// tree's children first.
func (s *Store) SelectSince(from, to plumbing.Hash) ([]plumbing.Hash, error) {
	// With no baseline there is nothing to prune against, so the two
	// selections would do the same single walk; and the comparison below
	// has nothing to compare.
	if from.IsZero() {
		return s.selectExact(from, to)
	}

	// Read before anything is selected, so a baseline this store does not
	// hold is refused rather than quietly treated as no baseline at all —
	// including when it names the tree being selected, which the comparison
	// would otherwise answer for without ever reading it.
	if _, err := s.entries(from); err != nil {
		return nil, err
	}

	out, err := s.selectCompared(from, to)
	if err != nil {
		return nil, err
	}

	// A comparison by position cannot see an object the client holds under
	// another name, and selects it again. While the selection is small that
	// costs a handful of objects and is not worth a traversal of the whole
	// baseline to avoid. Once it is most of what the store holds, the change
	// is a bulk one rather than the incremental one the comparison is for,
	// the traversal is no longer the expensive half, and what the client
	// already holds is worth establishing exactly.
	if len(out) <= s.held()/2 {
		return out, nil
	}

	return s.selectExact(from, to)
}

// held reports how many objects the store holds, which is the one bound on what
// any selection from it can come to.
func (s *Store) held() int {
	s.objects.RLock()
	defer s.objects.RUnlock()

	return len(s.storage.Objects)
}

// selectCompared walks the two trees side by side and selects what the newer
// one holds that the older one did not hold at the same name.
//
// This is what makes a small change cheap. Descending both trees at once means
// an unmoved subtree is recognised from its parent's entry alone, so the walk
// goes no further than the change: establishing what the client holds costs
// nothing, where collecting it in full costs a traversal of the whole tree the
// client already had, every time, for every subscriber.
//
// Both entry lists are in git's canonical order, so the older one is advanced
// in step with the newer rather than indexed, and no map is built per tree.
//
// What it cannot see is reuse across paths: an object the client holds under
// another name looks new here and is selected again. That is a superset of
// what the client needs rather than a gap, which is the safe direction — and
// [Store.SelectSince] falls back on the exact selection when the superset
// would grow large enough to be worth avoiding.
func (s *Store) selectCompared(from, to plumbing.Hash) ([]plumbing.Hash, error) {
	var (
		out  []plumbing.Hash
		seen = map[plumbing.Hash]bool{}
	)

	var descend func(from, to plumbing.Hash, isTree bool) error

	descend = func(from, to plumbing.Hash, isTree bool) error {
		// Unmoved at this name, so the client holds it and everything
		// beneath it.
		if from == to || seen[to] {
			return nil
		}

		seen[to] = true
		out = append(out, to)

		if !isTree {
			return nil
		}

		now, err := s.entries(to)
		if err != nil {
			return err
		}

		var was []object.TreeEntry

		if !from.IsZero() {
			if was, err = s.entries(from); err != nil {
				return err
			}
		}

		held := 0

		for _, e := range now {
			// The old list is in the same order as the new one, so
			// it only ever moves forwards.
			for held < len(was) && earlier(was[held], e) {
				held++
			}

			before := plumbing.ZeroHash
			if held < len(was) && was[held].Name == e.Name && was[held].Mode == e.Mode {
				before = was[held].Hash
			}

			if err := descend(before, e.Hash, e.Mode == filemode.Dir); err != nil {
				return err
			}
		}

		return nil
	}

	if err := descend(from, to, true); err != nil {
		return nil, err
	}

	return out, nil
}

// earlier reports whether a sorts before b in git's canonical tree order,
// which compares a directory as though its name ended in a slash.
func earlier(a, b object.TreeEntry) bool {
	return sortName(a) < sortName(b)
}

func sortName(e object.TreeEntry) string {
	if e.Mode == filemode.Dir {
		return e.Name + "/"
	}

	return e.Name
}

// selectExact collects what the client holds in full and selects everything the
// newer tree reaches that is not in it, so an object the client holds under any
// name is pruned wherever it turns up.
//
// It costs a traversal of the whole tree the client already had.
// [Store.SelectSince] reaches for it only when the cheaper comparison would
// select enough to make that traversal worth paying for.
func (s *Store) selectExact(from, to plumbing.Hash) ([]plumbing.Hash, error) {
	held, err := s.reachable(from)
	if err != nil {
		return nil, err
	}

	var out []plumbing.Hash

	// Marked into the same set: an object selected is one the client will
	// hold, which is also what says not to select it twice.
	err = s.walk(to, func(h plumbing.Hash) (bool, error) {
		if held[h] {
			return false, nil
		}
		held[h] = true
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
