// Package member is what one node of the cluster does with the log it shares
// with its peers.
//
// Everything here is deliberately the boring half of a real member. The state
// comes out of a map rather than a database, and it changes because a ticker
// says so rather than because a message bus delivered something. What is not
// simplified is the part being demonstrated: every node applies the same
// entries, builds its own objects from them, and serves its own clients from
// its own store, and no objects ever cross between nodes while the cluster
// runs.
//
// The pieces fit together like this:
//
//	db.go        the devices this cluster holds configurations for
//	command.go   the two kinds of entry a node may propose
//	member.go    the refs, their objects, and the treevial server serving them
//	fsm.go       Apply, Snapshot and Restore: raft's whole interface to us
//	snapshot.go  writing a snapshot out, which is a root hash and a packfile
//	inject.go    the leader's ticker, standing in for a message bus
package member

import (
	"fmt"
	"sort"

	"github.com/zonque/treevial/demo/shared"
)

// devices are the installations this cluster holds a configuration for. A real
// member would find them in a database; the point of the list is that finding
// them is expensive and rare, so it happens on one node, once.
var devices = []string{"printer-7", "sensor-3", "valve-9"}

// Ref is the head a device's configuration is served on. The shape is this
// demo's own convention — treevial attaches no meaning to it, and the client
// derives the same name from the same device without either side telling the
// other.
func Ref(device string) string {
	return fmt.Sprintf("refs/heads/%s/config", device)
}

// Devices reads the database.
//
// This is the expensive, rare query the whole arrangement is built around: the
// node that wins the first election calls it once and puts the answer in the
// log, and every other node — including one that joins hours later — receives
// that state rather than reading it again.
//
// The values are freshly built, so a caller that mutates what it was handed
// changes nothing but its own copy.
func Devices() map[string]*shared.Config {
	out := make(map[string]*shared.Config, len(devices))
	for _, device := range devices {
		out[Ref(device)] = shared.Example(device)
	}

	return out
}

// sortedRefs returns a map's refs in a fixed order. Every member has to build
// its objects in the same order to arrive at the same hashes, and a Go map
// hands out its keys in none.
func sortedRefs[V any](set map[string]V) []string {
	refs := make([]string, 0, len(set))
	for ref := range set {
		refs = append(refs, ref)
	}

	sort.Strings(refs)

	return refs
}
