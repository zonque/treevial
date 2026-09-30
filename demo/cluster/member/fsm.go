package member

import (
	"errors"
	"fmt"

	"github.com/hashicorp/raft"

	"github.com/zonque/treevial/server"
)

// Apply implements [raft.FSM]. It is called once per committed entry, on one
// goroutine, in log order, on every member — so whatever it does here is what
// the whole cluster does.
//
// Deterministic is the whole requirement, and it is met by the entry carrying
// the value: a follower encodes what it was given rather than working out what
// the change should be. The objects that come out are then byte for byte the
// ones every other member produced, which is why a client can be served by any
// of them and why none of them has to send objects to another.
func (m *Member) Apply(l *raft.Log) any {
	// Raft replicates its own configuration through the same call. Those
	// entries are not ours to decode.
	if l.Type != raft.LogCommand {
		return nil
	}

	cmd, err := Decode(l.Data)
	if err != nil {
		return err
	}

	if err := m.apply(cmd, l.Index); err != nil {
		// Two of the server's answers are ordinary traffic here. A ref
		// this member holds nothing for is an entry meant for state it
		// does not have, and a head that does not advance is an entry
		// whose state it is already past. Neither is a reason to stop
		// applying the log.
		switch {
		case errors.Is(err, server.ErrUnknownRef), errors.Is(err, server.ErrNotAdvancing):
			m.out.Printf("entry %d: %v", l.Index, err)

			return nil
		}

		return err
	}

	return nil
}

// apply dispatches one entry under the member's lock.
func (m *Member) apply(cmd Command, index uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// An entry this member is already past. A restore leaves it at the
	// index its snapshot was taken at, so the first entries to follow may
	// be ones the snapshot already accounts for.
	if index <= m.index {
		m.out.Printf("entry %d: already applied, at %d", index, m.index)

		return nil
	}

	// The member is at this entry once it has taken it up, whether or not
	// the entry had anything here to do: where it is in the log is the
	// log's business rather than a summary of what it happens to hold.
	m.index = index

	switch cmd.Kind {
	case KindLoad:
		return m.load(cmd.Devices, index)
	case KindMTU:
		return m.setMTU(cmd.Ref, cmd.MTU, index)
	default:
		return fmt.Errorf("entry %d is of unknown kind %q", index, cmd.Kind)
	}
}
