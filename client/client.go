// Package client is the receiving side of treevial. It dials the server and
// keeps one stream open for the lifetime of the process, but never asks for
// anything: it identifies itself in the request header and then simply
// interprets whatever the server pushes, on the fly, without writing a byte to
// disk. The ref it is served follows from its own ID.
//
// A client repository depends on this package and on
// [github.com/holoplot/treevial/receive]; it does not need the server side at
// all.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/holoplot/treevial"
	"github.com/holoplot/treevial/internal/treevialpb"
	"github.com/holoplot/treevial/receive"
)

// Update reports one completed push: the ref that moved, the object it now
// points at, what it pointed at before, how many objects the server had to
// send, and the accumulated graph the client has interpreted so far.
type Update struct {
	// ClientID this client identified itself with.
	ClientID string
	// Ref the update arrived for, which follows from ClientID.
	Ref  string
	Hash plumbing.Hash
	// Previous is the hash this subscription was at before the update, or
	// the zero hash for the first one. Graph.Diff(Previous, Hash) is what
	// turns an update into a list of changed paths.
	Previous    plumbing.Hash
	ObjectCount uint32
	Graph       *receive.Graph
}

// Client is a connection to a treevial server.
type Client struct {
	conn *grpc.ClientConn

	mu  sync.Mutex
	err error
}

// Dial connects to a treevial server. The connection is built to outlive any
// amount of idleness: the server needs it available to push at a moment of its
// own choosing, so nothing here may tear it down.
func Dial(ctx context.Context, addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			// Ping often enough to keep NATs and middleboxes from
			// forgetting the connection. The server never
			// penalises a client for pinging, at any rate.
			Time: 30 * time.Second,
			// Never give up on a ping: a missing reply must not be
			// grounds for closing the transport.
			Timeout:             forever,
			PermitWithoutStream: true,
		}),
		// Never park the connection for being unused.
		grpc.WithIdleTimeout(0),
	)
	if err != nil {
		return nil, err
	}

	return &Client{conn: conn}, nil
}

// Close hangs up.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Err returns the error that ended the update stream, if any.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

func (c *Client) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err == nil {
		c.err = err
	}
}

// Subscribe registers under clientID as a client holding nothing, and returns a
// channel of updates the server pushes. The ID decides which ref the server
// serves, and the server prepares that data as the client connects.
func (c *Client) Subscribe(ctx context.Context, clientID string) (<-chan Update, error) {
	return c.Resume(ctx, clientID, plumbing.ZeroHash, receive.NewGraph())
}

// Resume registers under clientID declaring that the client already holds the
// tree at synced, whose objects are in graph. That single hash is the whole of
// the client's state: the server sends only the difference between it and the
// ref. Later updates accumulate into the same graph.
func (c *Client) Resume(
	ctx context.Context,
	clientID string,
	synced plumbing.Hash,
	graph *receive.Graph,
) (<-chan Update, error) {
	// Checked here as well as on the server, so an unusable ID fails
	// without a round trip; the rule itself lives in one place. The code
	// matches what the server would answer, so a caller sees one error
	// either way.
	if err := treevial.ValidateID(clientID); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// The ID travels in the request header, where it identifies the stream
	// for its whole life.
	stream, err := treevialpb.NewObjectSyncClient(c.conn).Sync(
		metadata.AppendToOutgoingContext(ctx, treevial.IDHeader, clientID),
	)
	if err != nil {
		return nil, err
	}

	syncedHex := ""
	if !synced.IsZero() {
		syncedHex = synced.String()
	}

	register := &treevialpb.ClientMsg{Body: &treevialpb.ClientMsg_Register{Register: &treevialpb.Register{
		Synced: syncedHex,
	}}}
	if err := stream.Send(register); err != nil {
		return nil, err
	}

	updates := make(chan Update)

	go func() {
		defer close(updates)

		if err := c.consume(ctx, stream, clientID, synced, graph, updates); err != nil {
			c.setErr(err)
		}
	}()

	return updates, nil
}

// consume reads the server's half of the stream for as long as it lasts. Pack
// chunks are handed to the interpreter as they arrive; only when an update is
// complete is it acknowledged and reported.
func (c *Client) consume(
	ctx context.Context,
	stream treevialpb.ObjectSync_SyncClient,
	clientID string,
	synced plumbing.Hash,
	graph *receive.Graph,
	updates chan<- Update,
) error {
	var (
		current  *inflight
		previous = synced
	)

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		switch body := msg.GetBody().(type) {
		case *treevialpb.ServerMsg_Begin:
			if current != nil {
				return fmt.Errorf("server began an update while %s was still open", current.hash)
			}
			current = beginUpdate(body.Begin, graph)

		case *treevialpb.ServerMsg_Chunk:
			if current == nil {
				return errors.New("server sent pack data outside an update")
			}
			if err := current.write(body.Chunk.GetData()); err != nil {
				return err
			}

		case *treevialpb.ServerMsg_End:
			if current == nil {
				return errors.New("server ended an update that never began")
			}

			if err := current.finish(); err != nil {
				return err
			}

			ack := &treevialpb.ClientMsg{Body: &treevialpb.ClientMsg_Ack{Ack: &treevialpb.Ack{
				Hash: current.hash.String(),
			}}}
			if err := stream.Send(ack); err != nil {
				return err
			}

			select {
			case updates <- Update{
				ClientID:    clientID,
				Ref:         treevial.RefFor(clientID),
				Hash:        current.hash,
				Previous:    previous,
				ObjectCount: current.objectCount,
				Graph:       graph,
			}:
			case <-ctx.Done():
				return ctx.Err()
			}

			previous = current.hash
			current = nil

		default:
			return fmt.Errorf("unexpected message %T", body)
		}
	}
}

// inflight is one update being interpreted. Chunks are written into a pipe that
// the interpreter reads concurrently, so objects are decoded during the
// transfer rather than after it.
type inflight struct {
	hash        plumbing.Hash
	objectCount uint32

	pw   *io.PipeWriter
	done chan error
}

func beginUpdate(begin *treevialpb.UpdateBegin, graph *receive.Graph) *inflight {
	u := &inflight{
		hash:        plumbing.NewHash(begin.GetHash()),
		objectCount: begin.GetObjectCount(),
	}

	// An update that carries no objects has no pack to interpret.
	if u.objectCount == 0 {
		return u
	}

	pr, pw := io.Pipe()
	u.pw = pw
	u.done = make(chan error, 1)

	go func() { u.done <- receive.Interpret(pr, graph) }()

	return u
}

func (u *inflight) write(data []byte) error {
	if u.pw == nil {
		return errors.New("server sent pack data for an empty update")
	}

	_, err := u.pw.Write(data)

	return err
}

func (u *inflight) finish() error {
	if u.pw == nil {
		return nil
	}

	if err := u.pw.Close(); err != nil {
		return err
	}

	return <-u.done
}
