package server

// The refs an application holds itself: taking one on, reclaiming what the
// refs no longer reach, and giving one up.

import (
	"fmt"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/internal/wire"
	"github.com/zonque/treevial/objects"
)

// Publish hands the server a ref the application holds itself, with the objects
// behind it and the head it points at.
//
// The ref then exists because this node holds it. It needs no subscriber, it
// outlives every subscription to it, and [Server.SetHead] moves it whether or
// not anybody is connected — which is what lets a node apply entries for refs
// no client has asked for yet. A client that subscribes joins what is already
// there and is pushed the current head. Neither [Provider.Prepare] nor
// [Provider.Release] is ever called for it.
//
// Publishing does not replace. A ref the server already holds is refused, so
// there is no moment at which a subscriber's acknowledged tree stops existing
// underneath it. Taking on new state — a snapshot's restore included — is
// loading the objects into the store the ref already has and moving its head
// with SetHead; nothing removes objects from a store, so the old ones stay and
// every connected subscriber goes on being served incrementally. Reclaiming
// them is Unpublish and then Publish with a new store, which costs that ref's
// subscribers a reconnect.
//
// The head goes in by the same rule as SetHead's: publishing decides whether
// the ref is sequenced, and from then on it moves only forward.
func (s *Server) Publish(ref string, store *objects.Store, head Head) error {
	if err := treevial.ValidateRef(ref); err != nil {
		return treevial.Errorf(treevial.CodeInvalid, "publish: %v", err)
	}

	if store == nil {
		return treevial.Errorf(treevial.CodeInvalid, "publish %q: a nil store", ref)
	}

	st := newRefState()
	st.published = true
	st.prepared = true
	st.setStore(store)

	s.mu.Lock()
	if _, held := s.refs[ref]; held {
		s.mu.Unlock()

		return fmt.Errorf("publish %q: %w", ref, ErrRefHeld)
	}
	s.refs[ref] = st
	s.mu.Unlock()

	// Installed before the head is checked so that nothing else can take
	// the ref in between, and taken back out rather than left behind if the
	// head is refused.
	if _, err := st.moveHead(head); err != nil {
		s.forget(ref, st)

		return fmt.Errorf("publish %q: %w", ref, err)
	}

	close(st.ready)

	return nil
}

// Sweep installs a compacted store and a new set of heads in one step, and
// reports how many subscribers lost the state they were holding.
//
// This is the moment at which the objects behind state that has been replaced
// in bulk are reclaimed. [github.com/zonque/treevial/objects.Store.Compact]
// makes the store, keeping what the surviving heads reach, and this puts it in
// place for every published ref. What the new store does not hold is freed
// once the last push still reading the old one is done.
//
// heads names only the refs whose heads move, and a zero Hash is a ref whose
// state is gone: its subscribers are told so and stay subscribed. Every other
// published ref keeps the head it has, which is why a sweep must leave those
// heads reachable in the new store too.
//
// Nothing changes unless everything checks out: every named ref is published,
// every resulting non-zero head is present in the new store, and every named
// head advances its ref. A sweep that would leave a ref with a head its store
// cannot serve is refused with the world exactly as it was — which is what
// turns forgetting to retain a surviving ref's head into an error rather than
// a handful of dead subscriptions.
//
// Subscribers are not disturbed for nothing: one whose ref did not move and
// whose tree the new store still holds is not woken at all. One whose tree is
// gone has its baseline reset and is sent its ref's whole tree.
//
// Every builder writing into the old store must be retargeted afterwards —
// [github.com/zonque/treevial/structtree.Builder.Retarget] — whether or not
// its ref moved, since the store is replaced for all of them.
//
// Provider-backed refs are untouched: their stores came from
// [Provider.Prepare] and are that provider's business.
func (s *Server) Sweep(store *objects.Store, heads map[string]Head) (int, error) {
	if store == nil {
		return 0, treevial.Errorf(treevial.CodeInvalid, "sweep: a nil store")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for ref := range heads {
		st, held := s.living(ref)

		switch {
		case !held:
			return 0, fmt.Errorf("sweep %q: %w", ref, ErrUnknownRef)
		case !st.published:
			return 0, fmt.Errorf("sweep %q: %w", ref, ErrNotPublished)
		}
	}

	published := map[string]*refState{}
	for ref, st := range s.refs {
		if st.published {
			published[ref] = st
		}
	}

	// Checked in full before anything is installed.
	for ref, st := range published {
		head, named := heads[ref]
		if !named {
			head = st.currentHead()
		}

		if !head.Hash.IsZero() && !store.Has(head.Hash) {
			return 0, treevial.Errorf(treevial.CodeInvalid,
				"sweep %q: the new store does not hold %s", ref, head.Hash)
		}

		if named {
			if err := st.wouldMove(head); err != nil {
				return 0, fmt.Errorf("sweep %q: %w", ref, err)
			}
		}
	}

	reset := 0

	for ref, st := range published {
		head, named := heads[ref]

		var moved bool

		if named {
			// The head was checked above, so this cannot refuse.
			moved, _ = st.install(store, head)
		} else {
			st.setStore(store)
		}

		for c := range st.members {
			// A subscriber holding nothing has nothing to lose, and is
			// already on its way to being sent everything.
			held := c.held()
			dropped := !held.IsZero() && !store.Has(held)

			if dropped {
				c.reset()
				reset++
			}

			if moved || dropped {
				c.wake()
			}
		}
	}

	return reset, nil
}

// Unpublish gives up a ref the application published, and ends the
// subscriptions to it.
//
// The node is saying it no longer holds the objects those subscribers are being
// served from, so leaving them following the ref would leave them following
// something that can never be pushed to them again.
func (s *Server) Unpublish(ref string) error {
	s.mu.Lock()
	st, held := s.refs[ref]

	switch {
	case !held:
		s.mu.Unlock()

		return fmt.Errorf("unpublish %q: %w", ref, ErrUnknownRef)
	case !st.published:
		s.mu.Unlock()

		return fmt.Errorf("unpublish %q: %w", ref, ErrNotPublished)
	}

	delete(s.refs, ref)

	conns := make([]*wire.Conn, 0, len(st.members))
	for c := range st.members {
		conns = append(conns, c.conn)
	}
	s.mu.Unlock()

	// Closing the connection is what ends a subscription, whichever side
	// does it: the goroutine serving it notices on its next read or write.
	for _, conn := range conns {
		conn.Close()
	}

	return nil
}
