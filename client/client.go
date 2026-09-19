// Package client is the receiving side of treevial. It dials the server and
// keeps one connection open for the lifetime of the process, but never asks for
// anything after the first message: it names the head it wants and then simply
// interprets whatever the server pushes, on the fly, without writing a byte to
// disk.
//
// A caller names that head itself. This package attaches no meaning to a ref's
// shape and knows of no scheme for deriving one — how an application decides
// which ref a given device, tenant or installation should follow is entirely
// its own business.
//
// A client repository depends on this package and on
// [github.com/zonque/treevial/receive]; it does not need the server side at
// all.
package client

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/receive"
)

// keepalivePeriod is how often the operating system probes an idle connection.
// Probes keep NATs and middleboxes from forgetting a connection that may sit
// quiet for hours; they do not close a healthy one.
const keepalivePeriod = 30 * time.Second

// Update reports one completed push: the ref that moved, the object it now
// points at, what it pointed at before, how many objects the server had to
// send, what the push cost on the wire, and the accumulated graph the client
// has interpreted so far.
type Update struct {
	// Ref the update arrived for: the head this client asked for.
	Ref  string
	Hash plumbing.Hash
	// Previous is the hash this subscription was at before the update, or
	// the zero hash for the first one. Graph.Diff(Previous, Hash) is what
	// turns an update into a list of changed paths, and ApplySince uses the
	// pair to decode only what moved.
	Previous    plumbing.Hash
	ObjectCount int
	// Bytes is what this push cost on the wire: the update message, the
	// pack that followed it, and the pkt-line framing around both. It is
	// measured, not estimated from the object count.
	Bytes int64
	// TotalBytes is everything this connection has received since it was
	// opened, this push included.
	TotalBytes int64
	Graph      *receive.Graph
}

// Client is a connection to a treevial server. One connection carries one
// subscription, so Subscribe or Resume is called once per Client.
type Client struct {
	conn *wire.Conn
	id   string

	mu  sync.Mutex
	err error
}

// Option configures a Client as it is dialled.
type Option func(*Client)

// WithID gives the client a name of its own, which it sends to the server on
// the opening message.
//
// It is a label for whoever runs the server, so that connections can be told
// apart in a listing or a log. Nothing is decided by it: the ref is what says
// what a client is served, the server derives nothing from the name and does
// not require it to be unique, and a client that does not name itself is
// served exactly the same.
func WithID(id string) Option {
	return func(c *Client) { c.id = id }
}

// Dial connects to a treevial server. See [WithID] for naming the client.
//
// No deadline is ever set on the connection: the server pushes when it has
// something to say, which may be hours after the last byte, and a deadline
// would tear down a perfectly good connection in the meantime.
func Dial(ctx context.Context, addr string, opts ...Option) (*Client, error) {
	var c Client

	for _, opt := range opts {
		opt(&c)
	}

	// Checked before anything is dialled, so an unusable name costs no
	// connection at all.
	if err := treevial.ValidateClientID(c.id); err != nil {
		return nil, treevial.Errorf(treevial.CodeInvalid, "%v", err)
	}

	dialer := net.Dialer{KeepAlive: keepalivePeriod}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	c.conn = wire.NewConn(conn)

	return &c, nil
}

// ID reports the name this client gave itself, or the empty string if it did
// not name itself.
func (c *Client) ID() string {
	return c.id
}

// Close hangs up.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Received reports how many bytes have arrived on this connection since it was
// dialled, pkt-line framing included. Together with [Client.Sent] it is what
// the subscription has cost in traffic.
func (c *Client) Received() int64 {
	return c.conn.BytesRead()
}

// Sent reports how many bytes this client has put on the wire: one
// registration and one acknowledgement per push, which is everything a client
// ever says.
func (c *Client) Sent() int64 {
	return c.conn.BytesWritten()
}

// Err returns the error that ended the subscription, if any.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

func (c *Client) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err == nil {
		c.err = err
	}
}

// Subscribe subscribes to ref as a client holding nothing, and returns a
// channel of updates the server pushes. The server prepares that ref's data as
// the client connects.
func (c *Client) Subscribe(ctx context.Context, ref string) (<-chan Update, error) {
	return c.Resume(ctx, ref, plumbing.ZeroHash, receive.NewGraph())
}

// Resume subscribes to ref, declaring that the client already holds the tree at
// synced, whose objects are in graph. That single hash is the whole of the
// client's state: the server sends only the difference between it and the head.
// Later updates accumulate into the same graph.
//
// Cancelling ctx closes the connection, which ends the subscription and closes
// the channel.
func (c *Client) Resume(
	ctx context.Context,
	ref string,
	synced plumbing.Hash,
	graph *receive.Graph,
) (<-chan Update, error) {
	// Checked here as well as on the server, so an unusable ref fails
	// without a round trip; the rule itself lives in one place.
	if err := treevial.ValidateRef(ref); err != nil {
		return nil, treevial.Errorf(treevial.CodeInvalid, "%v", err)
	}

	if err := c.conn.WriteRegister(ref, synced, c.id); err != nil {
		return nil, err
	}

	updates := make(chan Update)
	done := make(chan struct{})

	// A blocked read does not notice a cancelled context; closing the
	// connection under it does.
	go func() {
		select {
		case <-ctx.Done():
			c.conn.Close()
		case <-done:
		}
	}()

	go func() {
		defer close(updates)
		defer close(done)

		if err := c.consume(ctx, ref, synced, graph, updates); err != nil {
			c.setErr(err)
		}
	}()

	return updates, nil
}

// consume reads the server's half of the connection for as long as it lasts.
// Pack bytes are handed to the interpreter as they arrive; only when an update
// is complete is it acknowledged and reported.
func (c *Client) consume(
	ctx context.Context,
	ref string,
	synced plumbing.Hash,
	graph *receive.Graph,
	updates chan<- Update,
) error {
	previous := synced

	for {
		// Counted from before the update message is read to after its
		// pack has been drained, so the figure is the bytes that
		// actually crossed the socket for this push.
		start := c.conn.BytesRead()

		msg, err := c.conn.ReadServerMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}

		// The pack reader ends at the marker closing the update, so it
		// can be handed straight to the interpreter: objects are
		// decoded as the bytes come off the socket, with nothing
		// buffered in between.
		pack := c.conn.PackReader()

		if msg.ObjectCount > 0 {
			if err := receive.Interpret(pack, graph); err != nil {
				return err
			}
		}

		// Reaching the end of the pack leaves the connection ready for
		// the next message, whether or not the interpreter read it all.
		if _, err := io.Copy(io.Discard, pack); err != nil {
			return err
		}

		received := c.conn.BytesRead()

		if err := c.conn.WriteAck(msg.Hash); err != nil {
			return err
		}

		select {
		case updates <- Update{
			Ref:         ref,
			Hash:        msg.Hash,
			Previous:    previous,
			ObjectCount: msg.ObjectCount,
			Bytes:       received - start,
			TotalBytes:  received,
			Graph:       graph,
		}:
		case <-ctx.Done():
			return ctx.Err()
		}

		previous = msg.Hash
	}
}
