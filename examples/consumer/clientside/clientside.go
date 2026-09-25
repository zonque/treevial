// Package clientside is one half of a compile fence: a client repository, in a
// module of its own, importing only the client side of treevial.
//
// It is deliberately not a demonstration — demo/cmd/client is that, and runs.
// What this is for is the one thing no test inside the treevial module can
// check: that an outside consumer can name every type the public API mentions.
// In-module code may import internal packages and may name internal types in
// exported signatures; a separate module may do neither. So if an internal
// type ever reached one of the signatures written out below, this package
// would stop compiling, and TestSeparateModuleConsumersBuild would say so.
//
// Writing the signatures out, rather than calling the functions, is what makes
// the check total: every parameter and result type here has to be spellable
// from outside, including the ones a running example would never mention.
package clientside

import (
	"context"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/zonque/treevial"
	"github.com/zonque/treevial/client"
	"github.com/zonque/treevial/receive"
	"github.com/zonque/treevial/structtree"

	"github.com/zonque/treevial-consumer-example/settings"
)

// What both sides must agree on.
var (
	_ func(string) error                                       = treevial.ValidateRef
	_ func(string) error                                       = treevial.ValidateClientID
	_ func(error) treevial.ErrorCode                           = treevial.CodeOf
	_ func(treevial.ErrorCode, string, ...any) *treevial.Error = treevial.Errorf
)

// Dialling, naming a head, and following it.
var (
	_ func(context.Context, string, ...client.Option) (*client.Client, error) = client.Dial
	_ func(string) client.Option                                              = client.WithID
	_ func(int) client.Option                                                 = client.WithHistory

	_ func(*client.Client, context.Context, string) (<-chan client.Update, error)                                = (*client.Client).Subscribe
	_ func(*client.Client, context.Context, string, plumbing.Hash, *receive.Graph) (<-chan client.Update, error) = (*client.Client).Resume
	_ func(*client.Client) error                                                                                 = (*client.Client).Close
	_ func(*client.Client) error                                                                                 = (*client.Client).Err
	_ func(*client.Client) string                                                                                = (*client.Client).ID
	_ func(*client.Client) int64                                                                                 = (*client.Client).Received
	_ func(*client.Client) int64                                                                                 = (*client.Client).Sent
)

// An update carries these, and nothing here may be an internal type either.
var update client.Update

var (
	_ string         = update.Ref
	_ string         = update.ServerID
	_ string         = update.OriginID
	_ plumbing.Hash  = update.Hash
	_ plumbing.Hash  = update.Previous
	_ int            = update.ObjectCount
	_ int64          = update.Bytes
	_ int64          = update.TotalBytes
	_ *receive.Graph = update.Graph
)

// Interpreting what arrives, without a store of any kind.
var (
	_ func() *receive.Graph                                                        = receive.NewGraph
	_ func(*receive.Graph, plumbing.Hash) (map[string][]byte, error)               = (*receive.Graph).Leaves
	_ func(*receive.Graph, plumbing.Hash, plumbing.Hash) ([]receive.Change, error) = (*receive.Graph).Diff
	_ func(*receive.Graph, plumbing.Hash) (string, error)                          = (*receive.Graph).Listing
	_ func(*receive.Graph, plumbing.Hash, plumbing.Hash) (string, error)           = (*receive.Graph).ListingSince
	_ func(*receive.Graph, ...plumbing.Hash) (int, error)                          = (*receive.Graph).Retain
	_ func(*receive.Graph) int                                                     = (*receive.Graph).Len
)

// Decoding the tree back into the struct both sides share. structtree is where
// a client repository does pick up treevial/objects, since Build writes into
// one; a client that only ever reads leaves can use receive alone and not.
var (
	_ func(any, map[string][]byte) error                               = structtree.Apply
	_ func(any, structtree.Source, plumbing.Hash, plumbing.Hash) error = structtree.ApplySince
	_ settings.Settings
)
