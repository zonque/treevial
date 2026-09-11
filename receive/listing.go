package receive

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
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
// a protobuf message or a slice appears as one blob, a nested struct as a tree.
func (g *Graph) Listing(root plumbing.Hash) (string, error) {
	var b strings.Builder

	if err := g.listing(root, "", &b); err != nil {
		return "", err
	}

	return b.String(), nil
}

func (g *Graph) listing(h plumbing.Hash, prefix string, b *strings.Builder) error {
	entries, ok := g.trees[h]
	if !ok {
		return fmt.Errorf("tree %s missing from graph", h)
	}

	for _, e := range entries {
		path := prefix + e.Name

		kind := "blob"
		if e.Mode == filemode.Dir {
			kind = "tree"
		}

		// Six octal digits, as git writes them.
		fmt.Fprintf(b, "%06o %s %s\t%s\n", uint32(e.Mode), kind, e.Hash, path)

		if e.Mode != filemode.Dir {
			continue
		}

		if err := g.listing(e.Hash, path+"/", b); err != nil {
			return err
		}
	}

	return nil
}
