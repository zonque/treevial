# treevial — reversed-role git object transfer

`treevial` moves git objects the wrong way round. The client dials the server, but
it never asks for anything: it states who it is, names the tree it already
holds, and then the **server** prepares that client's data and pushes objects
down the connection whenever its ref moves.

Neither side touches the filesystem. There is no repository, no `.git`
directory, no temporary pack file. The server keeps its object graph in memory,
and the client interprets each object as it is inflated off the wire and throws
the bytes away.

## Using it as a library

The two sides are separate packages, so a client repository and a server
repository can each depend on only what it needs.

```console
go get github.com/holoplot/treevial
```

| Import | For | Pulls in |
|---|---|---|
| `github.com/holoplot/treevial` | The shared contract: `IDHeader`, `RefFor`, `ValidateID` | both sides need it |
| `github.com/holoplot/treevial/client` | `Dial`, `Subscribe`, `Resume`, `Update` | client repositories |
| `github.com/holoplot/treevial/receive` | `Interpret`, `Handler`, `Graph`, `Diff` | client repositories |
| `github.com/holoplot/treevial/server` | `Server`, `Provider`, `ClientState` | server repositories |
| `github.com/holoplot/treevial/objects` | `Store`, `SelectSince`, `EncodePack`, `ReplaceBlob` | server repositories |
| `github.com/holoplot/treevial/structtree` | `Walk`, `Build`, `Apply`, `ApplySince`, `Encoder`, `Decoder` | both sides, when syncing a Go value |

A client:

```go
conn, err := client.Dial(ctx, "treevial.internal:9418")
updates, err := conn.Subscribe(ctx, "printer-7")   // served refs/heads/printer-7/config

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

**A field is a leaf if it is not a struct, or if it is a struct implementing
`proto.Message`.** So an `int`, a `[]string` and a `map` are each stored whole
in one blob; a generated protobuf message is one blob of its own wire bytes
rather than a subtree of its internal fields; and a plain nested struct becomes
a subtree. Unexported fields are skipped, and so are nil pointers — which makes
a field going nil read as a deletion and a field appearing read as an addition.

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

`ApplyWith` takes a `Decoder`, which must be the counterpart of the `Encoder`
that wrote the tree.

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
deterministic wire bytes and everything else as JSON; pass your own to
`BuildWith`. Whatever you choose **must be deterministic** — an unchanged value
that re-encodes differently looks like a change to everyone downstream.

This is what makes the git machinery pay off: change one deeply nested field
and only that blob and the trees on its path are new, however large the rest of
the struct is.

A server, which supplies each client's objects through a `Provider`:

```go
type provider struct{}

func (provider) Prepare(clientID string) (*objects.Store, plumbing.Hash, error) {
	store := objects.NewStore()
	// …build the client's tree…
	return store, root, nil
}

func (provider) Release(clientID string) { /* drop whatever Prepare set up */ }

srv := server.New(provider{})
go srv.Serve(lis)

srv.SetHead("printer-7", newRoot)   // pushes immediately
```

`examples/consumer` is a module of its own that does exactly this, and
`TestSeparateModuleConsumersBuild` builds it — so the claim that each side can
be consumed independently is checked, not asserted.

The wire format lives in `proto/treevial.proto`. Its generated Go bindings are
deliberately **internal**: the supported surface is the Go API above, and
anyone implementing another language's client works from the `.proto` file.

## Shape of it

```
client                                     server
  │  header: treevial-client-id: printer-7  ────►│   validate the ID, prepare
  │                                          │   refs/heads/printer-7/config
  │  Register{synced}  ─────────────────────►│   walk that ref, pruning what
  │                                          │   "synced" already covers
  │◄──────  UpdateBegin{hash, count}         │
  │◄──────  PackChunk{...} × n               │   pack streams out as it is encoded
  │◄──────  UpdateEnd{}                      │
  │  Ack{hash}  ────────────────────────────►│   subscriber is now synced
  │                                          │
  │              … ref moves …               │
  │◄──────  UpdateBegin{...}                 │   unprompted: only changed objects
```

One long-lived gRPC bidirectional stream carries all of it, so the server can
push the instant a ref changes.

Refs point **directly at a tree**. No commit objects are involved.

## A client is its ID

A client identifies itself in the `treevial-client-id` request header, and that ID
alone decides what it is served: its data is published at
`refs/heads/<client-id>/config`. Nothing in the stream names a ref.

The ID is interpolated into a ref path, so it is validated at the boundary —
letters, digits, `-`, `_`, `.`, bounded length, and none of the forms git
refuses or that could climb out of the path. The root package holds that one
rule, and both sides use it.

## A client's state is one hash

Holding a tree means holding everything under it, so `Register.synced` — the
tree the client last finished interpreting — tells the server both what to send
and exactly where the client stands. Neither side keeps an inventory of
objects.

`Store.SelectSince(from, to)` walks the new tree and prunes any subtree already
reachable from `from`, which is what makes an update after a small change cost
a few objects instead of the whole graph. The `Ack` is how the server learns
the client has caught up; until it arrives the client counts as behind. A
client that reconnects and names what it holds is sent nothing at all.

## Per-client data, released on disconnect

The server owns no objects of its own. It asks the `Provider` for a client's
graph as that client connects, and hands it back when the connection ends.

Give each client a store of its own and releasing one is nothing more than
dropping a reference — there is no shared graph to prune. `Release` runs from a
`defer` in the stream handler, so it fires however the connection ended: a
clean close, a cancelled context, a broken stream. Only one connection per ID
is served at a time (`AlreadyExists` otherwise), which gives a client's
prepared data exactly one owner.

## Connections do not die

Every mechanism gRPC has for closing a connection by itself is disabled, on
both sides. The server sets `MaxConnectionIdle`, `MaxConnectionAge` and its
keepalive `Time`/`Timeout` to the maximum duration, so it never reaps an idle
connection and never probes a client. Its enforcement policy sets `MinTime` to
one nanosecond and permits pings without a stream — without that, gRPC's default
policy answers any client pinging more often than every five minutes with
`GOAWAY ENHANCE_YOUR_CALM` and closes the connection after three strikes, which
would take a subscription down mid-flight. The client sets its ping timeout to
the maximum too, so an unanswered ping is never grounds for hanging up, and
disables connection idle mode.

`TestServerNeverAnswersPingsWithEnhanceYourCalm` holds this down by speaking
raw HTTP/2 to the server and flooding it with PING frames.

Note the flip side: this removes every *voluntary* close, so a genuinely dead
peer is never detected either, and a client entry survives until the OS TCP
stack gives up. If you later want dead peers cleaned up while healthy idle
connections stay immortal, the lever is the server's keepalive `Time`/`Timeout`,
not the enforcement policy.

## Try the example

```console
$ go run ./cmd/treevial-server                       # prepares data per client on connect
$ go run ./cmd/treevial-client -id printer-7         # served refs/heads/printer-7/config
$ go run ./cmd/treevial-client -id sensor-3          # served its own tree, independently
```

Each client gets its own configuration struct, personalised with its ID, and
the server changes `Network.Primary.MTU` a few seconds after each one has
synced:

```
[printer-7] prepared refs/heads/printer-7/config -> df0e0e1c…, 10 leaves walked from the struct
[printer-7] synced at df0e0e1c…; setting Network.Primary.MTU in 2s
[printer-7] moving refs/heads/printer-7/config -> b501ed17… and pushing
[printer-7] disconnected; released its config and objects (0 clients held)
```

Each push prints three views: the serialised tree, the paths that moved, and
the part of the struct they landed in — the deepest field containing every
change, which on the first push is the whole value and after a one-field change
is the struct holding that field.

`Graph.Listing` renders the received objects the way `git ls-tree -r -t` would,
which is where the leaf rules become visible — a slice or a protobuf message is
one `blob`, a nested struct a `tree`:

```
push 1: refs/heads/printer-7/config -> df0e0e1c…, 16 objects received
  tree df0e0e1cd7146ab580338deb67e66bd05d42c1e8
    040000 tree 8076d140…	Audio
    100644 blob 413477a4…	Audio/Delay             ← proto.Message: one blob
    100644 blob d594cf69…	Audio/Gain
    040000 tree 323551cb…	Device
    040000 tree eb5bd8cd…	Device/Location
    100644 blob 60c9f71d…	Device/Location/Room
    …
    040000 tree 92b4e348…	Network
    100644 blob ee000805…	Network/DNS             ← a slice: one blob
    040000 tree be9911a3…	Network/Primary
    100644 blob 37021f4a…	Network/Primary/MTU
  + Audio/Delay  413477a4  "\x10\x80\xb6\xdc\x05"
  …
  *demo.Config = { …the whole value… }

push 2: refs/heads/printer-7/config -> b501ed17…, 4 objects received
  tree b501ed1768b41ecd5086ec43345aece2a0fb5d1c
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
the three trees above it — and the other twelve are identical. That is the
whole mechanism, visible: it is why the second push carried four objects, why
`SelectSince` had nothing else to send, and why `ApplySince` decoded one leaf.

Sixteen objects the first time — ten leaves and six trees — and four the
second: the rewritten blob plus `Primary`, `Network` and the root.

The value it prints is a `demo.Config` — the same type the server walked —
filled in by `ApplySince`, so on the second push exactly one leaf was decoded
even though the whole struct is current.

## Layout

| Path | Role |
|---|---|
| `treevial.go` | The ID↔ref contract shared by both sides |
| `client/` | gRPC client: identifies itself, feeds chunks to the interpreter, acknowledges |
| `receive/` | Interprets an arriving packfile object by object; no storage of any kind |
| `server/` | gRPC server: client registry, per-client data lifecycle, push on ref change |
| `objects/` | In-memory store, `SelectSince` object arithmetic, pack encoding |
| `structtree/` | Walks a Go struct with reflect onto a tree, and applies a tree back into one, whole or incrementally |
| `internal/treevialpb/` | Generated wire bindings |
| `internal/demo/` | The example configuration struct, used by `cmd/` and the tests |
| `internal/e2e/` | Client and server together over a real TCP listener |
| `examples/consumer/` | A separate module: a shared `settings` struct, and each side importing only its own half |
| `proto/treevial.proto` | The wire contract |

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
