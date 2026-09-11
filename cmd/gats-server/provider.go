package main

import (
	"log"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/holoplot/gats"
	"github.com/holoplot/gats/internal/demo"
	"github.com/holoplot/gats/objects"
)

// demoProvider builds a nested tree of ten blobs for each client that
// connects, in a store of that client's own, and throws the store away when the
// client disconnects. Because every client's objects live in a separate store,
// releasing one is nothing more than dropping the reference.
type demoProvider struct {
	mu     sync.Mutex
	stores map[string]*objects.Store
}

func newDemoProvider() *demoProvider {
	return &demoProvider{stores: map[string]*objects.Store{}}
}

// Prepare implements server.Provider.
func (p *demoProvider) Prepare(clientID string) (*objects.Store, plumbing.Hash, error) {
	store := objects.NewStore()

	root, err := demo.BuildTree(store, clientID)
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	p.mu.Lock()
	p.stores[clientID] = store
	held := len(p.stores)
	p.mu.Unlock()

	log.Printf("[%s] prepared %s -> %s with %d blob leaves (%d clients held)",
		clientID, gats.RefFor(clientID), root, demo.LeafCount, held)

	return store, root, nil
}

// Release implements server.Provider.
func (p *demoProvider) Release(clientID string) {
	p.mu.Lock()
	_, existed := p.stores[clientID]
	delete(p.stores, clientID)
	held := len(p.stores)
	p.mu.Unlock()

	if !existed {
		return
	}

	log.Printf("[%s] disconnected; released its objects (%d clients held)", clientID, held)
}

// store returns a client's store, or nil once it has been released.
func (p *demoProvider) store(clientID string) *objects.Store {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.stores[clientID]
}
