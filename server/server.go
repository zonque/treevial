// Package server is the pushing side of treevial. It listens for clients, but
// the direction of the object flow is reversed from git's usual arrangement:
// once a client has named the head it wants, the server prepares that ref's
// objects and sends them down the long-lived connection on its own initiative,
// whenever the ref moves.
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
	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/objects"
)

// Provider supplies and disposes of the objects behind one ref. The server owns
// neither: it asks for a ref's data when a client subscribes to it, and hands
// it back when that client goes away.
//
// The ref is whatever the client asked for, passed on unchanged. What it means
// — a device, a tenant, a configuration — is the provider's business; the
// server only checks that it is a usable ref name.
type Provider interface {
	// Prepare builds the objects behind ref and returns the store holding
	// them together with the hash the ref points at.
	Prepare(ref string) (*objects.Store, plumbing.Hash, error)
	// Release is called once, after the subscriber has disconnected, so
	// whatever Prepare set up can be dropped.
	Release(ref string)
}

// Subscription is a snapshot of what the server knows about one connected
// subscriber.
type Subscription struct {
	// Ref the subscriber asked for, verbatim.
	Ref string
	// Head is the hash that ref points at.
	Head plumbing.Hash
	// Synced is the hash the subscriber has confirmed it fully interpreted,
	// or the zero hash if it has not caught up yet.
	Synced plumbing.Hash
}

// subscriber is one connected client. Its whole state is the hash it last
// acknowledged: holding a tree means holding everything under it, so a single
// hash is enough for the server to work out what it still needs.
type subscriber struct {
	ref    string
	store  *objects.Store
	notify chan plumbing.Hash

	mu     sync.Mutex
	head   plumbing.Hash
	synced plumbing.Hash
}

func (c *subscriber) state() Subscription {
	c.mu.Lock()
	defer c.mu.Unlock()

	return Subscription{Ref: c.ref, Head: c.head, Synced: c.synced}
}

func (c *subscriber) held() plumbing.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.synced
}

// setHead moves the client's ref.
func (c *subscriber) setHead(hash plumbing.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.head = hash
}

func (c *subscriber) currentHead() plumbing.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.head
}

// ack records that the client interpreted everything up to hash.
func (c *subscriber) ack(hash plumbing.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.synced = hash
}

// keepalivePeriod is how often the operating system probes an idle connection.
// Probes keep NATs and middleboxes from forgetting a connection that may sit
// quiet for hours; they do not close a healthy one.
const keepalivePeriod = 30 * time.Second

// Server publishes a ref per client and pushes the objects behind it.
type Server struct {
	provider Provider

	mu          sync.Mutex
	subscribers map[string]*subscriber
	conns       map[*wire.Conn]struct{}
	lis         net.Listener
	stopped     bool
}

// New returns a server that asks provider for each client's objects.
func New(provider Provider) *Server {
	return &Server{
		provider:    provider,
		subscribers: map[string]*subscriber{},
		conns:       map[*wire.Conn]struct{}{},
	}
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

// SetHead points ref at hash and immediately wakes its subscriber. This is the
// trigger for a push: nothing is requested by the client.
func (s *Server) SetHead(ref string, hash plumbing.Hash) error {
	s.mu.Lock()
	c, ok := s.subscribers[ref]
	s.mu.Unlock()

	if !ok || c == nil {
		return fmt.Errorf("nobody is subscribed to %q", ref)
	}

	c.setHead(hash)

	select {
	case c.notify <- hash:
	default:
		// A push is already queued for this client; it will pick up the
		// newest head when it runs.
	}

	return nil
}

// Head returns the hash ref points at, or the zero hash if nobody is
// subscribed to it.
func (s *Server) Head(ref string) plumbing.Hash {
	s.mu.Lock()
	c, ok := s.subscribers[ref]
	s.mu.Unlock()

	if !ok || c == nil {
		return plumbing.ZeroHash
	}

	return c.currentHead()
}

// Subscribers snapshots the connected subscribers.
func (s *Server) Subscribers() []Subscription {
	s.mu.Lock()
	connected := make([]*subscriber, 0, len(s.subscribers))
	for _, c := range s.subscribers {
		// A nil entry is a ref claimed by a connection whose data is
		// still being prepared.
		if c != nil {
			connected = append(connected, c)
		}
	}
	s.mu.Unlock()

	out := make([]Subscription, 0, len(connected))
	for _, c := range connected {
		out = append(out, c.state())
	}

	return out
}

func (s *Server) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.stopped
}

// handle serves one connection for its whole life. When it returns, the client
// has disconnected and its resources are released.
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

	err := s.serve(conn)

	// A refusal is told to the client; anything else is the connection
	// itself going away, and there is nobody left to tell.
	var reported *treevial.Error
	if errors.As(err, &reported) {
		_ = conn.WriteError(reported)
	}
}

// serve registers the client, pushes what it is missing, and then stays put,
// pushing again every time its ref moves.
func (s *Server) serve(conn *wire.Conn) error {
	ref, synced, err := register(conn)
	if err != nil {
		return err
	}

	c, err := s.connect(ref, synced)
	if err != nil {
		return err
	}
	defer s.disconnect(c)

	acks := make(chan plumbing.Hash, 1)
	recvErr := make(chan error, 1)

	go func() { recvErr <- readAcks(conn, acks) }()

	// Push what the client is missing right away, then on every ref change.
	pending := c.currentHead()

	for {
		if err := s.push(conn, c, pending); err != nil {
			return err
		}

		if err := awaitAck(c, pending, acks, recvErr); err != nil {
			return err
		}

		select {
		case pending = <-c.notify:
		case err := <-recvErr:
			return err
		}
	}
}

// register reads the opening message, in which the client names the head it
// wants and states what it already holds.
func register(conn *wire.Conn) (string, plumbing.Hash, error) {
	msg, err := conn.ReadClientMessage()
	if err != nil {
		return "", plumbing.ZeroHash, err
	}

	if msg.Kind != wire.Register {
		return "", plumbing.ZeroHash, treevial.Errorf(treevial.CodeInvalid,
			"first message must be a registration")
	}

	// The ref is taken as it was sent — the server derives nothing from it
	// — but it still has to be a ref, since the provider will key on it.
	if err := treevial.ValidateRef(msg.Ref); err != nil {
		return "", plumbing.ZeroHash, treevial.Errorf(treevial.CodeInvalid, "%v", err)
	}

	return msg.Ref, msg.Hash, nil
}

// connect prepares the ref's objects and registers the subscriber. Only one
// connection per ref is served at a time, so prepared data has exactly one
// owner.
func (s *Server) connect(ref string, synced plumbing.Hash) (*subscriber, error) {
	s.mu.Lock()
	if _, taken := s.subscribers[ref]; taken {
		s.mu.Unlock()

		return nil, treevial.Errorf(treevial.CodeAlreadyExists,
			"%q already has a subscriber", ref)
	}
	// Claim the ref before preparing, so a second connection racing this
	// one cannot have data prepared for it too.
	s.subscribers[ref] = nil
	s.mu.Unlock()

	store, head, err := s.provider.Prepare(ref)
	if err != nil {
		s.mu.Lock()
		delete(s.subscribers, ref)
		s.mu.Unlock()

		return nil, treevial.Errorf(treevial.CodeInternal,
			"prepare data for %q: %v", ref, err)
	}

	c := &subscriber{
		ref:    ref,
		store:  store,
		notify: make(chan plumbing.Hash, 1),
		head:   head,
		synced: synced,
	}

	s.mu.Lock()
	s.subscribers[ref] = c
	s.mu.Unlock()

	return c, nil
}

// disconnect is the other half of connect: it runs when a subscriber's
// connection ends, however it ended, and gives the provider its chance to let
// go.
func (s *Server) disconnect(c *subscriber) {
	s.mu.Lock()
	// Only drop the entry if it is still this connection's.
	if current, ok := s.subscribers[c.ref]; ok && current == c {
		delete(s.subscribers, c.ref)
	}
	s.mu.Unlock()

	s.provider.Release(c.ref)
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

// push sends the client the objects between the tree it holds and hash.
func (s *Server) push(conn *wire.Conn, c *subscriber, hash plumbing.Hash) error {
	missing, err := c.store.SelectSince(c.held(), hash)
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
		if _, err := c.store.EncodePack(w, missing); err != nil {
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

		if msg.Kind != wire.Ack {
			return treevial.Errorf(treevial.CodeInvalid,
				"unexpected %s after registration", msg.Kind)
		}

		select {
		case acks <- msg.Hash:
		default:
			// An unread acknowledgement is stale by definition; the
			// newest one is what matters.
		}
	}
}
