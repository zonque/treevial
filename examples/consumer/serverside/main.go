// Command serverside is a minimal gats server living in a module of its own.
// It imports the server side and the object store, and never mentions the
// client package.
package main

import (
	"flag"
	"log"
	"net"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/holoplot/gats"
	"github.com/holoplot/gats/objects"
	"github.com/holoplot/gats/server"
)

// provider hands each client a tree of its own, built when it connects and
// dropped when it leaves.
type provider struct{}

// Prepare implements server.Provider.
func (provider) Prepare(clientID string) (*objects.Store, plumbing.Hash, error) {
	store := objects.NewStore()

	blob, err := store.AddBlob([]byte("hello " + clientID + "\n"))
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	root, err := store.AddTree([]object.TreeEntry{
		{Name: "greeting", Mode: filemode.Regular, Hash: blob},
	})
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	log.Printf("prepared %s -> %s", gats.RefFor(clientID), root)

	return store, root, nil
}

// Release implements server.Provider.
func (provider) Release(clientID string) {
	log.Printf("released %s", clientID)
}

func main() {
	addr := flag.String("listen", "127.0.0.1:9418", "address to listen on")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("listening on %s", lis.Addr())

	if err := server.New(provider{}).Serve(lis); err != nil {
		log.Fatal(err)
	}
}
