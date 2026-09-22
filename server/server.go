// Package server is the pushing side of treevial. It listens for clients, but
// the direction of the object flow is reversed from git's usual arrangement:
// once a client has named the head it wants, the server prepares that ref's
// objects and sends them down the long-lived connection on its own initiative,
// whenever the ref moves.
//
// Any number of clients may follow the same ref. They are served from one set
// of prepared objects and each moves at its own pace, so a slow subscriber
// delays nobody else.
//
// A server need not own what it serves: a [Forwarder] fetches a push's objects
// from wherever the ref actually lives, which lets a cluster move a ref
// between its nodes without the clients following it noticing anything.
//
// A server repository depends on this package and on
// [github.com/zonque/treevial/objects]; it does not need the client side at
// all.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/objects"
)

// Provider supplies and disposes of the objects behind one ref. The server owns
// neither: it asks for a ref's data when the first client subscribes to it, and
// hands it back once the last one has disconnected.
//
// Prepare and Release are called once per ref, not once per connection, and
// never overlap for the same ref: a Release always completes before that ref
// can be prepared again. Several subscribers to one ref share what Prepare
// returned, so a store must not be written to while any of them may be reading
// it — see [Server.Subscribers] for how to tell.
//
// The ref is whatever the client asked for, passed on unchanged. What it means
// — a device, a tenant, a configuration — is the provider's business; the
// server only checks that it is a usable ref name.
type Provider interface {
	// Prepare builds the objects behind ref and returns the store holding
	// them together with the hash the ref points at.
	Prepare(ref string) (*objects.Store, plumbing.Hash, error)
	// Release is called once, after the last subscriber to ref has
	// disconnected, so whatever Prepare set up can be dropped.
	Release(ref string)
}

// Subscription is a snapshot of what the server knows about one connected
// subscriber. Several subscribers may share a Ref, in which case they differ in
// Addr, in how far they have got, and in what they have been sent.
type Subscription struct {
	// Ref the subscriber asked for, verbatim.
	Ref string
	// ClientID is what the client called itself when it registered, or
	// empty if it did not name itself. It is a label to recognise a
	// connection by: the server derives nothing from it, requires nothing
	// of it, and two subscribers may share one.
	ClientID string
	// Addr is where the subscriber connected from, which is what tells two
	// subscribers of one ref apart even when they are named alike.
	Addr string
	// Head is the hash that ref points at. Subscribers of one ref all see
	// the same head; what differs is how much of it they have taken up.
	Head plumbing.Hash
	// Synced is the hash the subscriber has confirmed it fully interpreted,
	// or the zero hash if it has not caught up yet.
	Synced plumbing.Hash
	// Sent is how many bytes have gone out on this subscriber's connection
	// since it was accepted: every update and its pack, pkt-line framing
	// included. Received is the other direction, which for a client is one
	// registration and one acknowledgement per push.
	Sent     int64
	Received int64
}

// Forwarder serves a push from somewhere other than this server's own
// objects.
//
// It exists for the case where the ref a client is following is owned by
// another server — a cluster that elects a leader, say. The client knows
// nothing of it: its connection stays where it is, and the objects are
// fetched behind it and written on as ordinary updates. Whatever decides who
// owns what, and however the servers talk among themselves, is the
// application's business; this is only where the answer is asked for.
//
// Forward is consulted for every push, not once per connection, so a cluster
// may reorganise under a live subscription and the next push follows the new
// arrangement. It is called from the goroutine serving that subscriber, so a
// slow fetch delays that one client and nobody else.
//
// Moving the ref is a separate matter and stays with the application: a
// server that does not own a ref still learns from its own consensus layer
// that the ref has moved, and says so with [Server.SetHead]. A forwarder
// alone pushes nothing, because nothing has told the server there is anything
// to push.
type Forwarder interface {
	// Forward returns the objects the subscriber needs to get from Have to
	// Want. A nil Pack means this server serves the push from its own
	// store, exactly as it would with no forwarder at all.
	//
	// The context is the client connection's: it is cancelled when that
	// client goes away, so a fetch is not left outstanding for a
	// subscriber that no longer exists.
	Forward(ctx context.Context, req Push) (*Pack, error)
}

// ForwarderFunc lets a plain function be a [Forwarder].
type ForwarderFunc func(ctx context.Context, req Push) (*Pack, error)

// Forward implements Forwarder.
func (f ForwarderFunc) Forward(ctx context.Context, req Push) (*Pack, error) {
	return f(ctx, req)
}

// Push is one subscriber's need for objects, as put to a [Forwarder].
type Push struct {
	// Ref the subscriber is following, verbatim.
	Ref string
	// ClientID is what the client called itself, and Addr where it
	// connected from. Neither decides anything; they are here so a
	// forwarder can log and account for what it fetches.
	ClientID string
	Addr     string
	// Have is the tree this subscriber has acknowledged, or the zero hash
	// if it holds nothing. Want is the head it should reach. The objects
	// asked for are those between them, for this subscriber alone.
	Have plumbing.Hash
	Want plumbing.Hash
}

// Pack is what a [Forwarder] answers with: a count, and the packfile carrying
// that many objects.
type Pack struct {
	// Objects is how many objects Body carries. It is announced to the
	// client before the pack is, so it must be the pack's own count.
	Objects int
	// Body is a packfile. It may be nil when Objects is zero, which is how
	// a forwarder says the subscriber is already current. If Body is an
	// io.Closer it is closed once the pack has been written on.
	Body io.Reader
}

// EventKind says what happened to a subscription.
type EventKind int

const (
	// Subscribed is a client joining a ref, once its objects are ready.
	Subscribed EventKind = iota
	// Synced is a client acknowledging a head it was pushed. When the
	// event's Synced equals its Head, that client is up to date.
	Synced
	// Unsubscribed is a client's connection ending, however it ended.
	Unsubscribed
)

// String implements fmt.Stringer.
func (k EventKind) String() string {
	switch k {
	case Subscribed:
		return "subscribed"
	case Synced:
		return "synced"
	case Unsubscribed:
		return "unsubscribed"
	}

	return "unknown"
}

// Event is something that happened to one subscription, together with how that
// subscription stood when it happened.
type Event struct {
	Kind EventKind
	Subscription
}

// Watcher is told about subscriptions as they come, catch up and go, so that
// an application can follow them without polling [Server.Subscribers].
//
// Observe is called from the goroutine serving that subscriber, with no lock
// of the server's held: a handler may call back into the server — Subscribers,
// Head, SetHead — without deadlocking. It is called in order for any one
// subscriber, and concurrently for different ones, so a handler must be safe
// to call from several goroutines at once.
//
// A handler runs while its subscriber waits, which delays that subscriber's
// next push and nobody else's. Anything slow belongs on a goroutine of the
// handler's own.
type Watcher interface {
	Observe(Event)
}

// WatcherFunc lets a plain function be a [Watcher].
type WatcherFunc func(Event)

// Observe implements Watcher.
func (f WatcherFunc) Observe(e Event) { f(e) }

// refState is everything the server holds for one ref: the objects behind it,
// the hash it points at, and the subscribers following it. It outlives any one
// connection — the first subscriber brings it into being and the last one to
// leave takes it away again.
type refState struct {
	// ready is closed once the provider has answered. store, prepared and
	// err are written before that and only read after it.
	ready    chan struct{}
	store    *objects.Store
	prepared bool
	err      error

	// retiring is set when the last subscriber has gone and the provider is
	// being given its data back; retired is closed once that has happened.
	// Together they keep a ref from being prepared again while its previous
	// release is still running. Both, like members, are guarded by the
	// server's mutex: they change as connections come and go, which is the
	// registry's business rather than this value's.
	retiring bool
	retired  chan struct{}
	members  map[*subscriber]struct{}

	mu   sync.Mutex
	head plumbing.Hash
}

func newRefState() *refState {
	return &refState{
		ready:   make(chan struct{}),
		retired: make(chan struct{}),
		members: map[*subscriber]struct{}{},
	}
}

func (st *refState) currentHead() plumbing.Hash {
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.head
}

// setHead moves the ref. Every subscriber following it is behind until it says
// otherwise.
func (st *refState) setHead(hash plumbing.Hash) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.head = hash
}

// subscriber is one connected client. Its whole state is the hash it last
// acknowledged: holding a tree means holding everything under it, so a single
// hash is enough for the server to work out what it still needs. The objects
// and the head it is being moved towards belong to the ref, not to it.
type subscriber struct {
	ref      string
	clientID string
	addr     string
	conn     *wire.Conn
	state    *refState
	// announced records that a watcher was told about this subscriber, so
	// that its departure is reported only if its arrival was. It is
	// touched only by the goroutine serving the connection.
	announced bool
	// notify is a signal rather than a queue: a woken subscriber reads the
	// ref's current head, so a burst of moves cannot leave it at an old one.
	notify chan struct{}

	mu     sync.Mutex
	synced plumbing.Hash
}

func (c *subscriber) snapshot() Subscription {
	c.mu.Lock()
	synced := c.synced
	c.mu.Unlock()

	// The connection keeps its own tally and is safe to ask at any time, so
	// the byte counts need no locking of their own.
	return Subscription{
		Ref:      c.ref,
		ClientID: c.clientID,
		Addr:     c.addr,
		Head:     c.state.currentHead(),
		Synced:   synced,
		Sent:     c.conn.BytesWritten(),
		Received: c.conn.BytesRead(),
	}
}

func (c *subscriber) held() plumbing.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.synced
}

// ack records that the client interpreted everything up to hash.
func (c *subscriber) ack(hash plumbing.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.synced = hash
}

// wake tells the subscriber its ref has moved.
func (c *subscriber) wake() {
	select {
	case c.notify <- struct{}{}:
	default:
		// A wake-up is already pending, and one is as good as ten: the
		// subscriber reads the current head when it gets to it.
	}
}

// keepalivePeriod is how often the operating system probes an idle connection.
// Probes keep NATs and middleboxes from forgetting a connection that may sit
// quiet for hours; they do not close a healthy one.
const keepalivePeriod = 30 * time.Second

// Server publishes a set of refs and pushes the objects behind them to
// everyone following.
type Server struct {
	provider Provider

	mu        sync.Mutex
	refs      map[string]*refState
	conns     map[*wire.Conn]struct{}
	lis       net.Listener
	stopped   bool
	watcher   Watcher
	forwarder Forwarder
}

// New returns a server that asks provider for each ref's objects.
func New(provider Provider) *Server {
	return &Server{
		provider: provider,
		refs:     map[string]*refState{},
		conns:    map[*wire.Conn]struct{}{},
	}
}

// Watch registers w to be told about subscriptions as they come, catch up and
// go. It replaces any previous watcher, and a nil one turns reporting off.
//
// Set it before Serve: a watcher registered while clients are already
// connected hears about them from their next event on, not retrospectively.
func (s *Server) Watch(w Watcher) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.watcher = w
}

// Forward registers f to be asked, for every push, whether this server serves
// it from its own objects or fetches them from elsewhere. It replaces any
// previous forwarder, and a nil one goes back to serving everything locally.
//
// Set it before Serve.
func (s *Server) Forward(f Forwarder) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.forwarder = f
}

// forward returns the forwarder in force, or nil.
func (s *Server) forward() Forwarder {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.forwarder
}

// report tells the watcher, if there is one, how a subscriber stands now. It
// is called with no lock held, which is what lets a handler ask the server
// anything it likes.
func (s *Server) report(kind EventKind, c *subscriber) {
	s.mu.Lock()
	w := s.watcher
	s.mu.Unlock()

	if w == nil {
		return
	}

	w.Observe(Event{Kind: kind, Subscription: c.snapshot()})
}

// Serve accepts connections on lis until Stop is called.
//
// No deadline is ever set on a connection: the server pushes when it has
// something to say, which may be hours after the last byte, and a deadline
// would tear down a perfectly good connection in the meantime.
func (s *Server) Serve(lis net.Listener) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		lis.Close()

		return nil
	}
	s.lis = lis
	s.mu.Unlock()

	for {
		conn, err := lis.Accept()
		if err != nil {
			if s.isStopped() {
				return nil
			}

			return err
		}

		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetKeepAlive(true)
			_ = tcp.SetKeepAlivePeriod(keepalivePeriod)
		}

		go s.handle(conn)
	}
}

// Stop shuts the server down and drops every client.
func (s *Server) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()

		return
	}
	s.stopped = true

	lis := s.lis
	conns := make([]*wire.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	if lis != nil {
		lis.Close()
	}
	for _, c := range conns {
		c.Close()
	}
}

// SetHead points ref at hash and immediately wakes everyone following it. This
// is the trigger for a push: nothing is requested by the client.
//
// Every subscriber to the ref moves. What each is sent is worked out from what
// it has acknowledged, so one that is several moves behind is brought up to the
// current head in one go rather than walked through the states it missed.
func (s *Server) SetHead(ref string, hash plumbing.Hash) error {
	s.mu.Lock()
	st, ok := s.refs[ref]

	var following []*subscriber
	if ok {
		following = make([]*subscriber, 0, len(st.members))
		for c := range st.members {
			following = append(following, c)
		}
	}
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("nobody is subscribed to %q", ref)
	}

	st.setHead(hash)

	for _, c := range following {
		c.wake()
	}

	return nil
}

// Head returns the hash ref points at, or the zero hash if nobody is
// subscribed to it.
func (s *Server) Head(ref string) plumbing.Hash {
	s.mu.Lock()
	st, ok := s.refs[ref]
	s.mu.Unlock()

	if !ok {
		return plumbing.ZeroHash
	}

	return st.currentHead()
}

// Subscribers snapshots the connected subscribers, one entry per connection.
// Several entries may share a Ref.
//
// This is how a provider tells whether a ref is quiet: when every subscriber to
// it reports Synced equal to Head, no push is in flight and its store can be
// rebuilt.
func (s *Server) Subscribers() []Subscription {
	s.mu.Lock()
	var connected []*subscriber

	for _, st := range s.refs {
		// A ref whose data is still being prepared has nothing to report
		// yet, and one whose preparation failed never will.
		select {
		case <-st.ready:
		default:
			continue
		}

		if !st.prepared {
			continue
		}

		for c := range st.members {
			connected = append(connected, c)
		}
	}
	s.mu.Unlock()

	out := make([]Subscription, 0, len(connected))
	for _, c := range connected {
		out = append(out, c.snapshot())
	}

	return out
}

func (s *Server) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.stopped
}

// handle serves one connection for its whole life. When it returns, the client
// has disconnected and, if it was the last one following its ref, that ref's
// resources are released.
func (s *Server) handle(nc net.Conn) {
	conn := wire.NewConn(nc)

	// Everything done on this client's behalf is done under a context of
	// its own, so nothing outlives the connection it was for.
	ctx, done := context.WithCancel(context.Background())
	defer done()

	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()

		conn.Close()
	}()

	err := s.serve(ctx, conn, nc.RemoteAddr().String())

	// A refusal is told to the client; anything else is the connection
	// itself going away, and there is nobody left to tell.
	var reported *treevial.Error
	if errors.As(err, &reported) {
		_ = conn.WriteError(reported)
	}
}

// serve registers the client, pushes what it is missing, and then stays put,
// pushing again every time its ref moves.
func (s *Server) serve(ctx context.Context, conn *wire.Conn, addr string) error {
	msg, err := register(conn)
	if err != nil {
		return err
	}

	c, err := s.connect(conn, addr, msg)
	if err != nil {
		return err
	}
	defer s.disconnect(c)

	acks := make(chan plumbing.Hash, 1)
	recvErr := make(chan error, 1)

	// The read side is what notices a client going away, and it is the
	// only thing that does while a push is in hand: cancelling here is
	// what stops a fetch being made on behalf of somebody who has hung up.
	ctx, hungUp := context.WithCancel(ctx)
	defer hungUp()

	go func() {
		err := readAcks(conn, acks)
		hungUp()
		recvErr <- err
	}()

	// Push what the client is missing right away, then on every ref change.
	pending := c.state.currentHead()

	for {
		if err := s.push(ctx, conn, c, pending); err != nil {
			return err
		}

		if err := awaitAck(c, pending, acks, recvErr); err != nil {
			return err
		}

		s.report(Synced, c)

		select {
		case <-c.notify:
			// However many times the ref moved while this client was
			// being brought up to date, what it owes it now is the
			// head as it stands.
			pending = c.state.currentHead()
		case err := <-recvErr:
			return err
		}
	}
}

// register reads the opening message, in which the client names the head it
// wants and states what it already holds.
func register(conn *wire.Conn) (wire.ClientMessage, error) {
	msg, err := conn.ReadClientMessage()
	if err != nil {
		return wire.ClientMessage{}, err
	}

	if msg.Type != wire.Register {
		return wire.ClientMessage{}, treevial.Errorf(treevial.CodeInvalid,
			"first message must be a registration")
	}

	// The ref is taken as it was sent — the server derives nothing from it
	// — but it still has to be a ref, since the provider will key on it.
	if err := treevial.ValidateRef(msg.Ref); err != nil {
		return wire.ClientMessage{}, treevial.Errorf(treevial.CodeInvalid, "%v", err)
	}

	// The name is only ever shown to whoever runs the server, but it is
	// still checked here rather than trusted: it was sent by the client.
	if err := treevial.ValidateClientID(msg.ClientID); err != nil {
		return wire.ClientMessage{}, treevial.Errorf(treevial.CodeInvalid, "%v", err)
	}

	return msg, nil
}

// connect joins the subscriber to its ref, preparing that ref's objects if it
// is the first to ask for them. Everyone who arrives while a preparation is
// running waits for that one answer rather than asking for another.
func (s *Server) connect(
	conn *wire.Conn,
	addr string,
	msg wire.ClientMessage,
) (*subscriber, error) {
	ref := msg.Ref

	c := &subscriber{
		ref:      ref,
		clientID: msg.ClientID,
		addr:     addr,
		conn:     conn,
		notify:   make(chan struct{}, 1),
		synced:   msg.Hash,
	}

	var first bool

	for {
		s.mu.Lock()
		st, following := s.refs[ref]

		if following && st.retiring {
			// The last subscriber has just left and the provider is
			// being given this ref's data back. Let that finish, then
			// start again from a clean slate.
			s.mu.Unlock()
			<-st.retired

			continue
		}

		if !following {
			st = newRefState()
			s.refs[ref] = st
			first = true
		}

		st.members[c] = struct{}{}
		s.mu.Unlock()

		c.state = st

		break
	}

	if first {
		s.prepare(c.state, ref)
	}

	// Either this connection's own preparation or the one it joined.
	<-c.state.ready

	if err := c.state.err; err != nil {
		s.disconnect(c)

		return nil, err
	}

	c.announced = true
	s.report(Subscribed, c)

	return c, nil
}

// prepare asks the provider for a ref's objects, once, and lets everyone
// waiting on the answer through.
func (s *Server) prepare(st *refState, ref string) {
	store, head, err := s.provider.Prepare(ref)
	if err != nil {
		st.err = treevial.Errorf(treevial.CodeInternal,
			"prepare data for %q: %v", ref, err)

		// A failed preparation is not remembered: the entry goes, so the
		// next client to ask for this ref has the provider tried again
		// rather than inheriting this answer.
		s.mu.Lock()
		if s.refs[ref] == st {
			delete(s.refs, ref)
		}
		s.mu.Unlock()
	} else {
		st.store = store
		st.prepared = true
		st.setHead(head)
	}

	close(st.ready)
}

// disconnect is the other half of connect: it runs when a subscriber's
// connection ends, however it ended. The provider gets its data back only once
// the last subscriber to that ref has gone.
func (s *Server) disconnect(c *subscriber) {
	st := c.state

	s.mu.Lock()
	delete(st.members, c)
	// Only this ref's last subscriber retires it, and only while the entry
	// is still the one it joined.
	last := len(st.members) == 0 && s.refs[c.ref] == st
	if last {
		// The entry stays in place, marked, until the provider has been
		// given its data back, so the ref cannot be prepared again while
		// this release is still running.
		st.retiring = true
	}
	s.mu.Unlock()

	// Reported here, after the subscriber has stopped being one of the
	// ref's members and so stopped showing up in Subscribers, but before
	// the provider is given anything back.
	if c.announced {
		s.report(Unsubscribed, c)
	}

	if !last {
		return
	}

	if st.prepared {
		s.provider.Release(c.ref)
	}

	s.mu.Lock()
	if s.refs[c.ref] == st {
		delete(s.refs, c.ref)
	}
	s.mu.Unlock()

	close(st.retired)
}

// awaitAck blocks until the client confirms it has interpreted the update, so
// the server's idea of the client's state never runs ahead of reality.
func awaitAck(c *subscriber, hash plumbing.Hash, acks <-chan plumbing.Hash, recvErr <-chan error) error {
	for {
		select {
		case acked := <-acks:
			if acked != hash {
				// A stale acknowledgement for an earlier push.
				continue
			}
			c.ack(hash)

			return nil
		case err := <-recvErr:
			return err
		}
	}
}

// push sends the client the objects between the tree it holds and hash. What
// that is depends on the subscriber, not on the ref: two clients following one
// ref from different starting points are sent different objects.
//
// Where the objects come from is asked afresh every time, so a server that has
// just stopped owning a ref — or just started — serves the next push
// accordingly, on the connection it already has.
func (s *Server) push(ctx context.Context, conn *wire.Conn, c *subscriber, hash plumbing.Hash) error {
	pack, err := s.fetch(ctx, c, hash)
	if err != nil {
		return err
	}

	if pack != nil {
		return sendPack(conn, hash, pack)
	}

	missing, err := c.state.store.SelectSince(c.held(), hash)
	if err != nil {
		return treevial.Errorf(treevial.CodeInvalid, "objects since %s: %v", c.held(), err)
	}

	if err := conn.WriteUpdate(hash, len(missing)); err != nil {
		return err
	}

	// The pack is framed as it is encoded, so it goes out while it is still
	// being produced.
	w := conn.PackWriter()

	if len(missing) > 0 {
		if _, err := c.state.store.EncodePack(w, missing); err != nil {
			return treevial.Errorf(treevial.CodeInternal, "encode pack: %v", err)
		}
	}

	return w.Close()
}

// fetch asks the forwarder, if there is one, for this subscriber's objects. A
// nil pack and a nil error mean this server serves the push itself.
func (s *Server) fetch(ctx context.Context, c *subscriber, hash plumbing.Hash) (*Pack, error) {
	f := s.forward()
	if f == nil {
		return nil, nil
	}

	pack, err := f.Forward(ctx, Push{
		Ref:      c.ref,
		ClientID: c.clientID,
		Addr:     c.addr,
		Have:     c.held(),
		Want:     hash,
	})
	if err != nil {
		// A forwarder that reports a treevial error has said how it
		// wants the client told; anything else is this server failing
		// to do its job.
		var reported *treevial.Error
		if errors.As(err, &reported) {
			return nil, reported
		}

		return nil, treevial.Errorf(treevial.CodeInternal,
			"fetch objects for %q: %v", c.ref, err)
	}

	return pack, nil
}

// sendPack writes a pack that came from somewhere else, framed by this server
// as though it had encoded it: the client cannot tell the difference, and the
// protocol does not know there is one.
func sendPack(conn *wire.Conn, hash plumbing.Hash, pack *Pack) error {
	if closer, ok := pack.Body.(io.Closer); ok {
		defer closer.Close()
	}

	// An update that announces objects and then carries none would leave
	// the client waiting on a pack that never comes, so it is refused here
	// rather than written.
	if pack.Objects > 0 && pack.Body == nil {
		return treevial.Errorf(treevial.CodeInternal,
			"forwarded pack announces %d objects but carries none", pack.Objects)
	}

	if err := conn.WriteUpdate(hash, pack.Objects); err != nil {
		return err
	}

	w := conn.PackWriter()

	if pack.Body != nil {
		if _, err := io.Copy(w, pack.Body); err != nil {
			return treevial.Errorf(treevial.CodeInternal,
				"forwarded pack: %v", err)
		}
	}

	return w.Close()
}

// readAcks drains the client's half of the connection. Registration aside, the
// only thing a client sends is an acknowledgement.
func readAcks(conn *wire.Conn, acks chan<- plumbing.Hash) error {
	for {
		msg, err := conn.ReadClientMessage()
		if err != nil {
			return err
		}

		if msg.Type != wire.Ack {
			return treevial.Errorf(treevial.CodeInvalid,
				"unexpected %s after registration", msg.Type)
		}

		select {
		case acks <- msg.Hash:
		default:
			// An unread acknowledgement is stale by definition; the
			// newest one is what matters.
		}
	}
}
