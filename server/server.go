// Package server is the pushing side of treevial. It listens for clients, but
// the direction of the object flow is reversed from git's usual arrangement:
// once a client has identified itself, the server prepares that client's
// objects and sends them down the long-lived stream on its own initiative,
// whenever the client's ref moves.
//
// A server repository depends on this package and on
// [github.com/holoplot/treevial/objects]; it does not need the client side at
// all.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/holoplot/treevial"
	"github.com/holoplot/treevial/internal/treevialpb"
	"github.com/holoplot/treevial/objects"
)

// chunkSize caps how much pack data travels in one PackChunk message.
const chunkSize = 32 * 1024

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

// Server publishes a ref per client and pushes the objects behind it.
type Server struct {
	provider Provider
	grpc     *grpc.Server

	mu      sync.Mutex
	clients map[string]*client
}

// New returns a server that asks provider for each client's objects.
func New(provider Provider) *Server {
	s := &Server{
		provider: provider,
		clients:  map[string]*client{},
	}

	// A treevial connection carries pushes the server initiates, so it must
	// outlive any amount of quiet. Every mechanism gRPC has for closing a
	// connection on its own is disabled here.
	s.grpc = grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// Never close a connection for being idle or old.
			MaxConnectionIdle:     forever,
			MaxConnectionAge:      forever,
			MaxConnectionAgeGrace: forever,
			// Never probe the client, and so never conclude from a
			// missing probe reply that it has gone away.
			Time:    forever,
			Timeout: forever,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			// The smallest non-zero interval, because zero means
			// "use the five-minute default". Without this, a client
			// that pings more often than every five minutes gets
			// GOAWAY ENHANCE_YOUR_CALM and its connection closed
			// after three strikes.
			MinTime: minPingInterval,
			// Client pings are welcome even with no stream open.
			PermitWithoutStream: true,
		}),
	)
	treevialpb.RegisterObjectSyncServer(s.grpc, &syncService{server: s})

	return s
}

// syncService adapts the generated gRPC service to the Server. It exists so
// that the wire types, which are an implementation detail, stay out of the
// Server's exported API.
type syncService struct {
	treevialpb.UnimplementedObjectSyncServer

	server *Server
}

// Sync implements the generated ObjectSyncServer interface.
func (s *syncService) Sync(stream treevialpb.ObjectSync_SyncServer) error {
	return s.server.sync(stream)
}

// Serve accepts connections on lis until Stop is called.
func (s *Server) Serve(lis net.Listener) error {
	if err := s.grpc.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}

	return nil
}

// Stop shuts the server down and drops every client.
func (s *Server) Stop() {
	s.grpc.Stop()
}

// SetHead points a client's ref at hash and immediately wakes it. This is the
// trigger for a push: nothing is requested by the client.
func (s *Server) SetHead(clientID string, hash plumbing.Hash) error {
	s.mu.Lock()
	c, ok := s.clients[clientID]
	s.mu.Unlock()

	if !ok {
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

	if !ok {
		return plumbing.ZeroHash
	}

	return c.currentHead()
}

// Clients snapshots the connected clients.
func (s *Server) Clients() []ClientState {
	s.mu.Lock()
	connected := make([]*client, 0, len(s.clients))
	for _, c := range s.clients {
		connected = append(connected, c)
	}
	s.mu.Unlock()

	out := make([]ClientState, 0, len(connected))
	for _, c := range connected {
		out = append(out, c.state())
	}

	return out
}

// sync serves one client for the whole life of its connection: it identifies
// the client from the request header, has its objects prepared, pushes what the
// client is missing, and then stays put, pushing again every time the ref
// moves. When it returns, the client has disconnected and its resources are
// released.
func (s *Server) sync(stream treevialpb.ObjectSync_SyncServer) error {
	clientID, err := clientIDFrom(stream.Context())
	if err != nil {
		return err
	}

	synced, err := readRegistration(stream)
	if err != nil {
		return err
	}

	c, err := s.connect(clientID, synced)
	if err != nil {
		return err
	}
	defer s.disconnect(c)

	acks := make(chan *treevialpb.Ack, 1)
	recvErr := make(chan error, 1)

	go func() { recvErr <- readAcks(stream, acks) }()

	// Push what the client is missing right away, then on every ref change.
	pending := c.currentHead()

	for {
		if err := s.push(stream, c, pending); err != nil {
			return err
		}

		if err := s.awaitAck(stream, c, pending, acks, recvErr); err != nil {
			return err
		}

		select {
		case pending = <-c.notify:
		case err := <-recvErr:
			return err
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// connect prepares the client's objects and registers it. Only one connection
// per client ID is served at a time, so a client's prepared data has exactly
// one owner.
func (s *Server) connect(clientID string, synced plumbing.Hash) (*client, error) {
	s.mu.Lock()
	if _, taken := s.clients[clientID]; taken {
		s.mu.Unlock()

		return nil, status.Errorf(codes.AlreadyExists, "client %q is already connected", clientID)
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

		return nil, status.Errorf(codes.Internal, "prepare data for %q: %v", clientID, err)
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

// disconnect is the other half of connect: it runs when a client's stream ends,
// however it ended, and gives the provider its chance to let go.
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
func (s *Server) awaitAck(
	stream treevialpb.ObjectSync_SyncServer,
	c *client,
	hash plumbing.Hash,
	acks <-chan *treevialpb.Ack,
	recvErr <-chan error,
) error {
	for {
		select {
		case ack := <-acks:
			if ack.GetHash() != hash.String() {
				// A stale acknowledgement for an earlier push.
				continue
			}
			c.ack(hash)

			return nil
		case err := <-recvErr:
			return err
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// clientIDFrom reads and validates the client's identity from the request
// header. The ID names the ref the client is served, so it is checked here
// before it reaches anything that builds a path from it.
func clientIDFrom(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Errorf(codes.InvalidArgument, "request carries no metadata")
	}

	values := md.Get(treevial.IDHeader)
	if len(values) == 0 {
		return "", status.Errorf(codes.InvalidArgument, "request has no %s header", treevial.IDHeader)
	}
	if len(values) > 1 {
		return "", status.Errorf(codes.InvalidArgument, "request has %d %s headers", len(values), treevial.IDHeader)
	}

	clientID := values[0]
	if err := treevial.ValidateID(clientID); err != nil {
		return "", status.Errorf(codes.InvalidArgument, "%v", err)
	}

	return clientID, nil
}

// readRegistration reads the opening message of the stream, in which the client
// states what it already holds.
func readRegistration(stream treevialpb.ObjectSync_SyncServer) (plumbing.Hash, error) {
	msg, err := stream.Recv()
	if err != nil {
		return plumbing.ZeroHash, err
	}

	reg := msg.GetRegister()
	if reg == nil {
		return plumbing.ZeroHash, status.Error(codes.InvalidArgument, "first message must be a registration")
	}

	synced := plumbing.ZeroHash
	if hex := reg.GetSynced(); hex != "" {
		if synced = plumbing.NewHash(hex); synced.IsZero() {
			return plumbing.ZeroHash, status.Errorf(codes.InvalidArgument, "malformed synced hash %q", hex)
		}
	}

	return synced, nil
}

// push sends the client the objects between the tree it holds and hash.
func (s *Server) push(stream treevialpb.ObjectSync_SyncServer, c *client, hash plumbing.Hash) error {
	missing, err := c.store.SelectSince(c.held(), hash)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "objects since %s: %v", c.held(), err)
	}

	begin := &treevialpb.ServerMsg{Body: &treevialpb.ServerMsg_Begin{Begin: &treevialpb.UpdateBegin{
		Hash:        hash.String(),
		ObjectCount: uint32(len(missing)),
	}}}
	if err := stream.Send(begin); err != nil {
		return err
	}

	if len(missing) > 0 {
		w := &chunkWriter{stream: stream}

		if _, err := c.store.EncodePack(w, missing); err != nil {
			return status.Errorf(codes.Internal, "encode pack: %v", err)
		}
		if err := w.flush(); err != nil {
			return err
		}
	}

	end := &treevialpb.ServerMsg{Body: &treevialpb.ServerMsg_End{End: &treevialpb.UpdateEnd{}}}

	return stream.Send(end)
}

// readAcks drains the client's half of the stream. Registration aside, the only
// thing a client sends is an acknowledgement.
func readAcks(stream treevialpb.ObjectSync_SyncServer, acks chan<- *treevialpb.Ack) error {
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		if ack := msg.GetAck(); ack != nil {
			select {
			case acks <- ack:
			default:
				// An unread acknowledgement is stale by
				// definition; the newest one is what matters.
			}
		}
	}
}

// chunkWriter turns the pack encoder's writes into PackChunk messages, so the
// pack streams out as it is produced instead of being assembled first and sent
// in one lump.
type chunkWriter struct {
	stream treevialpb.ObjectSync_SyncServer
	buf    []byte
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)

	for len(w.buf) >= chunkSize {
		if err := w.send(w.buf[:chunkSize]); err != nil {
			return 0, err
		}
		w.buf = w.buf[chunkSize:]
	}

	return len(p), nil
}

// flush sends whatever is left in the buffer.
func (w *chunkWriter) flush() error {
	defer func() { w.buf = nil }()

	return w.send(w.buf)
}

func (w *chunkWriter) send(b []byte) error {
	if len(b) == 0 {
		return nil
	}

	return w.stream.Send(&treevialpb.ServerMsg{Body: &treevialpb.ServerMsg_Chunk{
		Chunk: &treevialpb.PackChunk{Data: b},
	}})
}
