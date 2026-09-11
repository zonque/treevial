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
	entries, ok := g.trees[h]
	if !ok {
		return fmt.Errorf("tree %s missing from graph", h)
	}

	for _, e := range entries {
		path := prefix + e.Name

		if e.Mode == filemode.Dir {
			if err := g.walk(e.Hash, path+"/", out); err != nil {
				return err
			}

			continue
		}

		content, ok := g.blobs[e.Hash]
		if !ok {
			return fmt.Errorf("blob %s (%s) missing from graph", e.Hash, path)
		}
		out[path] = content
	}

	return nil
}
