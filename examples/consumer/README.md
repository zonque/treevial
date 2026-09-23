# Separate-module consumers

This directory is its own Go module, depending on `github.com/zonque/treevial`
through a `replace` directive. It is a **compile fence, not a demonstration**:
nothing in here runs, and nothing in here is meant to be read as an example of
how to use treevial. `demo/cmd/client` and `demo/cmd/server` are that, and they
run.

What this checks is the one thing no test inside the treevial module can. In
Go, code within a module may import that module's `internal/` packages and may
name internal types in exported signatures; a separate module may do neither.
So a second module that names every type the public API mentions will stop
compiling the moment an internal type reaches one of those signatures.

`clientside` and `serverside` therefore write the public surface out as typed
declarations — `var _ func(context.Context, string, ...client.Option)
(*client.Client, error) = client.Dial` and so on — rather than calling
anything. Spelling the signatures out is what makes the check total: a running
example only names the handful of types it happens to touch.

`TestSeparateModuleConsumersBuild` in the main module builds this one. It is
skipped under `go test -short`.

## What each half pulls in

`settings` holds the baseline struct the two sides share: the server walks it
into a tree, the client applies the tree back into it. When the two sides live
in different repositories, a package like this is what they both depend on,
alongside treevial itself.

```console
$ go list -deps ./clientside | grep treevial/
github.com/zonque/treevial/internal/wire
github.com/zonque/treevial/receive
github.com/zonque/treevial/client
github.com/zonque/treevial/objects
github.com/zonque/treevial/structtree

$ go list -deps ./serverside | grep treevial/
github.com/zonque/treevial/objects
github.com/zonque/treevial/internal/wire
github.com/zonque/treevial/server
github.com/zonque/treevial/structtree
```

The halves are disjoint where it counts: the client side never reaches
`treevial/server`, and the server side never reaches `treevial/client` or
`treevial/receive`.

`treevial/objects` appears on both, which is worth being plain about — the
client side picks it up through `structtree`, whose `Build` writes into an
`*objects.Store`. A client that only reads leaves off the wire can use
`receive` on its own and never touch the object store; one that maps a Go
struct cannot, because the package that maps it is the same package the server
builds with. `internal/wire` appears on both for the same kind of reason, as a
dependency rather than as something either side may import directly — the
refusal above is exactly what the fence tests.

In a real repository the `replace` line goes away and the dependency is an
ordinary `require github.com/zonque/treevial v…`.
