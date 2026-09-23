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
| `github.com/zonque/treevial/client` | `Dial`, `WithID`, `WithHistory`, `Subscribe`, `Resume`, `Update`, `Received`, `Sent` | client repositories |
| `github.com/zonque/treevial/receive` | `Interpret`, `Handler`, `Graph`, `Diff`, `Listing`, `ListingSince`, `Retain` | client repositories |
| `github.com/zonque/treevial/server` | `Server`, `Provider`, `Subscription`, `Watch`, `Event`, `Forwarder` | server repositories |
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
not have. A map keyed by anything else is reported as an error, unless it is
tagged as a leaf, which is how you ask for it in one blob. Keys are walked in
sorted order, so neither the tree nor `Walk` depends on Go's random map order.
A slice's children are named by index, and decoding reads them back as numbers
rather than as text, so order survives past ten elements. A nil or empty map, or
a slice that would have been a subtree, contributes nothing — like a struct with
nothing in it. A nil or empty slice of *scalars* is a leaf like any other, since
`nil` and `[]` are worth telling apart.

**Why a run of messages nests, and not just for granularity.** A protobuf
message stored on its own goes as its own wire bytes; one buried inside a leaf
would go as JSON, which writes a `oneof` as the wrapper the generated code uses
and then has nothing to unmarshal it back into. So a leaf that merely contains
a message is refused when the tree is built, rather than written and found
unreadable later: leave it untagged and each message gets a blob of its own, or
give the `Mapper` an `Encode` and `Decode` that know what to do with it. A
message held in an interface is refused for the same reason — nothing on the far
side would say which message to expect.

The default rule has one blind spot, worth knowing before it bites: **a
`time.Time` is a struct, is not a protobuf message, and has only unexported
fields**, so the walker descends into it, finds nothing it may read, and the
field vanishes from the tree entirely. Tag it and it is stored whole — JSON
already knows how to write a time, so no encoding of your own is needed. Any
struct of that shape needs the same treatment, and `Graph.Listing` will show you
what actually became a blob.

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

### What the graph keeps

A `Graph` accumulates, which is what makes an incremental push work at all: the
four objects a one-field change sends resolve against the rest of the tree,
which arrived earlier. Nothing is dropped on its own account, though, so the
superseded objects stay too. A thousand one-field moves leaves a graph holding
**4013 objects of which 17 are live** — the cost is small per push and
unbounded over a long run.

`WithHistory` hands that to the client:

```go
conn, err := client.Dial(ctx, addr, client.WithHistory(1))
```

The graph is then swept as each update arrives, keeping the `n` states behind
the one arriving. One is the usual answer: that is the baseline `ApplySince`,
`Diff` and `ListingSince` are asked about, so updates still cost only what
moved, and the graph settles at two states rather than growing with every push.
Ask for more only if you compare against something older than the update's own
`Previous`.

Sweeping by hand is the same thing without the option, for a caller who knows
better than a fixed number which states are worth keeping:

```go
dropped, err := u.Graph.Retain(u.Previous, u.Hash)   // and Graph.Len to watch it
```

Either way a dropped state costs work rather than correctness: an `ApplySince`
whose baseline is gone decodes everything instead of skipping what did not move.

The server side has no equivalent yet. A `Store` keeps every object it is ever
given, and it has to keep more than the client does — a subscriber sitting three
moves behind still needs the tree it is standing on, so the live set there is
the head plus every subscriber's `Synced`.

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
subtree, an entry of a map, an element of a slice, and the whole value:

| argument | recomputed |
|---|---|
| `&cfg.Network.Primary.MTU` | that leaf, and the trees above it |
| `&cfg.Network` | every leaf under `Network` |
| `cfg.Ports["eth0"]` | that entry of the map |
| `&cfg.Delays[1]` | that element of the slice |
| `cfg` | everything — the wildcard |
| *(none)* | everything |

Pointers rather than path strings, so nothing can be mistyped or left behind by
a rename. A pointer is resolved against an index of addresses the last build
recorded — one lookup, not a search — and then checked by descending the path it
names, so a stale address is refused rather than blamed on whatever field lives
there now. A pointer the last build never saw is an error, not a no-op:
publishing a tree without the change it was meant to carry is the one way this
could quietly go wrong.

Measured on a map of ten thousand entries, each a struct of three leaves, so
thirty thousand leaves in all:

```
$ go test -run '^$' -bench . ./structtree/
goos: linux
goarch: amd64
pkg: github.com/zonque/treevial/structtree
cpu: AMD Ryzen 7 PRO 7840U w/ Radeon 780M Graphics
BenchmarkBuild-16                     25      87032073 ns/op    55303609 B/op    1100618 allocs/op
BenchmarkBuilderEverything-16         19     120479792 ns/op    71937872 B/op    1160737 allocs/op
BenchmarkBuilderOneField-16          741       3048477 ns/op     3063405 B/op      40119 allocs/op
BenchmarkBuilderOneEntry-16          772       3063339 ns/op     3065942 B/op      40173 allocs/op
```

A full build is 87 ms; a declared one-field build is 3 ms, and that is the
whole point of the thing. Nothing outside the declared path is encoded, hashed
or even looked at, so what remains is mostly the map's own tree object, which
has ten thousand entries and has to be written again whenever any one of them
moves — which is also why declaring a whole entry costs the same as declaring
one field inside it.

`BenchmarkBuilderEverything` is a Builder told nothing about what moved, and it
is slower than a plain `Build` because it also records the index a later
targeted build resolves against. That index is what the 3 ms is bought with.

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

This is per subscriber, not per ref: two clients following one ref from
different starting points are sent different objects, worked out from the same
tree. Which means the walk is paid once per subscriber on every move, so what
it costs is worth knowing. On a tree of ten thousand subtrees and fifty
thousand blobs — sixty thousand objects in all:

```
$ go test -run '^$' -bench . ./objects/
goos: linux
goarch: amd64
pkg: github.com/zonque/treevial/objects
cpu: AMD Ryzen 7 PRO 7840U w/ Radeon 780M Graphics
BenchmarkSelectSinceFromNothing-16      272       8578397 ns/op    13078253 B/op    536 allocs/op
BenchmarkSelectSinceOneLeafMoved-16     321       7492818 ns/op     6024082 B/op    512 allocs/op
BenchmarkEncodePackOneLeafMoved-16      368       6413285 ns/op      814673 B/op     43 allocs/op
```

Working out that a one-field move owes a subscriber three objects takes 7.5 ms
— not far off the 8.6 ms it takes to work out that a subscriber holding nothing
owes the whole sixty thousand. Pruning saves the sending, not the deciding: a
subtree can only be pruned once the walk has established that the client is
standing on it. A `Store` remembers what each tree object parses to, which is
where the allocation counts above come from — five hundred rather than the
three hundred thousand a re-parse of every tree on every walk would cost.

## What went over the wire

Both ends count what they read and write at the one place every byte of the
protocol has to cross — the connection itself — so the figures include the
pkt-line framing and are measured rather than estimated from an object count.

A client is told what each push cost, and what the connection has cost so far:

```go
for u := range updates {
	log.Printf("%d objects, %d bytes (%d in total)",
		u.ObjectCount, u.Bytes, u.TotalBytes)
}
```

`Update.Bytes` covers the update message, the pack that followed it and the
headers around both, taken from before the message was read to after its pack
was drained. `Client.Received` and `Client.Sent` give the running totals at any
moment; a client's own traffic is one register line and one acknowledgement per
push.

The server reports the same from the other side, per subscriber:

```go
for _, sub := range srv.Subscribers() {
	log.Printf("%s: %d bytes sent, %d received", sub.Ref, sub.Sent, sub.Received)
}
```

Once a subscriber is up to date the two counts agree exactly, since they are
the same bytes seen from either end.

## Many subscribers, one ref

Any number of clients may follow the same ref, and all of them move when it
does. `SetHead` wakes every one; what each is then sent is worked out from the
hash *it* acknowledged, so a client that has been away for three moves is
brought to the current head in one push rather than walked through the states
it missed, and a slow subscriber holds nobody else up.

They are served from one prepared store. Reading it from several pushes at once
is safe; writing to it while any of them is mid-transfer is not, and that is
the one rule a provider has to keep. `Subscribers` is how to tell: when every
entry for a ref reports `Synced` equal to `Head`, no push is in flight and the
store can be rebuilt.

```go
// Everyone following this ref has taken up its current head.
func quiet(srv *server.Server, ref string) bool {
	for _, sub := range srv.Subscribers() {
		if sub.Ref == ref && sub.Synced != sub.Head {
			return false
		}
	}

	return true
}
```

Start the example server and point two clients at the same `-id` and the whole
of it shows up in the log: prepared once, moved together, and costing the same
409 bytes each.

```
[refs/heads/printer-7/config] prepared -> 35ae729e…, 11 leaves walked from the struct (1 refs held)
[refs/heads/printer-7/config] printer-7 (127.0.0.1:59090) subscribed
[refs/heads/printer-7/config] printer-7 (127.0.0.1:59090) synced at 35ae729e… (949 B sent, 141 B received)
[refs/heads/printer-7/config] printer-7 (127.0.0.1:59104) subscribed
[refs/heads/printer-7/config] printer-7 (127.0.0.1:59104) synced at 35ae729e… (949 B sent, 141 B received)
[refs/heads/printer-7/config] moving -> 30e5ce80… and pushing to 2 subscriber(s)
[refs/heads/printer-7/config] printer-7 (127.0.0.1:59090) synced at 30e5ce80… (1358 B sent, 190 B received)
[refs/heads/printer-7/config] printer-7 (127.0.0.1:59104) synced at 30e5ce80… (1358 B sent, 190 B received)
[refs/heads/printer-7/config] 2 subscriber(s) synced at 30e5ce80…; that push cost 818 bytes, 2716 sent in total
```

Both clients called themselves `printer-7` — the server is happy to have two of
them, and the address is what tells them apart.

## Watching who is connected

`Subscribers` answers *who is connected now*: one entry per connection, with
the ref it follows, the name it gave itself, where it connected from, where
that ref points, how far it has got, and what it has cost.

```go
for _, sub := range srv.Subscribers() {
	log.Printf("%-16s %-22s %s  synced %s  %d B sent",
		sub.ClientID, sub.Addr, sub.Head, sub.Synced, sub.Sent)
}
```

`Watch` answers *tell me when that changes*, so nothing has to poll for it:

```go
srv.Watch(server.WatcherFunc(func(e server.Event) {
	log.Printf("[%s] %s %s at %s", e.Ref, e.ClientID, e.Kind, e.Synced)
}))
```

There are three kinds. `Subscribed` is a client joining a ref, once that ref's
objects are ready. `Synced` is a client acknowledging a head it was pushed —
when the event's `Synced` equals its `Head`, that client is up to date.
`Unsubscribed` is a connection ending, however it ended.

What a handler may rely on:

- It runs on the goroutine serving that subscriber, with no lock of the
  server's held, so it may call `Subscribers`, `Head` or `SetHead` without
  deadlocking. Reacting to "everyone has caught up" by moving the ref again is
  the obvious thing to do from one, and it works.
- It is called in order for any one subscriber, and concurrently for different
  ones, so it must be safe to call from several goroutines at once.
- It runs while its subscriber waits, which delays that subscriber's next push
  and nobody else's. Anything slow belongs on a goroutine of the handler's own.
- By the time `Unsubscribed` arrives, that subscriber is already gone from
  `Subscribers`.

The example server keeps no state of its own about who is connected: every
line it prints and every rebuild it sets off comes from an event.

### A client may name itself

`WithID` gives a client a name it sends on its opening message, which is what
turns a listing of addresses into a listing of things:

```go
conn, err := client.Dial(ctx, addr, client.WithID("press-hall-a-7"))
```

It is a label and nothing more. The ref is what decides what a client is
served; the server derives nothing from the name, does not require it to be
unique, and serves a client that sends none exactly the same. It travels as one
field of one line, so it may not contain spaces or control characters and is
bounded at 128 bytes — `treevial.ValidateClientID` is the rule, applied by the
client before dialling and by the server on what arrives.

## Serving a ref this server does not own

In a cluster, the node a client happens to be connected to may not be the one
that owns the ref it is following — and which node that is can change while the
client sits there. A `Forwarder` fetches the objects from wherever they are and
the server writes them on as ordinary updates. **The client is told nothing and
notices nothing**: no redirect, no reconnect, no protocol change. Its connection
is as stable as the cluster is not.

```go
srv.Forward(server.ForwarderFunc(func(ctx context.Context, req server.Push) (*server.Pack, error) {
	if weOwn(req.Ref) {
		return nil, nil            // served from this server's own store
	}

	// Ask whoever does. req.Have is what this subscriber has
	// acknowledged and req.Want the head it should reach, so the
	// answer is that subscriber's delta and no more.
	count, body, err := leader.Objects(ctx, req.Ref, req.Have, req.Want)
	if err != nil {
		return nil, err
	}

	return &server.Pack{Objects: count, Body: body}, nil
}))
```

It is asked **for every push, not once per connection**, which is the point: a
re-election between two pushes takes effect on the second one, on the
connection that is already open. The `ctx` is that client's, cancelled when it
hangs up, so a fetch is not left outstanding for somebody who has gone. An
error is reported to the client as a refusal — a `*treevial.Error` with
whatever code the forwarder chose, anything else as `internal`.

Two things stay with the application, deliberately. **How the servers talk to
each other** is not treevial's business: the leader answers such a fetch with
`store.SelectSince` and `store.EncodePack`, over whatever transport the cluster
already has. And **moving the ref** is still `SetHead` — a server that owns
nothing still learns from its consensus layer that a ref has moved, and says
so. A forwarder alone pushes nothing, because nothing has told the server there
is anything to push.

One consequence worth knowing before building on it: a forwarded push asks the
owning node for the delta from a hash this client acknowledged, possibly long
ago. Whether that node still holds it is a retention question, and the answer
decides whether the client is served incrementally or has to be resynchronised
whole.

## Per-ref data, released by the last to leave

The server owns no objects of its own. It asks the `Provider` for a ref's graph
when the first client subscribes to it, and hands it back once the last one has
gone — once per ref, however many clients pass through, and never overlapping:
a `Release` always finishes before that ref can be prepared again.

Give each ref a store of its own and releasing one is nothing more than
dropping a reference — there is no shared graph to prune. `Release` runs from a
`defer` in the connection handler, so it fires however the connection ended: a
clean close, a cancelled context, a broken connection.

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
$ go run ./demo/cmd/client -id printer-7       # names itself, asks for refs/heads/printer-7/config
$ go run ./demo/cmd/client -id sensor-3        # asks for its own ref, served independently
$ go run ./demo/cmd/client -id printer-7       # a second follower of the first ref
```

Each ref gets a configuration struct of its own, personalised with the name the
provider reads out of it, and the server changes `Network.Primary.MTU` a few
seconds after every subscriber to that ref has caught up:

```
[refs/heads/printer-7/config] prepared -> 35ae729e…, 11 leaves walked from the struct (1 refs held)
[refs/heads/printer-7/config] printer-7 (127.0.0.1:55862) subscribed
[refs/heads/printer-7/config] printer-7 (127.0.0.1:55862) synced at 35ae729e… (949 B sent, 141 B received)
[refs/heads/printer-7/config] 1 subscriber(s) synced at 35ae729e… after 949 bytes; setting Network.Primary.MTU in 2s
[refs/heads/printer-7/config] moving -> 30e5ce80… and pushing to 1 subscriber(s)
[refs/heads/printer-7/config] printer-7 (127.0.0.1:55862) synced at 30e5ce80… (1358 B sent, 190 B received)
[refs/heads/printer-7/config] 1 subscriber(s) synced at 30e5ce80…; that push cost 409 bytes, 1358 sent in total
[refs/heads/printer-7/config] printer-7 (127.0.0.1:55862) unsubscribed
[refs/heads/printer-7/config] subscriber gone; released its config and objects (0 refs held)
```

Each push prints two views: the objects it moved, and the part of the struct
they landed in — the deepest field containing every change, which on the first
push is the whole value and after a one-field change is the struct holding that
field.

`Graph.ListingSince` renders the objects that moved the way `git ls-tree -r -t`
renders a whole tree, with a marker in front. It is also where the leaf rules
become visible: a slice of scalars or a protobuf message is one `blob`, a nested
struct or a map a `tree`.

```
push 1: refs/heads/printer-7/config -> 35ae729e…, 17 objects, 949 B on the wire (949 B in total)
    + 040000 tree 8076d140…	Audio
    + 100644 blob 413477a4…	Audio/Delay             ← proto.Message: one blob
    + 100644 blob d594cf69…	Audio/Gain
    + 040000 tree 7581aab3…	Device
    + 100644 blob 664684c1…	Device/Installed        ← tagged `treevial:"leaf"`
    + 040000 tree eb5bd8cd…	Device/Location         ← untagged struct: a subtree
    + 100644 blob 60c9f71d…	Device/Location/Room
    …
    + 100644 blob ee000805…	Network/DNS             ← a slice of scalars: one blob
    + 040000 tree be9911a3…	Network/Primary
    + 100644 blob 37021f4a…	Network/Primary/MTU
  *shared.Config = { …the whole value… }

push 2: refs/heads/printer-7/config -> 30e5ce80…, 4 objects, 409 B on the wire (1.3 KiB in total)
    ~ 040000 tree 40018139…	Network
    ~ 040000 tree 9d078488…	Network/Primary
    ~ 100644 blob bc5d0b77…	Network/Primary/MTU
  Network/Primary = {
    "Address": "10.0.0.7",
    "MTU": 9000
  }
```

That second push is the whole mechanism in three lines: one blob moved, and the
two trees above it had to follow. Nothing else is listed because nothing else
moved — which is why the push carried four objects rather than seventeen, why
`SelectSince` had nothing else to send, and why `ApplySince` decoded one leaf.
On the wire that is 409 bytes against the first push's 949, framing included.

Seventeen objects the first time — eleven leaves and six trees — and four the
second: the rewritten blob plus `Primary`, `Network` and the root, the root
being the fourth and not listed, since a listing names what is *in* a tree.

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
$ go test -run '^$' -bench . ./structtree/ ./objects/   # the figures quoted above
```

`.github/workflows/test.yml` runs the same checks on every pull request and on
every push to `main`: gofmt, vet, the suite under the race detector, the example
module, and `go mod tidy` against both modules to catch a dependency added
without tidying.
