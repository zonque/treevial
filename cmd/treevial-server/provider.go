package main

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/internal/demo"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/structtree"
)

// refData is everything the server holds for one ref: the Go value being
// synchronised and the store its objects live in.
type refData struct {
	config *demo.Config
	store  *objects.Store
}

// demoProvider gives each ref a configuration struct of its own, in a store of
// its own, and throws both away when its subscriber disconnects. Because
// nothing is shared between refs, releasing one is nothing more than dropping
// the reference.
type demoProvider struct {
	mu   sync.Mutex
	held map[string]*refData
}

func newDemoProvider() *demoProvider {
	return &demoProvider{held: map[string]*refData{}}
}

// name reads a label out of a ref. The server takes the ref verbatim; what to
// make of it is the provider's own business, and this one knows the shape of
// the namespace its clients ask for.
func name(ref string) string {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(ref, "refs/heads/"), "/config")
	if trimmed == "" {
		return ref
	}

	return trimmed
}

// Prepare implements server.Provider.
func (p *demoProvider) Prepare(ref string) (*objects.Store, plumbing.Hash, error) {
	data := &refData{config: demo.Example(name(ref)), store: objects.NewStore()}

	root, err := structtree.Build(data.store, data.config)
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	p.mu.Lock()
	p.held[ref] = data
	held := len(p.held)
	p.mu.Unlock()

	log.Printf("[%s] prepared -> %s, %d leaves walked from the struct (%d refs held)",
		ref, root, demo.LeafCount, held)

	return data.store, root, nil
}

// Release implements server.Provider.
func (p *demoProvider) Release(ref string) {
	p.mu.Lock()
	_, existed := p.held[ref]
	delete(p.held, ref)
	held := len(p.held)
	p.mu.Unlock()

	if !existed {
		return
	}

	log.Printf("[%s] subscriber gone; released its config and objects (%d refs held)", ref, held)
}

// Retune changes one deeply nested field of a ref's configuration and rebuilds
// the tree. Only the blob for that field and the trees above it are new, so the
// push that follows is tiny.
func (p *demoProvider) Retune(ref string) (plumbing.Hash, error) {
	p.mu.Lock()
	data, ok := p.held[ref]
	p.mu.Unlock()

	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("%q is gone", ref)
	}

	data.config.Network.Primary.MTU = 9000

	return structtree.Build(data.store, data.config)
}
