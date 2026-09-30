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
// A server may name itself, and every client that registers is told that name
// — a label for logs, not a credential, and not something the server acts on.
//
// A subscriber that vanishes rather than disconnecting holds everything it had
// — a place in [Server.Subscribers], and a ref that can never be released.
// [WithDeadPeerTimeout] bounds how long it may.
//
// A ref's head is a hash and, where something orders it, an ordinal: see
// [Head] for what that is for, and what a ref gains by having one — chiefly
// that it then moves only forward.
//
// A server repository depends on this package and on
// [github.com/zonque/treevial/objects]; it does not need the client side at
// all.
package server

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/internal/tcpkeep"
	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/objects"
)

// Head is where a ref points, and — behind a consensus layer — which state
// that is.
//
// Sequence is an ordinal supplied by whatever owns the ref: a log index, or a
// revision its state machine keeps. treevial requires one property of it and
// checks exactly that one — per ref, it must increase whenever the head moves
// — and derives nothing else from its value. It must therefore come out of the
// replicated state rather than from a counter a node keeps locally: a node
// that counts its own pushes agrees with its peers until one of them restores
// from a snapshot, and never again after.
//
// A zero Sequence means the ref is unsequenced, which is what a server with
// nothing to order against uses. Nothing about such a ref is checked, and
// nothing about it is written on the wire.
//
// Equal sequences mean equal heads. Unequal sequences do not mean unequal
// heads: with a log index used directly, two nodes at different indices hold
// the same head for every ref the entries between them did not touch. So an
// application comparing two of its clients tests Hash for agreement and
// Sequence only for ordering.
type Head struct {
	Hash     plumbing.Hash
	Sequence uint64
}

// Errors reported by [Server.SetHead]. Both happen in the ordinary running of
// a cluster, so both are named rather than left to be matched on text: a node
// applies entries for every ref it replicates while only some of them have
// subscribers here, and a head that has already been passed is what a
// re-delivered entry carries.
var (
	ErrNoSubscribers = errors.New("treevial/server: nobody is subscribed to that ref")
	ErrNotAdvancing  = errors.New("treevial/server: head does not advance")
)

// Provider supplies and disposes of the objects behind one ref. The server owns
// neither: it asks for a ref's data when the first client subscribes to it, and
// hands it back once the last one has disconnected.
//
// Prepare and Release are called once per ref, not once per connection, and
// never overlap for the same ref: a Release always completes before that ref
// can be prepared again. Several subscribers to one ref share what Prepare
// returned, and objects may be added to that store while they are reading it:
// adding cannot change what an existing root reaches. Nothing can be removed,
// which is why reclaiming a store means a new one rather than a pruned one.
// [Server.Subscribers] is how to tell a ref is quiet, for callers who need
// that for their own reasons.
//
// The ref is whatever the client asked for, passed on unchanged. What it means
// — a device, a tenant, a configuration — is the provider's business; the
// server only checks that it is a usable ref name.
type Provider interface {
	// Prepare builds the objects behind ref and returns the store holding
	// them together with the head the ref points at.
	//
	// That head must be reachable in the store returned alongside it, and
	// so must every head later given to [Server.SetHead] for this ref: the
	// server works out a subscriber's objects from it, and one that is not
	// there refuses the subscriber rather than merely leaving it behind.
	Prepare(ref string) (*objects.Store, Head, error)
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
	// Head is where ref points. Subscribers of one ref all see the same
	// head; what differs is how much of it they have taken up.
	Head Head
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
	head Head
	// sequenced says whether this ref is ordered, and installed whether
	// anybody has given it a head at all. The kind is fixed by whichever of
	// the provider and the application installs one first: a SetHead may
	// arrive while a preparation is still running, and must not block
	// waiting for it.
	sequenced bool
	installed bool
}

func newRefState() *refState {
	return &refState{
		ready:   make(chan struct{}),
		retired: make(chan struct{}),
		members: map[*subscriber]struct{}{},
	}
}

func (st *refState) currentHead() Head {
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.head
}

// moveHead points the ref at head if head is allowed to move it, and says
// whether it did. Every subscriber following the ref is behind until it says
// otherwise.
//
// A sequenced ref moves only forward: a head at a sequence the ref has reached
// already is a re-delivered one, and taking it would announce a state the
// subscribers have been told about. An unsequenced ref moves unconditionally,
// because with nothing to compare there is no way to tell which of two heads
// is the newer — which is what an ordinal is for.
func (st *refState) moveHead(head Head) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if !st.installed {
		st.head = head
		st.sequenced = head.Sequence != 0
		st.installed = true

		return true, nil
	}

	if st.sequenced != (head.Sequence != 0) {
		return false, treevial.Errorf(treevial.CodeInvalid,
			"a %s head on a %s ref: ordering across a mixture is undefined",
			kind(head.Sequence != 0), kind(st.sequenced))
	}

	if st.sequenced && head.Sequence <= st.head.Sequence {
		return false, nil
	}

	st.head = head

	return true, nil
}

// kind names what moveHead refused, so its message reads as a sentence.
func kind(sequenced bool) string {
	if sequenced {
		return "sequenced"
	}

	return "unsequenced"
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
	// id is the name this server gives itself, or empty if it was not
	// given one. Set once by an Option before Serve and never written
	// again, so it needs no lock.
	id string
	// deadPeer is how long a subscriber may be unresponsive before its
	// connection is given up on, or zero to leave that to the system. Set
	// once before Serve, so it needs no lock either.
	deadPeer time.Duration

	mu      sync.Mutex
	refs    map[string]*refState
	conns   map[*wire.Conn]struct{}
	lis     net.Listener
	stopped bool
	watcher Watcher
}

// Option configures a Server as it is created. An option that cannot be
// satisfied reports why, and [New] returns that error rather than a server.
type Option func(*Server) error

// New returns a server that asks provider for each ref's objects.
func New(provider Provider, opts ...Option) (*Server, error) {
	s := &Server{
		provider: provider,
		refs:     map[string]*refState{},
		conns:    map[*wire.Conn]struct{}{},
	}

	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// WithID gives the server a name of its own, which it tells every client that
// registers with it.
//
// It is a label for whoever reads the client's logs: something better than an
// address to recognise a server by, and a way for a client to notice it
// reached one it did not mean to. Nothing is decided by it — the server serves
// exactly the same whether or not it is named, and a client derives nothing
// from it. It is not a credential: a server can claim any name, exactly as a
// client can.
//
// A server that is not given one sends no such line at all, and is byte for
// byte on the wire what it has always been. Naming a server is therefore also
// choosing to require clients that understand the line.
func WithID(id string) Option {
	return func(s *Server) error {
		if err := treevial.ValidateServerID(id); err != nil {
			return treevial.Errorf(treevial.CodeInvalid, "%v", err)
		}

		s.id = id

		return nil
	}
}

// WithDeadPeerTimeout bounds how long a subscriber may be unresponsive before
// its connection is given up on.
//
// A subscriber that vanishes without closing — its host gone, its process
// killed — otherwise keeps everything it held. It stays in [Server.Subscribers]
// as though it were still following, its ref never has a last subscriber to
// leave, so [Provider.Release] never runs and the objects behind that ref are
// never handed back. Bounding the connection is what lets all of that be
// reclaimed.
//
// One duration drives both mechanisms that can notice: keepalive probes start
// halfway through the budget and run four times across the rest, and where
// TCP_USER_TIMEOUT exists it bounds unacknowledged data. A subscriber that
// dies mid-push is caught by the second alone, and so today only on Linux.
//
// The schedule is worked out in whole seconds, which is the unit the kernel
// keeps it in, so the shortest budget that can be expressed is eight seconds
// and rounding may cost a little more than was asked for — never less.
//
// Left unset, nothing changes: connections keep the plain 30-second keepalive
// they have always had, and a quiet subscriber is never disturbed.
func WithDeadPeerTimeout(d time.Duration) Option {
	return func(s *Server) error {
		// Zero is how "unset" is spelled, not a budget of nothing, and
		// it reads the same here as it does on the client — which
		// matters when both are driven from one piece of configuration.
		if d == 0 {
			s.deadPeer = 0

			return nil
		}

		if err := tcpkeep.Validate(d); err != nil {
			return treevial.Errorf(treevial.CodeInvalid, "%v", err)
		}

		s.deadPeer = d

		return nil
	}
}

// ID reports the name this server gave itself, or the empty string if it was
// not given one.
func (s *Server) ID() string {
	return s.id
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
			s.tune(tcp)
		}

		go s.handle(conn)
	}
}

// tune sets how long this connection may go unanswered before it is given up
// on. Failures are ignored, as they already were: a connection whose liveness
// could not be tuned is still a usable connection, and there is nobody to tell
// — the client it would concern has not registered yet.
func (s *Server) tune(conn *net.TCPConn) {
	if s.deadPeer == 0 {
		_ = conn.SetKeepAlive(true)
		_ = conn.SetKeepAlivePeriod(keepalivePeriod)

		return
	}

	live := tcpkeep.For(s.deadPeer)

	_ = conn.SetKeepAliveConfig(live.KeepAlive)

	if raw, err := conn.SyscallConn(); err == nil {
		_ = tcpkeep.SetUserTimeout(raw, live.UserTimeout)
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
func (s *Server) SetHead(ref string, head Head) error {
	s.mu.Lock()
	st, ok := s.living(ref)

	var following []*subscriber
	if ok {
		following = make([]*subscriber, 0, len(st.members))
		for c := range st.members {
			following = append(following, c)
		}
	}
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("set head of %q: %w", ref, ErrNoSubscribers)
	}

	moved, err := st.moveHead(head)
	if err != nil {
		return fmt.Errorf("set head of %q: %w", ref, err)
	}

	// Only a ref that moved has anything to say. Waking on an apply that
	// did not move one would announce a state its subscribers have already
	// been told about.
	if !moved {
		return fmt.Errorf("set head of %q to sequence %d: %w", ref, head.Sequence, ErrNotAdvancing)
	}

	for _, c := range following {
		c.wake()
	}

	return nil
}

// Head returns where ref points, or the zero [Head] if nobody is subscribed to
// it.
func (s *Server) Head(ref string) Head {
	s.mu.Lock()
	st, ok := s.living(ref)
	s.mu.Unlock()

	if !ok {
		return Head{}
	}

	return st.currentHead()
}

// living returns the state for ref if the ref is one that somebody is
// subscribed to. An entry being retired — its last subscriber gone, its data
// on the way back to the provider — stays in the map so the ref cannot be
// prepared again while that release is still running, but it no longer stands
// for a ref anyone can read or move. The caller holds s.mu.
func (s *Server) living(ref string) (*refState, bool) {
	st, ok := s.refs[ref]
	if !ok || st.retiring {
		return nil, false
	}

	return st, true
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

	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()

		conn.Close()
	}()

	err := s.serve(conn, nc.RemoteAddr().String())

	// A refusal is told to the client; anything else is the connection
	// itself going away, and there is nobody left to tell.
	var reported *treevial.Error
	if errors.As(err, &reported) {
		_ = conn.WriteError(reported)
	}
}

// serve registers the client, pushes what it is missing, and then stays put,
// pushing again every time its ref moves.
func (s *Server) serve(conn *wire.Conn, addr string) error {
	msg, err := register(conn)
	if err != nil {
		return err
	}

	// Sent before the ref is prepared, so a client refused because its ref
	// could not be prepared still learns which server refused it. A
	// registration that does not parse is refused above this, and gets no
	// name — there is nothing yet to say it to.
	if s.id != "" {
		if err := conn.WriteServerID(s.id); err != nil {
			return err
		}
	}

	c, err := s.connect(conn, addr, msg)
	if err != nil {
		return err
	}
	defer s.disconnect(c)

	acks := make(chan plumbing.Hash, 1)
	recvErr := make(chan error, 1)

	// The read side is what notices a client going away, and while a push
	// is in hand it is the only thing that does.
	go func() { recvErr <- readAcks(conn, acks) }()

	// Push what the client is missing right away, then on every ref change.
	pending := c.state.currentHead()

	for {
		if err := s.push(conn, c, pending); err != nil {
			return err
		}

		if err := awaitAck(c, pending.Hash, acks, recvErr); err != nil {
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
		s.forget(ref, st)
	} else if _, err := st.moveHead(head); err != nil {
		// The application has already given this ref a head of the other
		// kind, so which of the two is newer has no answer. Refusing the
		// subscriber says so; serving it would not.
		st.err = treevial.Errorf(treevial.CodeInternal,
			"prepare data for %q: %v", ref, err)
		s.forget(ref, st)
	} else {
		// A head that does not advance is not a failure: the application
		// has moved this ref past what the provider built, and the ref is
		// where it should be.
		st.store = store
		st.prepared = true
	}

	close(st.ready)
}

// forget drops a ref whose preparation failed, so that the next client to ask
// for it has the provider tried again rather than inheriting this answer.
func (s *Server) forget(ref string, st *refState) {
	s.mu.Lock()
	if s.refs[ref] == st {
		delete(s.refs, ref)
	}
	s.mu.Unlock()
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

// push sends the client the objects between the tree it holds and the head.
// What that is depends on the subscriber, not on the ref: two clients
// following one ref from different starting points are sent different objects.
func (s *Server) push(conn *wire.Conn, c *subscriber, head Head) error {
	missing, err := c.state.store.SelectSince(c.held(), head.Hash)
	if err != nil {
		return treevial.Errorf(treevial.CodeInvalid, "objects since %s: %v", c.held(), err)
	}

	if err := conn.WriteUpdate(head.Hash, len(missing), head.Sequence); err != nil {
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
