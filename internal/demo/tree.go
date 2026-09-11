// Package demo builds the example object graph that the gats example programs
// serve. It is not part of the library's API: a real server builds its own
// trees with the objects package.
package demo

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/holoplot/gats/objects"
)

// LeafCount is the number of blobs BuildTree hangs off its tree.
const LeafCount = 10

// BuildTree constructs a nested tree with ten blob leaves and returns the root
// tree hash. No commit object is involved: the ref will point straight at this
// tree.
//
//	root/
//	├── a/       leaf-00 .. leaf-03
//	├── b/
//	│   └── c/   leaf-04 .. leaf-07
//	└── leaf-08, leaf-09
//
// label is woven into every blob so a second call with a different label
// produces a different object graph.
func BuildTree(s *objects.Store, label string) (plumbing.Hash, error) {
	leaf := func(i int) (object.TreeEntry, error) {
		name := fmt.Sprintf("leaf-%02d", i)

		h, err := s.AddBlob(fmt.Appendf(nil, "%s %s\n", name, label))
		if err != nil {
			return object.TreeEntry{}, err
		}

		return object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h}, nil
	}

	var entries [LeafCount]object.TreeEntry
	for i := range entries {
		e, err := leaf(i)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries[i] = e
	}

	c, err := s.AddTree(entries[4:8])
	if err != nil {
		return plumbing.ZeroHash, err
	}

	b, err := s.AddTree([]object.TreeEntry{{Name: "c", Mode: filemode.Dir, Hash: c}})
	if err != nil {
		return plumbing.ZeroHash, err
	}

	a, err := s.AddTree(entries[0:4])
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return s.AddTree([]object.TreeEntry{
		{Name: "a", Mode: filemode.Dir, Hash: a},
		{Name: "b", Mode: filemode.Dir, Hash: b},
		entries[8],
		entries[9],
	})
}
