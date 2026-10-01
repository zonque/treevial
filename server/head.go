package server

// Where a ref points, and the state the server keeps behind one: the head
// itself, the refusals a head can meet, and the locking that keeps a ref's
// objects and its head answering as a pair.

import (
	"errors"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
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

// The errors this package reports for a ref, each answering one question.
// They are named rather than left to be matched on text because a cluster hits
// them in its ordinary running: a node applies entries for every ref it
// replicates while it may hold only some of them, and a head that has already
// been passed is what a re-delivered entry carries.
var (
	// ErrUnknownRef is reported for a ref this server does not hold at all:
	// one nobody has published and nobody is subscribed to.
	ErrUnknownRef = errors.New("treevial/server: this server does not hold that ref")
	// ErrRefHeld is reported by [Server.Publish] for a ref the server
	// already holds, whether published or prepared for a subscriber.
	ErrRefHeld = errors.New("treevial/server: this server already holds that ref")
	// ErrNotPublished is reported by [Server.Unpublish] for a ref the
	// server holds through a provider, whose lifetime is not the caller's
	// to end.
	ErrNotPublished = errors.New("treevial/server: that ref was not published")
	// ErrNotAdvancing is reported for a head that does not move its ref.
	ErrNotAdvancing = errors.New("treevial/server: head does not advance")
)

// refState is everything the server holds for one ref: the objects behind it,
// the hash it points at, and the subscribers following it. It outlives any one
// connection — the first subscriber brings it into being and the last one to
// leave takes it away again.
type refState struct {
	// ready is closed once the provider has answered. prepared and err are
	// written before that and only read after it.
	ready    chan struct{}
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
	// published says the application holds this ref, so no provider is
	// asked for it, no release is made of it, and it outlives every
	// subscription to it. Set before the entry is reachable and never
	// written again.
	published bool

	mu    sync.Mutex
	head  Head
	store *objects.Store
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

// current returns the objects behind the ref and the head they are at, which
// have to be read together: separately, a caller could pair a new head with
// the store it replaced, or the reverse, and either is a head that store
// cannot serve.
func (st *refState) current() (*objects.Store, Head) {
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.store, st.head
}

// currentHead returns the head alone, for a caller with no use for the store.
func (st *refState) currentHead() Head {
	_, head := st.current()

	return head
}

// currentStore returns the objects alone, for a caller with no use for the head.
func (st *refState) currentStore() *objects.Store {
	store, _ := st.current()

	return store
}

// setStore installs the objects behind the ref.
func (st *refState) setStore(store *objects.Store) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.store = store
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

	return st.move(head)
}

// move is moveHead with the ref's lock already held, so that a caller which
// has other things to do under it — installing a store alongside the head —
// does them in the same breath.
func (st *refState) move(head Head) (bool, error) {
	if !st.installed {
		st.head = head
		st.sequenced = head.Sequence != 0
		st.installed = true

		return true, nil
	}

	if err := st.allows(head); err != nil {
		return false, err
	}

	if st.sequenced && head.Sequence <= st.head.Sequence {
		return false, nil
	}

	st.head = head

	return true, nil
}

// allows reports why head could not move this ref, or nil if it could — which
// covers a head that is simply not newer, since that is not an error in every
// caller's eyes. The caller holds st.mu.
func (st *refState) allows(head Head) error {
	if st.installed && st.sequenced != (head.Sequence != 0) {
		return treevial.Errorf(treevial.CodeInvalid,
			"a %s head on a %s ref: ordering across a mixture is undefined",
			kind(head.Sequence != 0), kind(st.sequenced))
	}

	return nil
}

// wouldMove reports whether head is one this ref would take, without taking
// it: what [Server.Sweep] asks of every named head before it installs any of
// them.
func (st *refState) wouldMove(head Head) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	if err := st.allows(head); err != nil {
		return err
	}

	if st.installed && st.sequenced && head.Sequence <= st.head.Sequence {
		return ErrNotAdvancing
	}

	return nil
}

// install puts the objects and the head in place together, and says whether
// the head moved. A sweep replaces a ref's store whether or not its head
// moves, so the two are not the same question.
func (st *refState) install(store *objects.Store, head Head) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.store = store

	return st.move(head)
}

// kind names what moveHead refused, so its message reads as a sentence.
func kind(sequenced bool) string {
	if sequenced {
		return "sequenced"
	}

	return "unsequenced"
}
