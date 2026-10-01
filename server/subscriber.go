package server

// One connected subscriber: what the server keeps for it, what it reports
// about it, and how it is told its ref has moved.

import (
	"sync"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/objects"
)

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

// ack records that the client interpreted everything up to hash, which was
// pushed from store.
//
// A sweep replaces a ref's store and resets the baselines whose trees it
// dropped. An acknowledgement of a push made before that would put one back,
// and the next push would then compute a delta from a tree the new store does
// not hold — so an acknowledgement naming a store the ref has since replaced
// is stale, exactly as one naming an earlier head is, and is dropped the same
// way.
func (c *subscriber) ack(store *objects.Store, hash plumbing.Hash) {
	if store != c.state.currentStore() {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.synced = hash
}

// reset forgets what the subscriber was holding, so that its next push carries
// the whole of its ref's tree.
func (c *subscriber) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.synced = plumbing.ZeroHash
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
