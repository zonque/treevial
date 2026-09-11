package main

import (
	"fmt"
	"log"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/holoplot/gats"
	"github.com/holoplot/gats/internal/demo"
	"github.com/holoplot/gats/objects"
	"github.com/holoplot/gats/structtree"
)

// clientData is everything the server holds for one client: the Go value being
// synchronised and the store its objects live in.
type clientData struct {
	config *demo.Config
	store  *objects.Store
}

// demoProvider gives each client that connects its own configuration struct,
// in a store of its own, and throws both away when the client disconnects.
// Because nothing is shared between clients, releasing one is nothing more
// than dropping the reference.
type demoProvider struct {
	mu   sync.Mutex
	held map[string]*clientData
}

func newDemoProvider() *demoProvider {
	return &demoProvider{held: map[string]*clientData{}}
}

// Prepare implements server.Provider.
func (p *demoProvider) Prepare(clientID string) (*objects.Store, plumbing.Hash, error) {
	data := &clientData{config: demo.Example(clientID), store: objects.NewStore()}

	root, err := structtree.Build(data.store, data.config)
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	p.mu.Lock()
	p.held[clientID] = data
	held := len(p.held)
	p.mu.Unlock()

	log.Printf("[%s] prepared %s -> %s, %d leaves walked from the struct (%d clients held)",
		clientID, gats.RefFor(clientID), root, demo.LeafCount, held)

	return data.store, root, nil
}

// Release implements server.Provider.
func (p *demoProvider) Release(clientID string) {
	p.mu.Lock()
	_, existed := p.held[clientID]
	delete(p.held, clientID)
	held := len(p.held)
	p.mu.Unlock()

	if !existed {
		return
	}

	log.Printf("[%s] disconnected; released its config and objects (%d clients held)", clientID, held)
}

// Retune changes one deeply nested field of a client's configuration and
// rebuilds the tree. Only the blob for that field and the trees above it are
// new, so the push that follows is tiny.
func (p *demoProvider) Retune(clientID string) (plumbing.Hash, error) {
	p.mu.Lock()
	data, ok := p.held[clientID]
	p.mu.Unlock()

	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("client %q is gone", clientID)
	}

	data.config.Network.Primary.MTU = 9000

	return structtree.Build(data.store, data.config)
}
