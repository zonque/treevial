# treevial — server-driven sync over git objects

`treevial` keeps a client in sync with data the server owns. The client dials
in, names the head it wants to follow and the tree it already holds, and then
asks for nothing further: the **server** prepares that ref's objects and pushes
them down the connection, of its own accord, whenever the ref moves. Nobody
polls, and nothing is fetched on request.

Git's object model is what makes that cheap. Content addressing means a subtree
whose hash has not moved needs neither sending nor decoding, so an update after
a small change costs a few objects however large the whole is — and the
machinery for saying so, packfiles and trees and blobs, already exists and is
well understood. The roles are the other way around from git's usual
arrangement, and that is the point: the server decides when to send.

Neither side touches the filesystem. There is no repository, no `.git`
directory, no temporary pack file. The server keeps its object graph in memory,
and the client interprets each object as it is inflated off the wire and throws
the bytes away.

## Using it as a library

The two sides are separate packages, so a client repository and a server
repository can each depend on only what it needs.

```console
go get github.com/zonque/treevial
```

| Import | For | Pulls in |
|---|---|---|
| `github.com/zonque/treevial` | The shared contract: `ValidateRef`, `Error`, `CodeOf` | both sides need it |
| `github.com/zonque/treevial/client` | `Dial`, `Subscribe`, `Resume`, `Update` | client repositories |
| `github.com/zonque/treevial/receive` | `Interpret`, `Handler`, `Graph`, `Diff`, `Listing` | client repositories |
| `github.com/zonque/treevial/server` | `Server`, `Provider`, `Subscription` | server repositories |
| `github.com/zonque/treevial/objects` | `Store`, `SelectSince`, `EncodePack`, `ReplaceBlob` | server repositories |
| `github.com/zonque/treevial/structtree` | `Walk`, `Build`, `Builder`, `Apply`, `ApplySince`, `Mapper` | both sides, when syncing a Go value |

A client:

```go
conn, err := client.Dial(ctx, "treevial.internal:9418")
updates, err := conn.Subscribe(ctx, "refs/heads/printer-7/config")

for u := range updates {
	changes, _ := u.Graph.Diff(u.Previous, u.Hash)  // only what moved
	for _, c := range changes {
		log.Printf("%s %s = %q", c.Kind.Symbol(), c.Path, c.Content)
	}
}
```

## Synchronising a Go struct

`structtree` maps a Go value onto a tree, so the thing being synchronised can
be an ordinary nested struct. Field names become path elements:

```go
type Config struct {
	Device  Device                   //   Device/Name
	Network Network                  //   Network/Primary/MTU
	Audio   Audio                    //   Audio/Delay
}

root, err := structtree.Build(store, cfg)   // one tree, one blob per leaf
```

### What becomes a leaf

This is the decision the whole mapping turns on: it decides the shape of the
tree, and so the paths, what a diff reports, and how little has to move when one
field changes. **Say it with a struct tag.**

```go
type Device struct {
	Name      string
	Installed time.Time `treevial:"leaf"`   // stored whole, in one blob
	Location  Location                      // a subtree
}
```

`treevial:"leaf"` is honoured whatever else is configured, and it keeps the
decision in the type itself, next to the fields, where a reader of the struct
will look for it. Any other tag value is ignored.

Failing a tag, a field is a leaf if it has **no structure to descend into** — an
`int`, a `string`, a `[]byte`, a `[]string` — or if it is a **`proto.Message`**,
which is stored as one blob of its own wire bytes rather than a subtree of its
internal fields. Unexported fields are skipped, and so are nil pointers — which
makes a field going nil read as a deletion and a field appearing read as an
addition.

**Everything with structure becomes a subtree:**

```go
type Config struct {
	Device  Device                            // Device/Name, …
	Limits  map[string]int                    // Limits/gain, Limits/delay
	Ports   map[string]*Interface             // Ports/eth0/MTU, …
	Delays  []*durationpb.Duration            // Delays/0, Delays/1, …
	Tags    []string                          // one blob: scalars
	Raw     []byte                            // one blob
	Whole   map[string]int `treevial:"leaf"`  // one blob: you said so
}
```

A map's keys must be strings, since they become path elements, and a key may be
neither empty nor contain a slash — either would invent nesting the value does
not have. A map keyed by anything else is reported as an error unless tagged as
a leaf. Keys are walked in sorted order, so neither the tree nor `Walk` depends
on Go's random map order. A slice's children are named by index, and decoding
reads them as numbers rather than as text, so order survives past ten elements.
A nil or empty map, or a slice that would have been a subtree, contributes
nothing — like a struct with nothing in it. A nil or empty slice of *scalars* is
a leaf like any other, since `nil` and `[]` are worth telling apart.

**Why slices of messages nest, and not just for granularity.** A leaf that is
not itself a message but merely contains some is encoded as JSON, and JSON
cannot put a protobuf `oneof` back together — it writes the wrapper the
generated code uses and then has nothing to unmarshal it into. Giving each
message a blob of its own gets it the wire encoding it deserves. For the same
reason an interface counts as scalar and a message inside one is refused, since
nothing on the far side would say which message to expect.

Keys must be strings, since they become path elements, and a key may be neither
empty nor contain a slash — either would invent nesting the value does not have.
A map keyed by anything else has nothing to offer a path and is reported as an
error, unless it is tagged as a leaf, which is how you ask for it in one blob.
Keys are walked in sorted order, so neither the tree nor `Walk` depends on Go's
random map order. A nil or empty map contributes nothing, like a struct with
nothing in it.

One limitation worth knowing: Go has no address for a map element, so a
`Builder` can only be told that a whole map changed (`b.Build(&cfg.Limits)`),
which recomputes its entries.

That fallback has one blind spot, and it is worth knowing before it bites: **a
`time.Time` is a struct, is not a protobuf message, and has
only unexported fields**, so the walker descends into it, finds nothing it may
read, and the field vanishes from the tree entirely. Tag it and it is stored
whole — JSON already knows how to write a time, so no encoding of your own is
needed. Any struct of that shape needs the same treatment, and `Graph.Listing`
will show you what actually became a blob.

For types you do not own, and so cannot tag, a `Mapper` carries a rule of your
own:

```go
m := structtree.Mapper{
	IsLeaf: func(f reflect.StructField) bool {
		return f.Type == reflect.TypeFor[time.Time]() || structtree.DefaultIsLeaf(f)
	},
}

root, err := m.Build(store, cfg)   // and m.Walk, m.Apply, m.ApplySince, m.NewBuilder
```

A `Mapper` holds all three decisions — which fields are leaves, how a leaf
becomes bytes, and how bytes become a value again — because they have to agree:
a rule that keeps some struct whole only works if the encoding knows what to do
with it, and a value written by one encoding can only be read by its
counterpart. The package-level functions are shorthand for its zero value.

The walk is also available on its own, as an iterator:

```go
for leaf := range structtree.Walk(cfg) {
	fmt.Println(leaf.Path, leaf.Value)      // "Network/Primary/MTU", 1500
}
```

`Build` is defined in terms of `Walk`, so the paths you iterate and the paths
that end up in the tree cannot disagree.

### Decoding back into the struct

`Apply` is the inverse. It takes the flat `path → bytes` map that
`Graph.Leaves` already returns, so the client side needs nothing else:

```go
leaves, err := u.Graph.Leaves(u.Hash)
err = structtree.Apply(&config, leaves)     // config is now current
```

**The two sides share one baseline struct.** The server walks that type into a
tree; the client applies the tree back into the same type. When the two live in
different repositories, a small package holding the struct is what they both
depend on, alongside treevial itself — `examples/consumer/settings` is exactly
that.

The tree is the source of truth, so applying it settles the whole value: a
field whose path the tree does not carry is zeroed, and a pointer to a struct
with no paths beneath it is set to nil. Applying the same leaves repeatedly
leaves the value unchanged, and a field that stops being sent is cleared rather
than left stale. Paths the struct has no field for are ignored, so a server can
add fields before its clients know about them; a client that wants to notice
them can compare the keys of `leaves` against the paths `Walk` yields for its
own type.

A `Mapper`'s `Decode` must be the counterpart of the `Encode` that wrote the
tree — which is why they are fields of one value rather than separate
arguments.

### Following pushes incrementally

`Apply` decodes every leaf. `ApplySince` decodes only what moved, skipping any
subtree whose hash has not changed — the mirror of the pruning the sending side
does to decide what to transmit. An `Update` carries exactly the two hashes it
needs:

```go
err := structtree.ApplySince(&config, u.Graph, u.Previous, u.Hash)
```

On the first push `Previous` is the zero hash, so everything is decoded. On a
push that moved one field, one leaf is decoded however large the rest of the
struct is — and the value still comes out wholly current, because the parts
that were skipped were already right.

The catch, and it is the whole basis of the shortcut: **`dst` must already hold
the value at `old`.** The hashes say what moved between the two trees, not what
`dst` contains. Passing `plumbing.ZeroHash` as `old` decodes everything and is
always safe, and a baseline the source cannot resolve degrades to that rather
than silently skipping work.

The round trip is exact: `TestClientRebuildsTheStructTheServerPublished`
decodes a received tree into the struct, rebuilds a tree from it, and requires
the same root hash — which only holds if nothing was lost on the way.

Leaf bytes come from an `Encoder`. The default stores protobuf messages as
deterministic wire bytes and everything else as JSON; a `Mapper` carries your
own. Whatever you choose **must be deterministic** — an unchanged value that
re-encodes differently looks like a change to everyone downstream.

This is what makes the git machinery pay off: change one deeply nested field
and only that blob and the trees on its path are new, however large the rest of
the struct is. That is what gets sent, and what `ApplySince` has to decode.

### Rebuilding only what changed

Producing those objects is a separate question from sending them. `Build`
encodes and hashes every leaf, so it pays for the whole value however little of
it moved — on a few hundred megabytes that is most of a second for a one-field
change. A `Builder` keeps the hashes from its last build and re-encodes only
what you tell it has moved:

```go
b, err := structtree.NewBuilder(store, cfg)
root, err := b.Build()                        // everything, the first time

cfg.Network.Primary.MTU = 9000
root, err = b.Build(&cfg.Network.Primary.MTU) // that leaf and the trees above it
```

**A pointer stands for everything beneath it**, so one rule covers a field, a
subtree, an entry of a map, and the whole value:

| argument | recomputed |
|---|---|
| `&cfg.Network.Primary.MTU` | that leaf, and the trees above it |
| `&cfg.Network` | every leaf under `Network` |
| `cfg.Ports["eth0"]` | that entry of the map |
| `cfg` | everything — the wildcard |
| *(none)* | everything |

Pointers rather than path strings, so nothing can be mistyped or left behind by
a rename. A pointer is resolved against an index of addresses the last build
recorded — one lookup, not a search — and then checked by descending the path it
names, so a stale address is refused rather than blamed on whatever field lives
there now. A pointer the last build never saw is an error, not a no-op:
publishing a tree without the change it was meant to carry is the one way this
could quietly go wrong.

Measured on a map of ten thousand entries, fifty thousand leaves in all: a full
build 213 ms, a one-field build **5 ms**, one whole entry 5 ms. Nothing outside
the declared path is encoded, hashed or even looked at; what remains is mostly
the map's own tree object, which has ten thousand entries and has to be written
again whenever any of them moves.

### What it will not notice

Only what you declare, and what lies beneath it, is looked at. So a value
changed elsewhere keeps the hash it had — and so does a **member added or
removed** elsewhere, since adding a key to a map changes that map's shape and a
declaration naming something else cannot know about it.

Declare the thing whose shape changed — the map, the struct — and its whole
subtree is walked afresh, which picks up members coming and going inside it:

```go
cfg.Ports["eth2"] = &Interface{}
root, err = b.Build(&cfg.Ports)      // the new key is in the tree
```

Or call `b.Build()` with no arguments, which walks everything and is always
right. There are tests for each of those, including one that pins the miss so it
stays a documented bargain rather than a surprise.

A map of values hands out copies, so its entries have no address to take — Go
will not let a field inside one be assigned to either. Keep pointers in a map
whose entries you mean to change one at a time.

A server, which supplies each client's objects through a `Provider`:

```go
type provider struct{}

func (provider) Prepare(ref string) (*objects.Store, plumbing.Hash, error) {
	store := objects.NewStore()
	// …build the tree behind ref, keeping a structtree.Builder alongside
	// it if you will be changing the value later…
	return store, root, nil
}

func (provider) Release(ref string) { /* drop whatever Prepare set up */ }

srv := server.New(provider{})
go srv.Serve(lis)

srv.SetHead("refs/heads/printer-7/config", newRoot)   // pushes immediately
```

`examples/consumer` is a module of its own that does exactly this, and
`TestSeparateModuleConsumersBuild` builds it — so the claim that each side can
be consumed independently is checked, not asserted.

The wire format is described in [PROTOCOL.md](PROTOCOL.md): pkt-line framed
messages over a plain TCP connection, which is the framing git itself uses. Its
Go implementation is deliberately **internal**: the supported surface is the Go
API above, and anyone implementing another language's client works from
PROTOCOL.md.

## Shape of it

```
client                                     server
  │  register <ref> <synced>  ───────────────►│   take the ref verbatim, prepare
  │                                          │   its objects, walk them pruning
  │                                          │   what "synced" already covers
  │◄──────  update <hash> <count>            │
  │◄──────  <pack bytes as pkt-lines>        │   framed as it is encoded
  │◄──────  0000                             │   flush-pkt ends the pack
  │  ack <hash>  ───────────────────────────►│   subscriber is now synced
  │                                          │
  │              … ref moves …               │
  │◄──────  update <hash> <count>            │   unprompted: only changed objects
```

One long-lived TCP connection carries all of it, so the server can push the
instant a ref changes. Every message is a pkt-line — four hex length digits then
the payload — which is git's own framing, and comes from go-git rather than
being hand-rolled here.

Refs point **directly at a tree**. No commit objects are involved.

## The client names the head

The opening message carries the ref the client wants, and **the server takes it
verbatim**. It derives nothing from it, and nothing about
`refs/heads/<id>/config` is special to it — a client may ask for
`refs/devices/hall-a/row-3/seat-9` and be served just the same. What a ref
stands for is the provider's business.

No package here knows of any scheme for deriving a ref. `demo/cmd/client`
happens to build one as `refs/heads/<id>/config` from an identifier it is given,
but that is that program's own convention and lives in its `main.go` — an
application maps its identities to refs however suits it.

The one rule both sides share is `treevial.ValidateRef`, a conservative subset
of git's own: the client applies it before sending, so an unusable ref fails
without a round trip, and the server applies it to whatever it is sent.

## A client's state is one hash

Holding a tree means holding everything under it, so the `synced` hash on the
register line — the tree the client last finished interpreting — tells the
server both what to send and exactly where the client stands. Neither side keeps an inventory of
objects.

`Store.SelectSince(from, to)` walks the new tree and prunes any subtree already
reachable from `from`, which is what makes an update after a small change cost
a few objects instead of the whole graph. The `Ack` is how the server learns
the client has caught up; until it arrives the client counts as behind. A
client that reconnects and names what it holds is sent nothing at all.

## Per-ref data, released on disconnect

The server owns no objects of its own. It asks the `Provider` for a ref's graph
when a client subscribes to it, and hands it back when the connection ends.

Give each ref a store of its own and releasing one is nothing more than
dropping a reference — there is no shared graph to prune. `Release` runs from a
`defer` in the connection handler, so it fires however the connection ended: a
clean close, a cancelled context, a broken connection. Only one connection per
ref is served at a time (`exists` otherwise), which gives prepared data exactly
one owner.

## Connections do not die

Neither side ever sets a deadline. The server pushes when it has something to
say, which may be hours after the last byte, and a deadline would tear down a
perfectly good connection in the meantime. Both ends enable TCP keepalive at 30
seconds so NATs and middleboxes do not forget a quiet connection; the probes do
not close a healthy one.

That is the whole of it: there is nothing in a TCP connection that counts down
while it is quiet, and nothing that punishes a peer for talking. A subscription
ends when one side closes the connection, and not before.

## Try the example

```console
$ go run ./demo/cmd/server                     # prepares data per ref on connect
$ go run ./demo/cmd/client -id printer-7       # asks for refs/heads/printer-7/config
$ go run ./demo/cmd/client -id sensor-3        # asks for its own ref, served independently
```

Each ref gets a configuration struct of its own, personalised with the name the
provider reads out of it, and the server changes `Network.Primary.MTU` a few
seconds after each subscriber has caught up:

```
[refs/heads/printer-7/config] prepared -> 35ae729e…, 11 leaves walked from the struct (1 refs held)
[refs/heads/printer-7/config] synced at 35ae729e…; setting Network.Primary.MTU in 2s
[refs/heads/printer-7/config] moving -> 30e5ce80… and pushing
[refs/heads/printer-7/config] subscriber gone; released its config and objects (0 refs held)
```

Each push prints three views: the serialised tree, the paths that moved, and
the part of the struct they landed in — the deepest field containing every
change, which on the first push is the whole value and after a one-field change
is the struct holding that field.

`Graph.Listing` renders the received objects the way `git ls-tree -r -t` would,
which is where the leaf rules become visible — a slice or a protobuf message is
one `blob`, a nested struct a `tree`:

```
push 1: refs/heads/printer-7/config -> 35ae729e…, 17 objects received
  tree 35ae729ecbb6c621dc5bc6ac2d8efec6f83c2805
    040000 tree 8076d140…	Audio
    100644 blob 413477a4…	Audio/Delay             ← proto.Message: one blob
    100644 blob d594cf69…	Audio/Gain
    040000 tree 7581aab3…	Device
    100644 blob 664684c1…	Device/Installed        ← tagged `treevial:"leaf"`
    040000 tree eb5bd8cd…	Device/Location         ← untagged struct: a subtree
    100644 blob 60c9f71d…	Device/Location/Room
    …
    040000 tree 92b4e348…	Network
    100644 blob ee000805…	Network/DNS             ← a slice: one blob
    040000 tree be9911a3…	Network/Primary
    100644 blob 37021f4a…	Network/Primary/MTU
  + Audio/Delay  413477a4  "\x10\x80\xb6\xdc\x05"
  + Device/Installed  664684c1  "\"2023-11-14T22:13:20Z\""
  …
  *shared.Config = { …the whole value… }

push 2: refs/heads/printer-7/config -> 30e5ce80…, 4 objects received
    040000 tree 8076d140…	Audio                   ← unchanged
    100644 blob 413477a4…	Audio/Delay             ← unchanged
    …
    040000 tree 40018139…	Network                 ← new
    040000 tree 9d078488…	Network/Primary         ← new
    100644 blob bc5d0b77…	Network/Primary/MTU     ← new
  ~ Network/Primary/MTU  bc5d0b77  "9000"
  Network/Primary = {
    "Address": "10.0.0.7",
    "MTU": 9000
  }
```

Four object hashes moved between those two listings — the rewritten blob and
the three trees above it — and the other thirteen are identical. That is the
whole mechanism, visible: it is why the second push carried four objects, why
`SelectSince` had nothing else to send, and why `ApplySince` decoded one leaf.

Seventeen objects the first time — eleven leaves and six trees — and four the
second: the rewritten blob plus `Primary`, `Network` and the root.

The value it prints is a `shared.Config` — the same type the server walked —
filled in by `ApplySince`, so on the second push exactly one leaf was decoded
even though the whole struct is current.

## Layout

| Path | Role |
|---|---|
| `treevial.go` | What both sides must agree on: usable refs, and the error vocabulary |
| `client/` | Dials, names a head, feeds the pack to the interpreter, acknowledges |
| `receive/` | Interprets an arriving packfile object by object; no storage of any kind |
| `server/` | Listener, subscriber registry, per-ref data lifecycle, push on ref change |
| `objects/` | In-memory store, `SelectSince` object arithmetic, pack encoding |
| `structtree/` | Walks a Go struct with reflect onto a tree and back again, whole or incrementally in either direction |
| `internal/wire/` | The protocol: pkt-line framed messages over a connection |
| `demo/cmd/` | The runnable example: a server and a client |
| `demo/shared/` | The configuration struct both halves of the example share, used by the tests too |
| `internal/e2e/` | Client and server together over a real TCP listener |
| `examples/consumer/` | A separate module: a shared `settings` struct, and each side importing only its own half |
| `PROTOCOL.md` | The wire contract |

## A note on go-git's packfile API

go-git ships `packfile.Parser` with an `Observer` interface that looks made for
this, but it always passes `nil` object content (`parser.go` calls
`onInflatedObjectContent` with a nil buffer) and needs a storer to resolve
deltas. `receive` therefore drives `packfile.Scanner` directly, which hands
over the inflated bytes and needs no storage. The server encodes with a pack
window of zero so nothing is a delta, which is what lets the client get by
without any object it has not already been given.

## Tests

```console
$ go test ./...             # includes building examples/consumer
$ go test -short ./...      # skips the separate-module build
$ go test -race ./...
```

`.github/workflows/test.yml` runs the same checks on every pull request and on
every push to `main`: gofmt, vet, the suite under the race detector, the example
module, and `go mod tidy` against both modules to catch a dependency added
without tidying.
