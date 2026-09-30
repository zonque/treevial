package member_test

import (
	"io"
	"log"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zonque/treevial/server"

	"github.com/zonque/treevial-cluster-demo/member"
)

// quiet returns a treevial server that holds refs and listens nowhere, which is
// everything a member needs of one.
func quiet(t *testing.T) *server.Server {
	t.Helper()

	srv, err := server.New()
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	t.Cleanup(srv.Stop)

	return srv
}

// discard is a logger for a member nobody is reading.
func discard() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// loadEntry is the whole database, as raft would deliver it.
func loadEntry(t *testing.T, index uint64) *raft.Log {
	t.Helper()

	data, err := member.Load(member.Devices())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	return &raft.Log{Index: index, Type: raft.LogCommand, Data: data}
}

// mtuEntry is one field of one device, as raft would deliver it.
func mtuEntry(t *testing.T, index uint64, ref string, mtu int) *raft.Log {
	t.Helper()

	data, err := member.SetMTU(ref, mtu)
	if err != nil {
		t.Fatalf("SetMTU: %v", err)
	}

	return &raft.Log{Index: index, Type: raft.LogCommand, Data: data}
}

// apply applies one entry and fails the test if the member reported anything.
func apply(t *testing.T, m *member.Member, l *raft.Log) {
	t.Helper()

	if got := m.Apply(l); got != nil {
		t.Fatalf("Apply of entry %d: %v", l.Index, got)
	}
}

func TestACommandSurvivesTheLog(t *testing.T) {
	data, err := member.SetMTU(member.Ref("printer-7"), 9000)
	if err != nil {
		t.Fatalf("SetMTU: %v", err)
	}

	got, err := member.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if got.Kind != member.KindMTU || got.Ref != "refs/heads/printer-7/config" || got.MTU != 9000 {
		t.Errorf("the entry came back as %+v", got)
	}
}

func TestAnEntryOfAnUnknownKindIsRefused(t *testing.T) {
	if _, err := member.Decode([]byte(`{"kind":"drop-everything"}`)); err == nil {
		t.Error("Decode accepted an entry of a kind this member cannot apply")
	}
}

// The database hands out values of its own, so nothing a caller does to what it
// was given reaches the next reader.
func TestTheDatabaseHandsOutItsOwnValues(t *testing.T) {
	ref := member.Ref("printer-7")

	first := member.Devices()
	first[ref].Network.Primary.MTU = 1

	if second := member.Devices(); second[ref].Network.Primary.MTU == 1 {
		t.Error("a caller changed the database through the value it was handed")
	}
}

func TestLoadingPublishesEveryDeviceAtTheEntrysIndex(t *testing.T) {
	srv := quiet(t)
	m := member.New(srv, discard())

	apply(t, m, loadEntry(t, 7))

	devices := member.Devices()
	if len(devices) == 0 {
		t.Fatal("the database is empty")
	}

	for ref := range devices {
		head, held := srv.Head(ref)

		switch {
		case !held:
			t.Errorf("%s is not held", ref)
		case head.Hash.IsZero():
			t.Errorf("%s is held at no hash", ref)
		case head.Sequence != 7:
			t.Errorf("%s is at sequence %d, want the entry's index 7", ref, head.Sequence)
		}
	}

	if got, want := len(m.Roots()), len(devices); got != want {
		t.Errorf("the member holds %d ref(s), want %d", got, want)
	}
	if !m.Holds() {
		t.Error("a member that has loaded the database holds nothing")
	}
}

// Raft replicates its own configuration through Apply, and those entries are
// not the member's to decode.
func TestAConfigurationEntryIsNotAnApplication(t *testing.T) {
	m := member.New(quiet(t), discard())

	l := &raft.Log{Index: 1, Type: raft.LogConfiguration, Data: []byte("not a command")}
	if got := m.Apply(l); got != nil {
		t.Errorf("Apply of a configuration entry = %v, want nil", got)
	}
}

// A member applies every entry the cluster commits, including entries for refs
// it holds nothing for.
func TestAnEntryForARefThisMemberDoesNotHoldIsSwallowed(t *testing.T) {
	m := member.New(quiet(t), discard())

	if got := m.Apply(mtuEntry(t, 3, member.Ref("nobody"), 9000)); got != nil {
		t.Errorf("Apply = %v, want the unknown ref swallowed", got)
	}

	if m.Index() != 3 {
		t.Errorf("the member is at index %d, want the entry's 3", m.Index())
	}
}

func TestAnEntryTheMemberIsPastLeavesTheHeadAlone(t *testing.T) {
	srv := quiet(t)
	m := member.New(srv, discard())
	ref := member.Ref("printer-7")

	apply(t, m, loadEntry(t, 7))
	apply(t, m, mtuEntry(t, 9, ref, 9000))

	after, _ := srv.Head(ref)

	// The same entry again, as a member coming back from a snapshot may
	// see it.
	apply(t, m, mtuEntry(t, 9, ref, 1500))

	if now, _ := srv.Head(ref); now != after {
		t.Errorf("the head moved to %+v on an entry already applied, want it left at %+v", now, after)
	}
}

// The reason the cluster works at all: one field changing costs one blob and
// the trees above it, not a rebuild.
func TestAChangeCostsOneBlobAndTheTreesAboveIt(t *testing.T) {
	srv := quiet(t)
	m := member.New(srv, discard())
	ref := member.Ref("printer-7")

	apply(t, m, loadEntry(t, 7))
	before, _ := srv.Head(ref)

	apply(t, m, mtuEntry(t, 8, ref, 9000))
	after, _ := srv.Head(ref)

	if after.Hash == before.Hash {
		t.Fatal("the head did not move")
	}
	if after.Sequence != 8 {
		t.Errorf("sequence %d, want the entry's index 8", after.Sequence)
	}

	moved, err := m.Store().SelectSince(before.Hash, after.Hash)
	if err != nil {
		t.Fatalf("SelectSince: %v", err)
	}

	// The blob for the field, and Network/Primary, Network and the root
	// above it.
	if len(moved) != 4 {
		t.Errorf("a one-field change moved %d objects, want 4", len(moved))
	}
}

// Every member builds the same objects from the same entries, which is what
// lets a client be served by any of them.
func TestTwoMembersApplyingTheSameEntriesAgree(t *testing.T) {
	first := member.New(quiet(t), discard())
	second := member.New(quiet(t), discard())

	ref := member.Ref("sensor-3")

	for _, m := range []*member.Member{first, second} {
		apply(t, m, loadEntry(t, 7))
		apply(t, m, mtuEntry(t, 8, ref, 4000))
	}

	for ref, root := range first.Roots() {
		if got := second.Roots()[ref]; got != root {
			t.Errorf("%s is at %s on one member and %s on the other", ref, root, got)
		}
	}
}
