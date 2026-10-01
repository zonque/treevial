// Package e2e exercises the server and client together over a real TCP
// listener, which is where the reversed roles, the per-client data and the
// long-lived connection actually show up.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
)

// testProvider prepares a configuration per ref, labelled with the ref itself
// so a test can tell one subscriber's data from another's, and records the
// lifecycle calls the server makes.
type testProvider struct {
	mu       sync.Mutex
	held     map[string]*shared.Ref
	prepared []string
	released []string
	// calls records when a preparation started and when a release
	// finished, in order, so a test can see whether the two ever overlap
	// for one ref.
	calls []string
	// releaseDelay makes letting go take a while, which is when an
	// overlapping preparation would show up.
	releaseDelay time.Duration
	// failPrepare makes preparation fail, so a test can watch a client
	// being refused.
	failPrepare bool
	// sequenced makes this provider hand out sequenced heads, starting at
	// 1, the way one backed by a consensus layer would.
	sequenced bool
	// preparing is closed by Prepare to say it has been entered, and
	// prepareGate holds it there, so a test can act in the window between
	// a subscriber joining a ref and the provider answering for it.
	preparing   chan struct{}
	prepareGate chan struct{}
}

func newTestProvider() *testProvider {
	return &testProvider{held: map[string]*shared.Ref{}}
}

func (p *testProvider) Prepare(ref string) (*objects.Store, server.Head, error) {
	p.mu.Lock()
	p.calls = append(p.calls, "prepare "+ref)
	fail := p.failPrepare
	p.mu.Unlock()

	if fail {
		return nil, server.Head{}, errors.New("no data for this ref")
	}

	if p.preparing != nil {
		close(p.preparing)
		<-p.prepareGate
	}

	data, root, err := shared.NewRef(ref)
	if err != nil {
		return nil, server.Head{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.held[ref] = data
	p.prepared = append(p.prepared, ref)

	head := server.Head{Hash: root}
	if p.sequenced {
		head.Sequence = 1
	}

	return data.Store, head, nil
}

func (p *testProvider) Release(ref string) {
	p.mu.Lock()
	delay := p.releaseDelay
	p.mu.Unlock()

	// Letting go may take a provider a while — closing files, draining a
	// cache. Whatever the server does next must not depend on it being
	// quick.
	time.Sleep(delay)

	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.held, ref)
	p.released = append(p.released, ref)
	p.calls = append(p.calls, "release "+ref)
}

// lifecycle returns the preparations and releases in order.
func (p *testProvider) lifecycle() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.calls...)
}

// retune moves a ref on by one field, through the same helper the example
// server uses — so only that leaf is encoded again, and the path the example
// takes is the path exercised here end to end.
func (p *testProvider) retune(t *testing.T, ref string, mtu int) server.Head {
	t.Helper()

	p.mu.Lock()
	data, ok := p.held[ref]
	p.mu.Unlock()

	if !ok {
		t.Fatalf("no data prepared for %q", ref)
	}

	root, err := data.Retune(mtu)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return server.Head{Hash: root}
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

	srv, err := server.New(server.WithProvider(provider))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return &harness{server: srv, provider: provider, addr: serve(t, srv)}
}

// serve starts srv on its own listener and returns where to reach it. The
// listener is stopped, along with the server, when the test ends.
func serve(t *testing.T, srv *server.Server) string {
	t.Helper()

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

	return lis.Addr().String()
}

// subscribe connects a client to ref and waits for nothing; the caller decides
// when to read.
func (h *harness) subscribe(t *testing.T, ctx context.Context, ref string) (*client.Client, <-chan client.Update) {
	t.Helper()

	return dial(t, ctx, h.addr, ref)
}

// dial connects to a treevial server at addr and subscribes to ref, waiting
// for nothing; the caller decides when to read.
func dial(t *testing.T, ctx context.Context, addr, ref string) (*client.Client, <-chan client.Update) {
	t.Helper()

	c, err := client.Dial(ctx, addr)
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

	eventually(t, fmt.Sprintf("%q synced at %s", ref, h), func() bool {
		for _, sub := range srv.Subscribers() {
			if sub.Ref == ref && sub.Synced == h {
				return true
			}
		}

		return false
	})
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
