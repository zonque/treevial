package member_test

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/zonque/treevial-cluster-demo/member"
)

// sink is the smallest thing that is a raft.SnapshotSink: a buffer that
// remembers whether it was given up on.
type sink struct {
	bytes.Buffer
	cancelled bool
}

func (s *sink) ID() string    { return "test" }
func (s *sink) Cancel() error { s.cancelled = true; return nil }
func (s *sink) Close() error  { return nil }

// persist takes a snapshot of m and writes it out.
func persist(t *testing.T, m *member.Member) []byte {
	t.Helper()

	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer snap.Release()

	var out sink
	if err := snap.Persist(&out); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	if out.cancelled {
		t.Fatal("Persist gave up on a sink it reported no error for")
	}

	return out.Bytes()
}

// restore is Restore, from bytes.
func restore(t *testing.T, m *member.Member, snapshot []byte) error {
	t.Helper()

	return m.Restore(io.NopCloser(bytes.NewReader(snapshot)))
}

func TestAMemberRestoredFromASnapshotHoldsTheSameRoots(t *testing.T) {
	first := member.New(quiet(t), discard())

	apply(t, first, loadEntry(t, 7))
	apply(t, first, mtuEntry(t, 8, member.Ref("sensor-3"), 9000))

	snapshot := persist(t, first)

	srv := quiet(t)
	second := member.New(srv, discard())

	if err := restore(t, second, snapshot); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got, want := second.Roots(), first.Roots(); !maps.Equal(got, want) {
		t.Fatalf("the restored member holds %v, want %v", got, want)
	}
	if got := second.Index(); got != 8 {
		t.Errorf("the restored member is at index %d, want the snapshot's 8", got)
	}

	// And it serves them, at the index the snapshot was taken at — which
	// is where the ordinal comes from for a member that was not there for
	// the entries.
	for ref, root := range first.Roots() {
		head, held := srv.Head(ref)

		switch {
		case !held:
			t.Errorf("%s was not published by the restore", ref)
		case head.Hash != root:
			t.Errorf("%s is at %s, want %s", ref, head.Hash, root)
		case head.Sequence != 8:
			t.Errorf("%s is at sequence %d, want the snapshot's index 8", ref, head.Sequence)
		}
	}
}

// The full walk in Restore is what leaves the builder able to do the cheap
// builds that follow, so a restored member keeps up with its peers.
func TestARestoredMemberGoesOnBuildingIncrementally(t *testing.T) {
	first := member.New(quiet(t), discard())
	apply(t, first, loadEntry(t, 7))

	second := member.New(quiet(t), discard())
	if err := restore(t, second, persist(t, first)); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	ref := member.Ref("valve-9")

	apply(t, first, mtuEntry(t, 8, ref, 9000))
	apply(t, second, mtuEntry(t, 8, ref, 9000))

	if got, want := second.Roots()[ref], first.Roots()[ref]; got != want {
		t.Errorf("after the same entry the restored member is at %s, want %s", got, want)
	}
}

// Persist runs while entries keep being applied, and must still write a
// snapshot of the state as it was when it was taken.
func TestASnapshotIsWrittenWhileEntriesKeepArriving(t *testing.T) {
	m := member.New(quiet(t), discard())
	apply(t, m, loadEntry(t, 7))

	taken := m.Roots()

	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer snap.Release()

	ref := member.Ref("printer-7")

	done := make(chan struct{})

	go func() {
		defer close(done)

		for index := uint64(8); index < 40; index++ {
			m.Apply(mtuEntry(t, index, ref, 1500+int(index)))
		}
	}()

	var out sink
	if err := snap.Persist(&out); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	<-done

	restored := member.New(quiet(t), discard())
	if err := restore(t, restored, out.Bytes()); err != nil {
		t.Fatalf("Restore of a snapshot written under load: %v", err)
	}

	// The state as it was when the snapshot was taken, not as it became.
	if got := restored.Roots(); !maps.Equal(got, taken) {
		t.Errorf("the snapshot restored %v, want the state it was taken at, %v", got, taken)
	}
}

// A snapshot naming a root its pack does not carry is refused rather than
// published: a head a member cannot serve is worse than no head.
func TestARestoreThatCannotFindItsRootFails(t *testing.T) {
	m := member.New(quiet(t), discard())
	apply(t, m, loadEntry(t, 7))

	tampered := retarget(t, persist(t, m), member.Ref("printer-7"),
		"1111111111111111111111111111111111111111")

	if err := restore(t, member.New(quiet(t), discard()), tampered); err == nil {
		t.Error("Restore accepted a snapshot whose root is not in its pack")
	}
}

// A truncated snapshot is refused too, which is the other way a pack can fail
// to carry what its header names.
func TestARestoreOfATruncatedSnapshotFails(t *testing.T) {
	m := member.New(quiet(t), discard())
	apply(t, m, loadEntry(t, 7))

	whole := persist(t, m)

	if err := restore(t, member.New(quiet(t), discard()), whole[:len(whole)/2]); err == nil {
		t.Error("Restore accepted half a snapshot")
	}
}

// retarget rewrites one ref's root in a snapshot's header, leaving its pack
// alone.
func retarget(t *testing.T, snapshot []byte, ref, root string) []byte {
	t.Helper()

	line, pack, found := bytes.Cut(snapshot, []byte("\n"))
	if !found {
		t.Fatal("the snapshot has no header line")
	}

	var header struct {
		Index uint64            `json:"index"`
		Refs  map[string]string `json:"refs"`
	}

	if err := json.Unmarshal(line, &header); err != nil {
		t.Fatalf("decode the header: %v", err)
	}

	if _, named := header.Refs[ref]; !named {
		t.Fatalf("the snapshot does not name %s", ref)
	}

	header.Refs[ref] = root

	rewritten, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("encode the header: %v", err)
	}

	return append(append(rewritten, '\n'), pack...)
}

// A root naming a tree that is not the shape this member's value has is the
// closest a test can come to a member that did not reproduce the state, and it
// is the equality check in Restore that catches it: the objects all arrive, the
// value decodes, and the walk over it comes out at a different hash.
func TestARestoreThatDoesNotReproduceItsRootFails(t *testing.T) {
	m := member.New(quiet(t), discard())
	apply(t, m, loadEntry(t, 7))

	ref := member.Ref("printer-7")

	entries, held := m.Store().Tree(m.Roots()[ref])
	if !held {
		t.Fatalf("%s has no tree", ref)
	}

	// A subtree of the ref's own state: in the pack, reachable, and not
	// what a Config walks out to.
	tampered := retarget(t, persist(t, m), ref, entries[0].Hash.String())

	err := restore(t, member.New(quiet(t), discard()), tampered)
	if err == nil {
		t.Fatal("Restore accepted a root it did not rebuild")
	}

	if !strings.Contains(err.Error(), "but the snapshot says") {
		t.Errorf("Restore failed with %v, want the rebuilt root reported against the snapshot's", err)
	}
}

// Raft may ask for a snapshot of a member that has applied nothing yet.
func TestASnapshotOfAMemberHoldingNothingRestores(t *testing.T) {
	empty := persist(t, member.New(quiet(t), discard()))

	restored := member.New(quiet(t), discard())
	if err := restore(t, restored, empty); err != nil {
		t.Fatalf("Restore of an empty snapshot: %v", err)
	}

	if restored.Holds() {
		t.Error("a member restored from an empty snapshot holds something")
	}
}
