# gats — reversed-role git object transfer

`gats` moves git objects the wrong way round. The client dials the server, but
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
go get github.com/holoplot/gats
```

| Import | For | Pulls in |
|---|---|---|
| `github.com/holoplot/gats` | The shared contract: `IDHeader`, `RefFor`, `ValidateID` | both sides need it |
| `github.com/holoplot/gats/client` | `Dial`, `Subscribe`, `Resume`, `Update` | client repositories |
| `github.com/holoplot/gats/receive` | `Interpret`, `Handler`, `Graph`, `Diff` | client repositories |
| `github.com/holoplot/gats/server` | `Server`, `Provider`, `ClientState` | server repositories |
| `github.com/holoplot/gats/objects` | `Store`, `SelectSince`, `EncodePack`, `ReplaceBlob` | server repositories |

A client:

```go
conn, err := client.Dial(ctx, "gats.internal:9418")
updates, err := conn.Subscribe(ctx, "printer-7")   // served refs/heads/printer-7/config

for u := range updates {
	changes, _ := u.Graph.Diff(u.Previous, u.Hash)  // only what moved
	for _, c := range changes {
		log.Printf("%s %s = %q", c.Kind.Symbol(), c.Path, c.Content)
	}
}
```

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

The wire format lives in `proto/gats.proto`. Its generated Go bindings are
deliberately **internal**: the supported surface is the Go API above, and
anyone implementing another language's client works from the `.proto` file.

## Shape of it

```
client                                     server
  │  header: gats-client-id: printer-7  ────►│   validate the ID, prepare
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

A client identifies itself in the `gats-client-id` request header, and that ID
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
$ go run ./cmd/gats-server                       # prepares data per client on connect
$ go run ./cmd/gats-client -id printer-7         # served refs/heads/printer-7/config
$ go run ./cmd/gats-client -id sensor-3          # served its own tree, independently
```

Each client gets its own ten-leaf tree, labelled with its own ID, and the
server rewrites a leaf a few seconds after each one has synced:

```
[printer-7] prepared refs/heads/printer-7/config -> 71319cbf… with 10 blob leaves (1 clients held)
[sensor-3]  prepared refs/heads/sensor-3/config  -> a331a759… with 10 blob leaves (2 clients held)
[printer-7] moving refs/heads/printer-7/config -> e92f62ec… and pushing
[sensor-3]  disconnected; released its objects (1 clients held)
[printer-7] disconnected; released its objects (0 clients held)
```

The first push prints the whole hierarchy. Every push after that prints only
what changed, diffed against the previous state:

```
push 1: refs/heads/printer-7/config -> 71319cbf…, 14 objects received
a/
  leaf-00  d7badb3f  "leaf-00 printer-7\n"
  …
push 2: refs/heads/printer-7/config -> e92f62ec…, 4 objects received
  ~ b/c/leaf-07  165e5430  "leaf-07 v2\n"
```

## Layout

| Path | Role |
|---|---|
| `gats.go` | The ID↔ref contract shared by both sides |
| `client/` | gRPC client: identifies itself, feeds chunks to the interpreter, acknowledges |
| `receive/` | Interprets an arriving packfile object by object; no storage of any kind |
| `server/` | gRPC server: client registry, per-client data lifecycle, push on ref change |
| `objects/` | In-memory store, `SelectSince` object arithmetic, pack encoding |
| `internal/gatspb/` | Generated wire bindings |
| `internal/demo/` | The ten-leaf example tree, used by `cmd/` and the tests |
| `internal/e2e/` | Client and server together over a real TCP listener |
| `examples/consumer/` | A separate module, proving each side is independently importable |
| `proto/gats.proto` | The wire contract |

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
