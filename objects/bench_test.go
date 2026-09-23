package objects_test

import (
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/zonque/treevial/objects"
)

// The shape these benchmarks are measured on: a tree wide enough that walking
// it is the cost that matters, at roughly the scale the README quotes for a
// struct of ten thousand map entries.
const (
	benchSubtrees   = 10000
	benchLeavesEach = 5
)

// wideTree stores a root of n subtrees, each holding benchLeavesEach blobs,
// and returns the store and the root hash.
func wideTree(tb testing.TB, n int) (*objects.Store, plumbing.Hash) {
	tb.Helper()

	s := objects.NewStore()

	roots := make([]object.TreeEntry, 0, n)

	for i := range n {
		leaves := make([]object.TreeEntry, 0, benchLeavesEach)

		for j := range benchLeavesEach {
			blob, err := s.AddBlob(fmt.Appendf(nil, "value-%d-%d", i, j))
			if err != nil {
				tb.Fatalf("AddBlob: %v", err)
			}

			leaves = append(leaves, object.TreeEntry{
				Name: fmt.Sprintf("leaf-%d", j),
				Mode: filemode.Regular,
				Hash: blob,
			})
		}

		sub, err := s.AddTree(leaves)
		if err != nil {
			tb.Fatalf("AddTree: %v", err)
		}

		roots = append(roots, object.TreeEntry{
			Name: fmt.Sprintf("entry-%05d", i),
			Mode: filemode.Dir,
			Hash: sub,
		})
	}

	root, err := s.AddTree(roots)
	if err != nil {
		tb.Fatalf("AddTree: %v", err)
	}

	return s, root
}

// moved rewrites one leaf of a wide tree, the way a one-field change does. The
// three objects it produces — the blob, its subtree and the root — are what a
// subscriber one move behind has to be sent.
func moved(tb testing.TB, s *objects.Store, root plumbing.Hash, entry int) plumbing.Hash {
	tb.Helper()

	next, err := s.ReplaceBlob(root, fmt.Sprintf("entry-%05d/leaf-2", entry), []byte("9000"))
	if err != nil {
		tb.Fatalf("ReplaceBlob: %v", err)
	}

	return next
}

// BenchmarkSelectSinceFromNothing is what a client that holds nothing costs
// the server: the whole graph, with nothing to prune.
func BenchmarkSelectSinceFromNothing(b *testing.B) {
	s, root := wideTree(b, benchSubtrees)

	b.ReportAllocs()

	for b.Loop() {
		missing, err := s.SelectSince(plumbing.ZeroHash, root)
		if err != nil {
			b.Fatalf("SelectSince: %v", err)
		}
		if want := benchSubtrees*(benchLeavesEach+1) + 1; len(missing) != want {
			b.Fatalf("selected %d objects, want %d", len(missing), want)
		}
	}
}

// BenchmarkSelectSinceOneLeafMoved is the case the whole design is for: a
// client one field behind, which should be told about three objects. The walk
// still has to establish what that client already holds, which is what this
// measures.
func BenchmarkSelectSinceOneLeafMoved(b *testing.B) {
	s, root := wideTree(b, benchSubtrees)
	next := moved(b, s, root, 4212)

	b.ReportAllocs()

	for b.Loop() {
		missing, err := s.SelectSince(root, next)
		if err != nil {
			b.Fatalf("SelectSince: %v", err)
		}
		if want := 3; len(missing) != want {
			b.Fatalf("selected %d objects, want %d", len(missing), want)
		}
	}
}

// BenchmarkEncodePackOneLeafMoved measures writing that handful of objects
// out, so the cost of deciding what to send can be read against the cost of
// sending it.
func BenchmarkEncodePackOneLeafMoved(b *testing.B) {
	s, root := wideTree(b, benchSubtrees)
	next := moved(b, s, root, 4212)

	missing, err := s.SelectSince(root, next)
	if err != nil {
		b.Fatalf("SelectSince: %v", err)
	}

	b.ReportAllocs()

	for b.Loop() {
		if _, err := s.EncodePack(io.Discard, missing); err != nil {
			b.Fatalf("EncodePack: %v", err)
		}
	}
}

// TestConcurrentSelectSinceIsSafe covers the reason the memo is locked at all:
// every subscriber to a ref walks the one store the provider prepared, on its
// own goroutine. Run under -race, this is what says the memo may be shared.
func TestConcurrentSelectSinceIsSafe(t *testing.T) {
	s, root := wideTree(t, 64)
	next := moved(t, s, root, 17)

	const readers = 8

	var (
		wg   sync.WaitGroup
		errs = make(chan error, 2*readers)
	)

	for range readers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			// From nothing and from the previous head at once, so the
			// memo is read and filled from several goroutines.
			if _, err := s.SelectSince(plumbing.ZeroHash, next); err != nil {
				errs <- err

				return
			}

			missing, err := s.SelectSince(root, next)
			if err != nil {
				errs <- err

				return
			}

			if want := 3; len(missing) != want {
				errs <- fmt.Errorf("selected %d objects, want %d", len(missing), want)
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}
