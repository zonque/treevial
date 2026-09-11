// Package receive interprets an incoming git packfile as it arrives. It has no
// object store: each object is inflated, identified and handed to a Handler
// during the read, and the bytes are then free to be discarded. Nothing is
// written to disk or kept in a repository.
package receive

import (
	"bytes"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Handler receives the objects of a packfile in stream order.
//
// go-git's packfile.Parser has an Observer interface with a similar shape, but
// it always passes nil object content (parser.go calls
// onInflatedObjectContent with a nil buffer) and it needs a storer to resolve
// deltas. Interpret therefore drives packfile.Scanner directly, which yields
// the inflated bytes and requires no storage at all.
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
	scanner := packfile.NewScanner(r)

	_, count, err := scanner.Header()
	if err != nil {
		return fmt.Errorf("read pack header: %w", err)
	}

	if err := h.OnPackHeader(count); err != nil {
		return err
	}

	var buf bytes.Buffer

	for i := range count {
		oh, err := scanner.NextObjectHeader()
		if err != nil {
			return fmt.Errorf("read object %d header: %w", i, err)
		}

		buf.Reset()
		if _, _, err := scanner.NextObject(&buf); err != nil {
			return fmt.Errorf("inflate object %d: %w", i, err)
		}

		hash := plumbing.ComputeHash(oh.Type, buf.Bytes())

		switch oh.Type {
		case plumbing.BlobObject:
			if err := h.OnBlob(hash, buf.Bytes()); err != nil {
				return err
			}
		case plumbing.TreeObject:
			entries, err := decodeTreeEntries(buf.Bytes())
			if err != nil {
				return fmt.Errorf("decode tree %s: %w", hash, err)
			}
			if err := h.OnTree(hash, entries); err != nil {
				return err
			}
		default:
			return fmt.Errorf("object %d: unexpected type %s", i, oh.Type)
		}
	}

	checksum, err := scanner.Checksum()
	if err != nil {
		return fmt.Errorf("read pack checksum: %w", err)
	}

	return h.OnPackFooter(checksum)
}

// decodeTreeEntries parses a tree object's body without storing it.
func decodeTreeEntries(content []byte) ([]object.TreeEntry, error) {
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.TreeObject)

	if _, err := obj.Write(content); err != nil {
		return nil, err
	}

	tree := &object.Tree{}
	if err := tree.Decode(obj); err != nil {
		return nil, err
	}

	return tree.Entries, nil
}
