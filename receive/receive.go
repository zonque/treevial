// Package receive interprets an incoming git packfile as it arrives. It has no
// object store: each object is inflated, identified and handed to a Handler
// during the read, and the bytes are then free to be discarded. Nothing is
// written to disk or kept in a repository.
//
// A [Graph] is the Handler most callers want: it keeps what it is given in
// plain Go maps and can then be asked what arrived.
//
//   - [Graph.Leaves] gives every blob by path, which is what
//     structtree.Apply reads.
//   - [Graph.Diff] gives the paths that differ between two trees.
//   - [Graph.Listing] renders a whole tree the way "git ls-tree -r -t" would,
//     and [Graph.ListingSince] renders only what moved between two, in the
//     same format.
//   - [Graph.Retain] drops the states it has been pushed and no longer needs,
//     and [Graph.Len] says how much it is holding.
package receive

import (
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/zonque/treevial/internal/packread"
)

// Handler receives the objects of a packfile in stream order.
//
// go-git's packfile.Parser has an Observer interface with a similar shape, but
// it always passes nil object content (parser.go calls
// onInflatedObjectContent with a nil buffer) and it needs a storer to resolve
// deltas. The reading beneath [Interpret] therefore drives packfile.Scanner
// directly, which yields the inflated bytes and requires no storage at all.
type Handler interface {
	// OnPackHeader reports how many objects the pack declares.
	OnPackHeader(count uint32) error
	// OnBlob is called with a blob's inflated content. The slice is reused
	// after the call returns, so a handler that keeps it must copy it.
	OnBlob(h plumbing.Hash, content []byte) error
	// OnTree is called with a tree's decoded entries.
	OnTree(h plumbing.Hash, entries []object.TreeEntry) error
	// OnPackFooter reports the pack's trailing checksum.
	OnPackFooter(checksum plumbing.Hash) error
}

// Interpret reads a packfile from r and hands every object to h as it is
// inflated. Only blobs and trees are expected: the sender encodes without
// deltas, and treevial transfers no commits or tags.
func Interpret(r io.Reader, h Handler) error {
	return packread.Scan(r, h)
}
