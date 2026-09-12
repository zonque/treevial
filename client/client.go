// Package client is the receiving side of treevial. It dials the server and
// keeps one connection open for the lifetime of the process, but never asks for
// anything: it names itself in the opening message and then simply interprets
// whatever the server pushes, on the fly, without writing a byte to disk. The
// ref it is served follows from its own ID.
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
// send, and the accumulated graph the client has interpreted so far.
type Update struct {
	// ClientID this client identified itself with.
	ClientID string
	// Ref the update arrived for, which follows from ClientID.
	Ref  string
	Hash plumbing.Hash
	// Previous is the hash this subscription was at before the update, or
	// the zero hash for the first one. Graph.Diff(Previous, Hash) is what
	// turns an update into a list of changed paths, and ApplySince uses the
	// pair to decode only what moved.
	Previous    plumbing.Hash
	ObjectCount int
	Graph       *receive.Graph
}

// Client is a connection to a treevial server. One connection carries one
// subscription, so Subscribe or Resume is called once per Client.
type Client struct {
	conn *wire.Conn

	mu  sync.Mutex
	err error
}

// Dial connects to a treevial server.
//
// No deadline is ever set on the connection: the server pushes when it has
// something to say, which may be hours after the last byte, and a deadline
// would tear down a perfectly good connection in the meantime.
func Dial(ctx context.Context, addr string) (*Client, error) {
	dialer := net.Dialer{KeepAlive: keepalivePeriod}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	return &Client{conn: wire.NewConn(conn)}, nil
}

// Close hangs up.
func (c *Client) Close() error {
	return c.conn.Close()
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

// Subscribe registers under clientID as a client holding nothing, and returns a
// channel of updates the server pushes. The ID decides which ref the server
// serves, and the server prepares that data as the client connects.
func (c *Client) Subscribe(ctx context.Context, clientID string) (<-chan Update, error) {
	return c.Resume(ctx, clientID, plumbing.ZeroHash, receive.NewGraph())
}

// Resume registers under clientID declaring that the client already holds the
// tree at synced, whose objects are in graph. That single hash is the whole of
// the client's state: the server sends only the difference between it and the
// ref. Later updates accumulate into the same graph.
//
// Cancelling ctx closes the connection, which ends the subscription and closes
// the channel.
func (c *Client) Resume(
	ctx context.Context,
	clientID string,
	synced plumbing.Hash,
	graph *receive.Graph,
) (<-chan Update, error) {
	// Checked here as well as on the server, so an unusable ID fails
	// without a round trip; the rule itself lives in one place.
	if err := treevial.ValidateID(clientID); err != nil {
		return nil, treevial.Errorf(treevial.CodeInvalid, "%v", err)
	}

	if err := c.conn.WriteRegister(clientID, synced); err != nil {
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

		if err := c.consume(ctx, clientID, synced, graph, updates); err != nil {
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
	clientID string,
	synced plumbing.Hash,
	graph *receive.Graph,
	updates chan<- Update,
) error {
	previous := synced

	for {
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

		if err := c.conn.WriteAck(msg.Hash); err != nil {
			return err
		}

		select {
		case updates <- Update{
			ClientID:    clientID,
			Ref:         treevial.RefFor(clientID),
			Hash:        msg.Hash,
			Previous:    previous,
			ObjectCount: msg.ObjectCount,
			Graph:       graph,
		}:
		case <-ctx.Done():
			return ctx.Err()
		}

		previous = msg.Hash
	}
}
