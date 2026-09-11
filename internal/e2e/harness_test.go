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

	"github.com/holoplot/treevial/client"
	"github.com/holoplot/treevial/internal/demo"
	"github.com/holoplot/treevial/objects"
	"github.com/holoplot/treevial/server"
)

// testProvider prepares a ten-leaf tree per client, labelled with the client's
// own ID so a test can tell one client's data from another's, and records the
// lifecycle calls the server makes.
type testProvider struct {
	mu       sync.Mutex
	stores   map[string]*objects.Store
	prepared []string
	released []string
}

func newTestProvider() *testProvider {
	return &testProvider{stores: map[string]*objects.Store{}}
}

func (p *testProvider) Prepare(clientID string) (*objects.Store, plumbing.Hash, error) {
	store := objects.NewStore()

	root, err := demo.BuildTree(store, clientID)
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.stores[clientID] = store
	p.prepared = append(p.prepared, clientID)

	return store, root, nil
}

func (p *testProvider) Release(clientID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.stores, clientID)
	p.released = append(p.released, clientID)
}

func (p *testProvider) store(t *testing.T, clientID string) *objects.Store {
	t.Helper()

	p.mu.Lock()
	defer p.mu.Unlock()

	store, ok := p.stores[clientID]
	if !ok {
		t.Fatalf("no store prepared for %q", clientID)
	}

	return store
}

func (p *testProvider) counts(clientID string) (prepared, released int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, id := range p.prepared {
		if id == clientID {
			prepared++
		}
	}
	for _, id := range p.released {
		if id == clientID {
			released++
		}
	}

	return prepared, released
}

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

// subscribe connects a client under the given ID and waits for nothing; the
// caller decides when to read.
func (h *harness) subscribe(t *testing.T, ctx context.Context, clientID string) (*client.Client, <-chan client.Update) {
	t.Helper()

	c, err := client.Dial(ctx, h.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	updates, err := c.Subscribe(ctx, clientID)
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

func waitForSync(t *testing.T, srv *server.Server, clientID string, h plumbing.Hash) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range srv.Clients() {
			if c.ID == clientID && c.Synced == h {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("server never saw %q synced at %s", clientID, h)
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
