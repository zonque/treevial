package receive

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Graph is a Handler that assembles the objects it is handed into a hierarchy
// held in plain Go maps. It accumulates across updates, so a later push that
// carries only the objects on a changed path still resolves against the parts
// of the graph an earlier push delivered.
//
// Accumulating is all it does on its own: a superseded blob or tree stays in
// the maps for as long as the graph lives, so a long-running subscription
// holds every state it has ever been pushed. [Graph.Retain] is how a caller
// says which of them it still needs.
//
// A Graph is not safe for concurrent use. Its objects arrive on the goroutine
// reading the connection, so a caller that works with one after an update
// must be finished with it before the next update is taken from the channel.
type Graph struct {
	blobs map[plumbing.Hash][]byte
	trees map[plumbing.Hash][]object.TreeEntry
}

// NewGraph returns an empty Graph.
func NewGraph() *Graph {
	return &Graph{
		blobs: map[plumbing.Hash][]byte{},
		trees: map[plumbing.Hash][]object.TreeEntry{},
	}
}

// OnPackHeader implements Handler.
func (g *Graph) OnPackHeader(uint32) error { return nil }

// OnBlob implements Handler.
func (g *Graph) OnBlob(h plumbing.Hash, content []byte) error {
	g.blobs[h] = append([]byte(nil), content...)

	return nil
}

// OnTree implements Handler.
func (g *Graph) OnTree(h plumbing.Hash, entries []object.TreeEntry) error {
	g.trees[h] = append([]object.TreeEntry(nil), entries...)

	return nil
}

// OnPackFooter implements Handler.
func (g *Graph) OnPackFooter(plumbing.Hash) error { return nil }

// Blob returns the content of the blob at h. The bytes belong to the graph and
// must not be modified.
func (g *Graph) Blob(h plumbing.Hash) ([]byte, bool) {
	content, ok := g.blobs[h]

	return content, ok
}

// Tree returns the entries of the tree at h. The slice belongs to the graph and
// must not be modified.
func (g *Graph) Tree(h plumbing.Hash) ([]object.TreeEntry, bool) {
	entries, ok := g.trees[h]

	return entries, ok
}

// entries is Tree for the callers that treat a gap as an error rather than as
// an answer, so that every one of them reports it the same way.
func (g *Graph) entries(h plumbing.Hash) ([]object.TreeEntry, error) {
	entries, ok := g.trees[h]
	if !ok {
		return nil, fmt.Errorf("tree %s missing from graph", h)
	}

	return entries, nil
}

// content is Blob for those same callers. The path is carried only so the
// message can say where the gap was found.
func (g *Graph) content(h plumbing.Hash, path string) ([]byte, error) {
	content, ok := g.blobs[h]
	if !ok {
		return nil, fmt.Errorf("blob %s (%s) missing from graph", h, path)
	}

	return content, nil
}

// Len reports how many objects the graph is holding, live or superseded. It is
// what a long-running client watches to see whether [Graph.Retain] is keeping
// up with what it is being sent.
func (g *Graph) Len() int {
	return len(g.blobs) + len(g.trees)
}

// Retain drops every object that no named root can reach, and reports how many
// it dropped.
//
// What a client still needs is the head it holds and the one it moved from —
// structtree.ApplySince, [Graph.Diff] and [Graph.ListingSince] are all asked
// about that pair — so after handling an update:
//
//	dropped, err := u.Graph.Retain(u.Previous, u.Hash)
//
// The zero hash among the roots is ignored, so the pair a first update carries
// works unchanged. A non-zero root the graph cannot walk in full is refused
// and nothing is dropped, since a sweep that cannot see all of what it is
// keeping would take the graph apart. Retaining nothing at all empties it.
//
// Dropping a state costs work rather than correctness: a later ApplySince
// against a baseline the graph no longer holds decodes everything instead of
// skipping the subtrees that did not move.
func (g *Graph) Retain(roots ...plumbing.Hash) (int, error) {
	live := map[plumbing.Hash]bool{}

	// Marked in full before anything is swept, so a root that turns out to
	// be incomplete leaves the graph as it was.
	for _, root := range roots {
		if root.IsZero() {
			continue
		}

		if err := g.mark(root, "", live); err != nil {
			return 0, err
		}
	}

	dropped := 0

	for h := range g.blobs {
		if !live[h] {
			delete(g.blobs, h)
			dropped++
		}
	}

	for h := range g.trees {
		if !live[h] {
			delete(g.trees, h)
			dropped++
		}
	}

	return dropped, nil
}

// mark collects every object reachable from the tree at h, reporting the first
// one the graph does not hold.
func (g *Graph) mark(h plumbing.Hash, prefix string, live map[plumbing.Hash]bool) error {
	if live[h] {
		return nil
	}

	entries, err := g.entries(h)
	if err != nil {
		return err
	}

	live[h] = true

	for _, e := range entries {
		path := prefix + e.Name

		if e.Mode == filemode.Dir {
			if err := g.mark(e.Hash, path+"/", live); err != nil {
				return err
			}

			continue
		}

		if _, err := g.content(e.Hash, path); err != nil {
			return err
		}

		live[e.Hash] = true
	}

	return nil
}

// Leaves walks the tree at root and returns every blob keyed by its
// slash-separated path.
func (g *Graph) Leaves(root plumbing.Hash) (map[string][]byte, error) {
	out := map[string][]byte{}

	if err := g.walk(root, "", out); err != nil {
		return nil, err
	}

	return out, nil
}

func (g *Graph) walk(h plumbing.Hash, prefix string, out map[string][]byte) error {
	entries, err := g.entries(h)
	if err != nil {
		return err
	}

	for _, e := range entries {
		path := prefix + e.Name

		if e.Mode == filemode.Dir {
			if err := g.walk(e.Hash, path+"/", out); err != nil {
				return err
			}

			continue
		}

		content, err := g.content(e.Hash, path)
		if err != nil {
			return err
		}
		out[path] = content
	}

	return nil
}
