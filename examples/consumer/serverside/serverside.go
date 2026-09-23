// Package serverside is the other half of the compile fence: a server
// repository, in a module of its own, importing only the server side of
// treevial and never mentioning the client package.
//
// See clientside for what this is for and why it is written as signatures
// rather than as a running program. demo/cmd/server is the one that runs.
package serverside

import (
	"context"
	"io"
	"net"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/zonque/treevial/objects"
	"github.com/zonque/treevial/server"
	"github.com/zonque/treevial/structtree"

	"github.com/zonque/treevial-consumer-example/settings"
)

// Listening, and pushing when a ref moves.
var (
	_ func(server.Provider) *server.Server = server.New

	_ func(*server.Server, net.Listener) error          = (*server.Server).Serve
	_ func(*server.Server)                              = (*server.Server).Stop
	_ func(*server.Server, string, plumbing.Hash) error = (*server.Server).SetHead
	_ func(*server.Server, string) plumbing.Hash        = (*server.Server).Head
	_ func(*server.Server) []server.Subscription        = (*server.Server).Subscribers
	_ func(*server.Server, server.Watcher)              = (*server.Server).Watch
	_ func(*server.Server, server.Forwarder)            = (*server.Server).Forward
)

// The interfaces an application implements, and the values they are handed.
var (
	_ server.Provider  = provider{}
	_ server.Watcher   = server.WatcherFunc(func(server.Event) {})
	_ server.Forwarder = server.ForwarderFunc(func(context.Context, server.Push) (*server.Pack, error) {
		return nil, nil
	})
)

// provider is the smallest thing that satisfies server.Provider, present so
// that the interface has to be satisfiable from outside the module.
type provider struct{}

func (provider) Prepare(ref string) (*objects.Store, plumbing.Hash, error) {
	store := objects.NewStore()

	root, err := structtree.Build(store, &settings.Settings{
		Owner: settings.Owner{Name: ref},
	})
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	return store, root, nil
}

func (provider) Release(string) {}

// A subscription, a push and a pack, field by field.
var (
	subscription server.Subscription
	push         server.Push
	pack         server.Pack
	event        server.Event
)

var (
	_ string        = subscription.Ref
	_ string        = subscription.ClientID
	_ string        = subscription.Addr
	_ plumbing.Hash = subscription.Head
	_ plumbing.Hash = subscription.Synced
	_ int64         = subscription.Sent
	_ int64         = subscription.Received

	_ string        = push.Ref
	_ string        = push.ClientID
	_ string        = push.Addr
	_ plumbing.Hash = push.Have
	_ plumbing.Hash = push.Want

	_ int       = pack.Objects
	_ io.Reader = pack.Body

	_ server.EventKind    = event.Kind
	_ server.Subscription = event.Subscription
)

// The object store a provider hands over, and the struct walker that fills it.
var (
	_ func() *objects.Store                                                       = objects.NewStore
	_ func(*objects.Store, []byte) (plumbing.Hash, error)                         = (*objects.Store).AddBlob
	_ func(*objects.Store, []object.TreeEntry) (plumbing.Hash, error)             = (*objects.Store).AddTree
	_ func(*objects.Store, plumbing.Hash) (*object.Tree, error)                   = (*objects.Store).Tree
	_ func(*objects.Store, plumbing.Hash, plumbing.Hash) ([]plumbing.Hash, error) = (*objects.Store).SelectSince
	_ func(*objects.Store, io.Writer, []plumbing.Hash) (plumbing.Hash, error)     = (*objects.Store).EncodePack
	_ func(*objects.Store, plumbing.Hash, string, []byte) (plumbing.Hash, error)  = (*objects.Store).ReplaceBlob

	_ func(*objects.Store, any) (plumbing.Hash, error)         = structtree.Build
	_ func(*objects.Store, any) (*structtree.Builder, error)   = structtree.NewBuilder
	_ func(*structtree.Builder, ...any) (plumbing.Hash, error) = (*structtree.Builder).Build
)
