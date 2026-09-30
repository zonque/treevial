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
// A peer that vanishes rather than closing is a different matter, and
// [WithDeadPeerTimeout] is how long one may do so before its connection is
// given up on.
//
// A client is told the name of the server it reached, if that server has one,
// and the name of wherever a forwarded push's objects came from. Both arrive
// on every [Update] as labels to log; nothing is decided by either, and
// neither is checked.
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
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/internal/tcpkeep"
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
	Ref string
	// ServerID is the name the server gave itself, or empty if it did not
	// name itself. A server sends it at most once, so in practice it is
	// the same on every update of a connection — but that is the
	// protocol's promise rather than something this client enforces, and
	// a server that broke it would simply be reported as it behaved.
	//
	// It is a label to log. The client takes it verbatim: it is not
	// checked, nothing is decided by it, and a server that sends one
	// serves exactly what a server that does not would.
	ServerID string
	// OriginID names the server this push's objects were fetched from, and
	// is empty when the server named none — which covers both a push served
	// from its own store and a forwarder that said nothing.
	OriginID string
	// Sequence is the ordinal of the head this update carries, or zero if
	// the server sent none.
	//
	// It is non-decreasing on one connection, and may repeat: a wake that
	// finds the head unmoved produces a zero-object update at the sequence
	// already reported. Across connections it is comparable only insofar as
	// whatever owns the ref makes it so — see
	// [github.com/zonque/treevial/server.Head].
	//
	// One client is one connection to one server, so nothing here acts on
	// it. An application that runs several clients for redundancy is what
	// compares them and decides which arriving head supersedes which.
	Sequence uint64
	Hash     plumbing.Hash
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
	// history is how many states the graph is asked to keep, and sweeping
	// says whether it was asked at all: without the option a graph keeps
	// everything it is ever sent, which is not the same as keeping none.
	history  int
	sweeping bool
	// deadPeer is how long a peer may be unresponsive before the
	// connection is given up on, or zero to leave that to the system.
	deadPeer time.Duration

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

// WithHistory has the client sweep its graph as each update arrives, keeping
// only the n most recent states.
//
// A graph accumulates: an update carries only what moved, so the objects it
// already holds are what the new ones resolve against. Nothing is dropped on
// its own account, which over a long-lived subscription means every state the
// client was ever pushed is still in memory — on the example's configuration,
// four objects a push, for as long as the process runs.
//
// n counts the states kept behind the one arriving, and one is the usual
// answer: that is the baseline [structtree.ApplySince], Graph.Diff and
// Graph.ListingSince are asked about, so an update still costs only what
// moved. Ask for more if you compare against older states than the update's
// own Previous. Without this option nothing is ever dropped.
//
// The sweep happens as the next update arrives, just before its objects are
// interpreted — the one moment the graph is written to in any case. A caller
// that asks for this should leave [github.com/zonque/treevial/receive.Graph.Retain]
// alone: if a sweep cannot resolve a state it was keeping, the subscription
// ends with that error rather than quietly growing again.
func WithHistory(n int) Option {
	return func(c *Client) {
		c.history = n
		c.sweeping = true
	}
}

// WithDeadPeerTimeout bounds how long a peer may be unresponsive before the
// connection to it is given up on.
//
// A treevial connection is quiet by design — the server pushes when it has
// something to say, which may be hours after the last byte — so nothing here
// puts a deadline on silence. This bounds the other thing: a peer that has
// actually gone, whose host vanished or whose process was killed without
// closing. Without it such a connection can be held for the system's own
// retransmit default, which is measured in minutes.
//
// One duration drives both mechanisms that can notice. Keepalive probes start
// halfway through the budget and run four times across the rest of it, and
// where TCP_USER_TIMEOUT exists it bounds unacknowledged data. Those are
// different failures: probes only go out on an idle connection, so a peer that
// dies mid-push is caught by the user timeout alone — and therefore, today,
// only on Linux.
//
// The schedule is worked out in whole seconds, which is the unit the kernel
// keeps it in, so the shortest budget that can be expressed is eight seconds
// and rounding may cost a little more than was asked for — never less. See
// [github.com/zonque/treevial/internal/tcpkeep.For].
//
// When the connection does go, it ends the way any other ending does: the
// update channel closes and [Client.Err] says why. Deciding whether to dial
// again is the application's business, not this package's.
//
// Left unset, nothing changes: the connection keeps the plain 30-second
// keepalive it has always had, and outlives a long outage.
func WithDeadPeerTimeout(d time.Duration) Option {
	return func(c *Client) { c.deadPeer = d }
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

	// Checked before anything is dialled, so an unusable option costs no
	// connection at all.
	if err := treevial.ValidateClientID(c.id); err != nil {
		return nil, treevial.Errorf(treevial.CodeInvalid, "%v", err)
	}

	if c.sweeping && c.history < 1 {
		return nil, treevial.Errorf(treevial.CodeInvalid,
			"a history of %d states keeps nothing to decode against; leave the option off to keep everything",
			c.history)
	}

	if c.deadPeer != 0 {
		if err := tcpkeep.Validate(c.deadPeer); err != nil {
			return nil, treevial.Errorf(treevial.CodeInvalid, "%v", err)
		}
	}

	// Unset, this is the plain keepalive the connection has always had.
	dialer := net.Dialer{KeepAlive: keepalivePeriod}

	if c.deadPeer != 0 {
		live := tcpkeep.For(c.deadPeer)

		dialer = net.Dialer{
			KeepAliveConfig: live.KeepAlive,
			Control: func(_, _ string, rc syscall.RawConn) error {
				return tcpkeep.SetUserTimeout(rc, live.UserTimeout)
			},
		}
	}

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

	// serverID is whatever the server called itself, recorded as it
	// arrives and stamped onto every update. The client does not examine
	// it, and a second such line simply replaces it.
	var serverID string

	// heads are the states the graph is asked to keep, newest last. It is
	// only used when the caller asked for the graph to be swept.
	var heads []plumbing.Hash
	if c.sweeping && !synced.IsZero() {
		heads = append(heads, synced)
	}

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

		if msg.Type == wire.Announce {
			// Nothing follows a name — no pack, nothing to drain —
			// so this reads the next message rather than the pack
			// reader, which would otherwise swallow the next
			// update's pack.
			serverID = msg.ServerID

			continue
		}

		// Swept here, with an update in hand but before a byte of it
		// has been interpreted: the graph is about to be written to in
		// any case, so nothing is dropped while a caller might still
		// be reading what it was handed last.
		if c.sweeping {
			if err := sweep(graph, heads); err != nil {
				return err
			}
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
			ServerID:    serverID,
			OriginID:    msg.OriginID,
			Sequence:    msg.Sequence,
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

		if c.sweeping && (len(heads) == 0 || heads[len(heads)-1] != msg.Hash) {
			heads = append(heads, msg.Hash)
			if len(heads) > c.history {
				heads = heads[len(heads)-c.history:]
			}
		}
	}
}

// sweep drops from the graph everything the states named in heads cannot
// reach. A graph it cannot resolve is one something else has taken objects
// out of, and carrying on would mean growing without bound, which is the one
// thing the caller asked not to happen.
func sweep(graph *receive.Graph, heads []plumbing.Hash) error {
	if len(heads) == 0 {
		return nil
	}

	if _, err := graph.Retain(heads...); err != nil {
		return err
	}

	return nil
}
