// Package e2e exercises the server and client together over a real TCP
// listener, which is where the reversed roles, the per-client data and the
// long-lived connection actually show up.
package e2e

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
	"github.com/zonque/treevial/structtree"
)

// refData is what the provider holds for one ref, the same way the example
// server does: the value being synchronised, its store, and the builder that
// keeps the two in step without redoing work.
type refData struct {
	config  *shared.Config
	store   *objects.Store
	builder *structtree.Builder
}

// testProvider prepares a configuration per ref, labelled with the ref itself
// so a test can tell one subscriber's data from another's, and records the
// lifecycle calls the server makes.
type testProvider struct {
	mu       sync.Mutex
	held     map[string]*refData
	prepared []string
	released []string
}

func newTestProvider() *testProvider {
	return &testProvider{held: map[string]*refData{}}
}

func (p *testProvider) Prepare(ref string) (*objects.Store, plumbing.Hash, error) {
	data := &refData{config: shared.Example(ref), store: objects.NewStore()}

	builder, err := structtree.NewBuilder(data.store, data.config)
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}
	data.builder = builder

	root, err := builder.Build()
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.held[ref] = data
	p.prepared = append(p.prepared, ref)

	return data.store, root, nil
}

func (p *testProvider) Release(ref string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.held, ref)
	p.released = append(p.released, ref)
}

// retune changes one deeply nested field and rebuilds, declaring the field it
// touched so only that leaf is encoded again — the path the example server
// takes, exercised here end to end.
func (p *testProvider) retune(t *testing.T, ref string, mtu int) plumbing.Hash {
	t.Helper()

	p.mu.Lock()
	data, ok := p.held[ref]
	p.mu.Unlock()

	if !ok {
		t.Fatalf("no data prepared for %q", ref)
	}

	data.config.Network.Primary.MTU = mtu

	root, err := data.builder.Build(&data.config.Network.Primary.MTU)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return root
}

func (p *testProvider) counts(ref string) (prepared, released int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, r := range p.prepared {
		if r == ref {
			prepared++
		}
	}
	for _, r := range p.released {
		if r == ref {
			released++
		}
	}

	return prepared, released
}

// The refs these tests subscribe to. Nothing about their shape is meaningful
// to either side; they are just two distinct, well-formed refs.
const (
	refA = "refs/heads/printer-7/config"
	refB = "refs/heads/printer-8/config"
)

type harness struct {
	server   *server.Server
	provider *testProvider
	addr     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	provider := newTestProvider()
	srv := server.New(provider)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("Serve: %v", err)
		}
	}()
	t.Cleanup(srv.Stop)

	return &harness{server: srv, provider: provider, addr: lis.Addr().String()}
}

// subscribe connects a client to ref and waits for nothing; the caller decides
// when to read.
func (h *harness) subscribe(t *testing.T, ctx context.Context, ref string) (*client.Client, <-chan client.Update) {
	t.Helper()

	c, err := client.Dial(ctx, h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	updates, err := c.Subscribe(ctx, ref)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	return c, updates
}

func nextUpdate(t *testing.T, updates <-chan client.Update) client.Update {
	t.Helper()

	select {
	case u, ok := <-updates:
		if !ok {
			t.Fatal("update channel closed before an update arrived")
		}

		return u
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the server to push an update")
	}

	return client.Update{}
}

func waitForSync(t *testing.T, srv *server.Server, ref string, h plumbing.Hash) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, sub := range srv.Subscribers() {
			if sub.Ref == ref && sub.Synced == h {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("server never saw %q synced at %s", ref, h)
}

// eventually polls until cond holds, for assertions about state the server
// reaches on its own.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}
