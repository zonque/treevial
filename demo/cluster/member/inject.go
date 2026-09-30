package member

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/raft"
)

// proposeTimeout bounds how long a proposal waits to be committed. A demo
// should say something went wrong rather than sit there.
const proposeTimeout = 5 * time.Second

// mtus are the values the ticker cycles a device through. The database's own
// value is last, so the very first change to a device is one that moves its
// tree rather than one that arrives at the state it is already in.
var mtus = []int{4000, 9000, 1500}

// Proposer is the part of raft that entries are put into the log through. Only
// the leader may, which is the one thing about a cluster that treevial itself
// has no opinion on: a member serves its clients whatever its role, and only
// changing the state needs consensus.
type Proposer interface {
	Apply(cmd []byte, timeout time.Duration) raft.ApplyFuture
	State() raft.RaftState
}

// LoadOnce proposes the contents of the database, unless this member already
// has state.
//
// It is called on winning an election. The condition is what makes the
// expensive read happen once in the cluster's life rather than once per
// leadership change: a member that already holds refs got them from the log or
// from a snapshot, and reading the database again would be paying for what it
// has.
func (m *Member) LoadOnce(p Proposer) error {
	if m.Holds() {
		m.out.Printf("elected leader at index %d; the state is already here", m.Index())

		return nil
	}

	m.out.Print("elected leader with nothing held; reading the database")

	cmd, err := Load(Devices())
	if err != nil {
		return fmt.Errorf("encode the database: %w", err)
	}

	return propose(p, cmd)
}

// Inject stands in for the message bus: every interval it changes one field of
// one device, in turn.
//
// It runs on every member and does nothing at all on one that is not the
// leader, so a cluster that has just elected a new leader goes on changing
// without anybody being told to take over.
func (m *Member) Inject(ctx context.Context, p Proposer, interval time.Duration) {
	if interval <= 0 {
		return
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()

	proposals := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		if p.State() != raft.Leader {
			continue
		}

		ref := Ref(devices[proposals%len(devices)])
		mtu := mtus[(proposals/len(devices))%len(mtus)]
		proposals++

		cmd, err := SetMTU(ref, mtu)
		if err != nil {
			m.out.Printf("encode a change to %s: %v", ref, err)

			continue
		}

		if err := propose(p, cmd); err != nil {
			// Losing the leadership between the check above and the
			// proposal is the ordinary way this fails, and the
			// answer is to leave it to whoever won.
			m.out.Printf("propose a change to %s: %v", ref, err)
		}
	}
}

// propose puts one entry in the log and waits for it to be committed.
func propose(p Proposer, cmd []byte) error {
	future := p.Apply(cmd, proposeTimeout)
	if err := future.Error(); err != nil {
		return err
	}

	// An entry that could not be applied is reported through the future,
	// on the node that proposed it, once every member has it.
	if err, failed := future.Response().(error); failed {
		return err
	}

	return nil
}
