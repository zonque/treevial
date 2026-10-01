package main

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
)

// demoProvider gives each ref a configuration struct of its own, in a store of
// its own, and throws both away when its subscriber disconnects. Because
// nothing is shared between refs, releasing one is nothing more than dropping
// the reference.
type demoProvider struct {
	mu   sync.Mutex
	held map[string]*shared.Ref
}

func newDemoProvider() *demoProvider {
	return &demoProvider{held: map[string]*shared.Ref{}}
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
func (p *demoProvider) Prepare(ref string) (*objects.Store, server.Head, error) {
	data, root, err := shared.NewRef(name(ref))
	if err != nil {
		return nil, server.Head{}, err
	}

	p.mu.Lock()
	p.held[ref] = data
	held := len(p.held)
	p.mu.Unlock()

	log.Printf("[%s] prepared -> %s, %d leaves walked from the struct (%d refs held)",
		ref, root, shared.LeafCount, held)

	return data.Store, server.Head{Hash: root}, nil
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

// Retune moves a ref's configuration on by one field. See
// [shared.Ref.Retune] for why that costs so little.
func (p *demoProvider) Retune(ref string) (server.Head, error) {
	p.mu.Lock()
	data, ok := p.held[ref]
	p.mu.Unlock()

	if !ok {
		return server.Head{}, fmt.Errorf("%q is gone", ref)
	}

	root, err := data.Retune(9000)
	if err != nil {
		return server.Head{}, err
	}

	// This server has no consensus layer behind it, so its refs are
	// unsequenced: there is nothing to order them against.
	return server.Head{Hash: root}, nil
}
