// Package server is the pushing side of treevial. It listens for clients, but
// the direction of the object flow is reversed from git's usual arrangement:
// once a client has identified itself, the server prepares that client's
// objects and sends them down the long-lived connection on its own initiative,
// whenever the client's ref moves.
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

// Provider supplies and disposes of the objects belonging to one client. The
// server owns neither: it asks for a client's data when that client connects
// and hands it back when the client goes away.
type Provider interface {
	// Prepare builds the objects for a newly connected client and returns
	// the store holding them together with the hash its ref points at.
	Prepare(clientID string) (*objects.Store, plumbing.Hash, error)
	// Release is called once, after the client has disconnected, so
	// whatever Prepare set up can be dropped.
	Release(clientID string)
}

// ClientState is a snapshot of what the server knows about one connected
// client.
type ClientState struct {
	// ID the client identified itself with.
	ID string
	// Ref its data is published at.
	Ref string
	// Head is the hash that ref points at.
	Head plumbing.Hash
	// Synced is the hash the client has confirmed it fully interpreted, or
	// the zero hash if it has not caught up yet.
	Synced plumbing.Hash
}

// client is one connected client. Its whole state is the hash it last
// acknowledged: holding a tree means holding everything under it, so a single
// hash is enough for the server to work out what the client still needs.
type client struct {
	id     string
	ref    string
	store  *objects.Store
	notify chan plumbing.Hash

	mu     sync.Mutex
	head   plumbing.Hash
	synced plumbing.Hash
}

func (c *client) state() ClientState {
	c.mu.Lock()
	defer c.mu.Unlock()

	return ClientState{ID: c.id, Ref: c.ref, Head: c.head, Synced: c.synced}
}

func (c *client) held() plumbing.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.synced
}

// setHead moves the client's ref.
func (c *client) setHead(hash plumbing.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.head = hash
}

func (c *client) currentHead() plumbing.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.head
}

// ack records that the client interpreted everything up to hash.
func (c *client) ack(hash plumbing.Hash) {
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

	mu      sync.Mutex
	clients map[string]*client
	conns   map[*wire.Conn]struct{}
	lis     net.Listener
	stopped bool
}

// New returns a server that asks provider for each client's objects.
func New(provider Provider) *Server {
	return &Server{
		provider: provider,
		clients:  map[string]*client{},
		conns:    map[*wire.Conn]struct{}{},
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

// SetHead points a client's ref at hash and immediately wakes it. This is the
// trigger for a push: nothing is requested by the client.
func (s *Server) SetHead(clientID string, hash plumbing.Hash) error {
	s.mu.Lock()
	c, ok := s.clients[clientID]
	s.mu.Unlock()

	if !ok || c == nil {
		return fmt.Errorf("no client %q is connected", clientID)
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

// Head returns the hash a client's ref points at, or the zero hash if no such
// client is connected.
func (s *Server) Head(clientID string) plumbing.Hash {
	s.mu.Lock()
	c, ok := s.clients[clientID]
	s.mu.Unlock()

	if !ok || c == nil {
		return plumbing.ZeroHash
	}

	return c.currentHead()
}

// Clients snapshots the connected clients.
func (s *Server) Clients() []ClientState {
	s.mu.Lock()
	connected := make([]*client, 0, len(s.clients))
	for _, c := range s.clients {
		// A nil entry is an ID claimed by a connection whose data is
		// still being prepared.
		if c != nil {
			connected = append(connected, c)
		}
	}
	s.mu.Unlock()

	out := make([]ClientState, 0, len(connected))
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
	clientID, synced, err := register(conn)
	if err != nil {
		return err
	}

	c, err := s.connect(clientID, synced)
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

// register reads the opening message, in which the client names itself and
// states what it already holds.
func register(conn *wire.Conn) (string, plumbing.Hash, error) {
	msg, err := conn.ReadClientMessage()
	if err != nil {
		return "", plumbing.ZeroHash, err
	}

	if msg.Kind != wire.Register {
		return "", plumbing.ZeroHash, treevial.Errorf(treevial.CodeInvalid,
			"first message must be a registration")
	}

	// The ID is interpolated into a ref path, so it is checked here, before
	// it reaches anything that builds a path from it.
	if err := treevial.ValidateID(msg.ClientID); err != nil {
		return "", plumbing.ZeroHash, treevial.Errorf(treevial.CodeInvalid, "%v", err)
	}

	return msg.ClientID, msg.Hash, nil
}

// connect prepares the client's objects and registers it. Only one connection
// per client ID is served at a time, so a client's prepared data has exactly
// one owner.
func (s *Server) connect(clientID string, synced plumbing.Hash) (*client, error) {
	s.mu.Lock()
	if _, taken := s.clients[clientID]; taken {
		s.mu.Unlock()

		return nil, treevial.Errorf(treevial.CodeAlreadyExists,
			"client %q is already connected", clientID)
	}
	// Claim the ID before preparing, so a second connection racing this one
	// cannot have data prepared for it too.
	s.clients[clientID] = nil
	s.mu.Unlock()

	store, head, err := s.provider.Prepare(clientID)
	if err != nil {
		s.mu.Lock()
		delete(s.clients, clientID)
		s.mu.Unlock()

		return nil, treevial.Errorf(treevial.CodeInternal,
			"prepare data for %q: %v", clientID, err)
	}

	c := &client{
		id:     clientID,
		ref:    treevial.RefFor(clientID),
		store:  store,
		notify: make(chan plumbing.Hash, 1),
		head:   head,
		synced: synced,
	}

	s.mu.Lock()
	s.clients[clientID] = c
	s.mu.Unlock()

	return c, nil
}

// disconnect is the other half of connect: it runs when a client's connection
// ends, however it ended, and gives the provider its chance to let go.
func (s *Server) disconnect(c *client) {
	s.mu.Lock()
	// Only drop the entry if it is still this connection's.
	if current, ok := s.clients[c.id]; ok && current == c {
		delete(s.clients, c.id)
	}
	s.mu.Unlock()

	s.provider.Release(c.id)
}

// awaitAck blocks until the client confirms it has interpreted the update, so
// the server's idea of the client's state never runs ahead of reality.
func awaitAck(c *client, hash plumbing.Hash, acks <-chan plumbing.Hash, recvErr <-chan error) error {
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
func (s *Server) push(conn *wire.Conn, c *client, hash plumbing.Hash) error {
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
			return fmt.Errorf("server: unexpected %s after registration", msg.Kind)
		}

		select {
		case acks <- msg.Hash:
		default:
			// An unread acknowledgement is stale by definition; the
			// newest one is what matters.
		}
	}
}
