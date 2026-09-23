package e2e

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/demo/shared"
	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/receive"
	"github.com/zonque/treevial/server"
	"github.com/zonque/treevial/structtree"
)

// elsewhere stands for the node that actually owns a ref: a store and the
// builder that moves it. In a cluster this would be the leader, reached over
// whatever the application uses to talk between its own servers; here it is
// the same process, which is beside the point — what matters is that the
// server serving the client has none of it.
type elsewhere struct {
	store   *objects.Store
	config  *shared.Config
	builder *structtree.Builder
	head    plumbing.Hash
}

func newElsewhere(t *testing.T, ref string) *elsewhere {
	t.Helper()

	e := &elsewhere{store: objects.NewStore(), config: shared.Example(ref)}

	builder, err := structtree.NewBuilder(e.store, e.config)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	e.builder = builder

	if e.head, err = builder.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	return e
}

// move changes one field, the way the owning node would, and returns the head
// its consensus layer would then tell the others about.
func (e *elsewhere) move(t *testing.T, mtu int) plumbing.Hash {
	t.Helper()

	e.config.Network.Primary.MTU = mtu

	head, err := e.builder.Build(&e.config.Network.Primary.MTU)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	e.head = head

	return head
}

// pack answers what a forwarded push asks for, which is what the owning node
// would compute to answer one.
func (e *elsewhere) pack(have, want plumbing.Hash) (*server.Pack, error) {
	hashes, err := e.store.SelectSince(have, want)
	if err != nil {
		return nil, err
	}

	if len(hashes) == 0 {
		return &server.Pack{}, nil
	}

	var body bytes.Buffer
	if _, err := e.store.EncodePack(&body, hashes); err != nil {
		return nil, err
	}

	return &server.Pack{Objects: len(hashes), Body: &body}, nil
}

// forwarder is the hook under test: it decides, for each push, whether this
// server answers it from its own objects or fetches them from elsewhere.
type forwarder struct {
	remote *elsewhere

	mu sync.Mutex
	// on says whether this server currently believes somebody else owns
	// the ref. A cluster flips this as it re-elects.
	on bool
	// forwarded records the head of every push this hook answered.
	forwarded []plumbing.Hash
	// fail, when set, is what the hook reports instead of a pack.
	fail error
	// block, when set, is closed by the hook before it waits for the
	// context to be cancelled; what the context then says goes to stopped.
	block   chan struct{}
	stopped chan error
	// withheld, when set, is returned instead of a real pack.
	withheld *server.Pack
}

func (f *forwarder) Forward(ctx context.Context, req server.Push) (*server.Pack, error) {
	f.mu.Lock()
	on, fail, block, withheld := f.on, f.fail, f.block, f.withheld
	f.mu.Unlock()

	if !on {
		// Served here, exactly as if there were no hook at all.
		return nil, nil
	}

	if block != nil {
		close(block)
		<-ctx.Done()

		f.stopped <- ctx.Err()

		return nil, ctx.Err()
	}

	if fail != nil {
		return nil, fail
	}

	f.mu.Lock()
	f.forwarded = append(f.forwarded, req.Want)
	f.mu.Unlock()

	if withheld != nil {
		return withheld, nil
	}

	return f.remote.pack(req.Have, req.Want)
}

func (f *forwarder) set(fn func(*forwarder)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}

func (f *forwarder) pushes() []plumbing.Hash {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]plumbing.Hash(nil), f.forwarded...)
}

// stubProvider is the provider of a server that is not the one holding the
// data: it hands over whatever store the test gave it, which is usually an
// empty one, and the head its consensus layer last told it about.
type stubProvider struct {
	mu    sync.Mutex
	store *objects.Store
	head  plumbing.Hash
}

func (p *stubProvider) Prepare(string) (*objects.Store, plumbing.Hash, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.store, p.head, nil
}

func (p *stubProvider) Release(string) {}

// serving starts a server on its own listener and returns where to reach it.
func serving(t *testing.T, provider server.Provider, f server.Forwarder) (*server.Server, string) {
	t.Helper()

	srv := server.New(provider)
	if f != nil {
		srv.Forward(f)
	}

	return srv, serve(t, srv)
}

func TestAServerWithNoObjectsServesAClientFromElsewhere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	remote := newElsewhere(t, refA)

	// This server holds nothing at all: every object the client receives
	// has to have been fetched.
	provider := &stubProvider{store: objects.NewStore(), head: remote.head}
	hook := &forwarder{remote: remote, on: true}

	srv, addr := serving(t, provider, hook)

	_, updates := dial(t, ctx, addr, refA)

	u := nextUpdate(t, updates)

	if u.Hash != remote.head {
		t.Errorf("client was served %s, want %s", u.Hash, remote.head)
	}
	if want := 17; u.ObjectCount != want {
		t.Errorf("client was sent %d objects, want %d", u.ObjectCount, want)
	}

	leaves, err := u.Graph.Leaves(u.Hash)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}
	if want := shared.LeafCount; len(leaves) != want {
		t.Fatalf("client reconstructed %d leaves, want %d", len(leaves), want)
	}

	// And it follows a move, which its own consensus layer tells it about
	// by moving the ref — the objects again coming from elsewhere.
	next := remote.move(t, 9000)
	if err := srv.SetHead(refA, next); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	u = followTo(t, updates, next)

	if want := 4; u.ObjectCount != want {
		t.Errorf("the move cost %d objects, want %d", u.ObjectCount, want)
	}

	leaves, err = u.Graph.Leaves(u.Hash)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}
	if got := string(leaves["Network/Primary/MTU"]); got != "9000" {
		t.Errorf("Network/Primary/MTU = %q, want %q", got, "9000")
	}
}

func TestWhereAPushIsServedFromIsDecidedEveryTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	remote := newElsewhere(t, refA)

	// This server can serve from its own objects as well, so the two paths
	// are interchangeable and the client has no way to tell them apart.
	local := objects.NewStore()
	localConfig := shared.Example(refA)

	builder, err := structtree.NewBuilder(local, localConfig)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := builder.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	provider := &stubProvider{store: local, head: remote.head}
	hook := &forwarder{remote: remote, on: true}

	srv, addr := serving(t, provider, hook)

	_, updates := dial(t, ctx, addr, refA)

	first := nextUpdate(t, updates)

	// The cluster re-elects: this server now owns the ref and serves the
	// next push itself, on the same connection.
	hook.set(func(f *forwarder) { f.on = false })

	second := remote.move(t, 9000)

	// The same change here, so this server's own objects reach the same
	// head — which is what content addressing means and what makes the
	// two paths interchangeable.
	localConfig.Network.Primary.MTU = 9000

	local2, err := builder.Build(&localConfig.Network.Primary.MTU)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if local2 != second {
		t.Fatalf("the two nodes built different trees: %s and %s", local2, second)
	}

	if err := srv.SetHead(refA, second); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	followTo(t, updates, second)

	// And it loses the election again before the third.
	hook.set(func(f *forwarder) { f.on = true })

	third := remote.move(t, 4000)
	if err := srv.SetHead(refA, third); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	last := followTo(t, updates, third)

	leaves, err := last.Graph.Leaves(last.Hash)
	if err != nil {
		t.Fatalf("Leaves: %v", err)
	}
	if got := string(leaves["Network/Primary/MTU"]); got != "4000" {
		t.Errorf("Network/Primary/MTU = %q, want %q", got, "4000")
	}

	// The first and the third were fetched; the second was not. One
	// connection, three pushes, two different answers to where from.
	forwarded := hook.pushes()
	if len(forwarded) != 2 {
		t.Fatalf("%d pushes were forwarded, want 2", len(forwarded))
	}
	if forwarded[0] != first.Hash || forwarded[1] != third {
		t.Errorf("forwarded %v, want the first and third heads (%s, %s)", forwarded, first.Hash, third)
	}
}

func TestAFailedFetchIsReportedToTheClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	remote := newElsewhere(t, refA)

	provider := &stubProvider{store: objects.NewStore(), head: remote.head}
	hook := &forwarder{
		remote: remote,
		on:     true,
		fail:   treevial.Errorf(treevial.CodeInternal, "no leader elected"),
	}

	_, addr := serving(t, provider, hook)

	c, err := client.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	updates, err := c.Subscribe(ctx, refA)
	if err == nil {
		err = drainForError(t, c, updates)
	}

	if got := treevial.CodeOf(err); got != treevial.CodeInternal {
		t.Errorf("got error %v (code %s), want %s", err, got, treevial.CodeInternal)
	}
}

func TestAnEmptyForwardedPackSaysThereIsNothingToSend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	remote := newElsewhere(t, refA)

	provider := &stubProvider{store: objects.NewStore(), head: remote.head}
	hook := &forwarder{remote: remote, on: true}

	_, addr := serving(t, provider, hook)

	// A client that already holds the head is sent nothing, and the
	// fetch that says so carries no pack at all.
	c, err := client.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	updates, err := c.Resume(ctx, refA, remote.head, receive.NewGraph())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	u := nextUpdate(t, updates)

	if u.ObjectCount != 0 {
		t.Errorf("client was sent %d objects, want none", u.ObjectCount)
	}
	if u.Hash != remote.head {
		t.Errorf("client was told %s, want %s", u.Hash, remote.head)
	}
}

func TestAPackThatPromisesObjectsItDoesNotCarryIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	remote := newElsewhere(t, refA)

	provider := &stubProvider{store: objects.NewStore(), head: remote.head}
	hook := &forwarder{
		remote: remote,
		on:     true,
		// Seventeen objects announced and nothing to send: writing the
		// update anyway would leave the client waiting on a pack that
		// never comes.
		withheld: &server.Pack{Objects: 17},
	}

	_, addr := serving(t, provider, hook)

	c, err := client.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	updates, err := c.Subscribe(ctx, refA)
	if err == nil {
		err = drainForError(t, c, updates)
	}

	if got := treevial.CodeOf(err); got != treevial.CodeInternal {
		t.Errorf("got error %v (code %s), want %s", err, got, treevial.CodeInternal)
	}
}

func TestAFetchIsAbandonedWhenItsClientHangsUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	remote := newElsewhere(t, refA)

	provider := &stubProvider{store: objects.NewStore(), head: remote.head}
	blocked := make(chan struct{})
	stopped := make(chan error, 1)
	hook := &forwarder{remote: remote, on: true, block: blocked, stopped: stopped}

	_, addr := serving(t, provider, hook)

	c, err := client.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	if _, err := c.Subscribe(ctx, refA); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the fetch was never attempted")
	}

	// The client goes away while the fetch is outstanding. The context it
	// was given is the connection's, so the fetch is told to stop rather
	// than left waiting on a client that is no longer there.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the fetch stopped with %v, want it cancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fetch was never told the client had gone")
	}
}
