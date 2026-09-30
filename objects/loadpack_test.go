package objects_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/zonque/treevial/objects"
)

// A whole graph out through a pack and back into a store that had nothing.
// This is a snapshot and its restore with the transport left out, and the only
// assertion content addressing needs is that the same root reaches the same
// objects on the far side.
func TestAPackRoundTripsBetweenStores(t *testing.T) {
	from := objects.NewStore()

	blob, err := from.AddBlob([]byte("9000"))
	if err != nil {
		t.Fatalf("AddBlob: %v", err)
	}

	inner, err := from.AddTree([]object.TreeEntry{
		{Name: "mtu", Mode: filemode.Regular, Hash: blob},
	})
	if err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	root, err := from.AddTree([]object.TreeEntry{
		{Name: "network", Mode: filemode.Dir, Hash: inner},
	})
	if err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	hashes, err := from.SelectSince(plumbing.ZeroHash, root)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	var pack bytes.Buffer
	if _, err := from.EncodePack(&pack, hashes); err != nil {
		t.Fatalf("EncodePack: %v", err)
	}

	into := objects.NewStore()

	n, err := into.LoadPack(&pack)
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}
	if n != len(hashes) {
		t.Errorf("loaded %d objects, want %d", n, len(hashes))
	}

	restored, err := into.SelectSince(plumbing.ZeroHash, root)
	if err != nil {
		t.Fatalf("SelectSince on the restored store: %v", err)
	}
	if len(restored) != len(hashes) {
		t.Errorf("the restored store reaches %d objects from %s, want %d",
			len(restored), root, len(hashes))
	}

	if got, ok := into.Blob(blob); !ok || string(got) != "9000" {
		t.Errorf("Blob(%s) = %q, %v; want %q, true", blob, got, ok, "9000")
	}
}

func TestLoadPackReadsAnEmptyPack(t *testing.T) {
	var pack bytes.Buffer
	if _, err := objects.NewStore().EncodePack(&pack, nil); err != nil {
		t.Fatalf("EncodePack: %v", err)
	}

	n, err := objects.NewStore().LoadPack(&pack)
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}
	if n != 0 {
		t.Errorf("loaded %d objects from an empty pack, want 0", n)
	}
}

func TestLoadPackRefusesATruncatedPack(t *testing.T) {
	store := objects.NewStore()

	blob, err := store.AddBlob([]byte("something long enough to be cut in half"))
	if err != nil {
		t.Fatalf("AddBlob: %v", err)
	}

	var pack bytes.Buffer
	if _, err := store.EncodePack(&pack, []plumbing.Hash{blob}); err != nil {
		t.Fatalf("EncodePack: %v", err)
	}

	cut := pack.Bytes()[:pack.Len()/2]

	if _, err := objects.NewStore().LoadPack(bytes.NewReader(cut)); err == nil {
		t.Error("LoadPack accepted a truncated pack")
	}
}

// AddTree sorts entries into git's canonical order before hashing, so a tree
// arriving in any other order would be stored under a hash its sender never
// used. That is what the check inside LoadPack is for, and it is the one thing
// a pack can get wrong that its own checksum will not catch.
func TestLoadPackRefusesATreeThatIsNotInCanonicalOrder(t *testing.T) {
	// Built outside a Store, because a Store cannot store a tree in the
	// wrong order — which is the assumption under test.
	storage := memory.NewStorage()

	blob := storage.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)

	w, err := blob.Writer()
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}
	if _, err := w.Write([]byte("1500")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	blobHash, err := storage.SetEncodedObject(blob)
	if err != nil {
		t.Fatalf("SetEncodedObject: %v", err)
	}

	// "b" before "a", written as the bytes of a tree object rather than
	// through go-git's tree encoder, which refuses to write one out of
	// order. Another implementation is under no such obligation, which is
	// the case worth defending against.
	var body bytes.Buffer
	for _, name := range []string{"b", "a"} {
		fmt.Fprintf(&body, "%o %s%c", filemode.Regular, name, 0)
		body.Write(blobHash[:])
	}

	obj := storage.NewEncodedObject()
	obj.SetType(plumbing.TreeObject)
	obj.SetSize(int64(body.Len()))

	tw, err := obj.Writer()
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}
	if _, err := tw.Write(body.Bytes()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	treeHash, err := storage.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("SetEncodedObject: %v", err)
	}

	var pack bytes.Buffer

	_, err = packfile.NewEncoder(&pack, storage, false).
		Encode([]plumbing.Hash{treeHash, blobHash}, 0)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	_, err = objects.NewStore().LoadPack(&pack)
	if err == nil {
		t.Fatal("LoadPack accepted a tree that was not in canonical order")
	}
	if !strings.Contains(err.Error(), treeHash.String()) {
		t.Errorf("the error does not name the hash the pack carried: %v", err)
	}
}
