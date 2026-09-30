// Command node is one member of a treevial cluster.
//
// Every node runs the same thing: a hashicorp/raft peer, a treevial server, and
// the state in between. What differs between them is one flag and who happens
// to be the leader.
//
// A node serves its clients whatever its role. Subscribing is a read, and
// because every member applies the same entries and hashes the result the same
// way, all of them hold byte-identical objects — so a client is served the same
// state wherever it connects, and no objects ever pass between the nodes while
// the cluster runs. Only changing the state needs the leader.
//
// Three of these form a cluster. Each is started with its own -id and the same
// -peers, and each bootstraps that configuration into its own empty log, so
// there is no order to start them in and nothing to join:
//
//	node -id a -raft 127.0.0.1:7001 -listen 127.0.0.1:9418 -peers a=127.0.0.1:7001,b=127.0.0.1:7002,c=127.0.0.1:7003
//	node -id b -raft 127.0.0.1:7002 -listen 127.0.0.1:9419 -peers <the same>
//	node -id c -raft 127.0.0.1:7003 -listen 127.0.0.1:9420 -peers <the same>
//
// The log and the snapshots are kept in memory, so a node that is restarted
// comes back with nothing and has to be brought up to date by its peers. That
// is deliberate: it is how running the demo exercises the snapshot path rather
// than only its tests.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zonque/treevial/server"

	"github.com/zonque/treevial-cluster-demo/member"
)

// Snapshots are taken often and little of the log is kept behind them, so that
// a node restarted during a demo is brought up by a snapshot rather than by a
// replay of everything that ever happened.
const (
	snapshotThreshold = 16
	snapshotInterval  = 10 * time.Second
	trailingLogs      = 8
)

func main() {
	id := flag.String("id", "", "name of this node, unique in the cluster (required)")
	raftAddr := flag.String("raft", "127.0.0.1:7001", "address this node's peers reach it on")
	listen := flag.String("listen", "127.0.0.1:9418", "address treevial clients connect to")
	peers := flag.String("peers", "", "every node in the cluster, as id=host:port,... (required)")
	interval := flag.Duration("interval", 5*time.Second, "how often the leader changes a field (0 to leave the state alone)")
	raftLog := flag.Bool("raftlog", false, "let raft report elections and unreachable peers itself")
	flag.Parse()

	switch {
	case *id == "":
		log.Fatal("a node needs a name: pass -id")
	case *peers == "":
		log.Fatal("a node needs to know its cluster: pass -peers")
	}

	if err := run(*id, *raftAddr, *listen, *peers, *interval, *raftLog); err != nil {
		log.Fatal(err)
	}
}

func run(id, raftAddr, listen, peers string, interval time.Duration, raftLog bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	out := log.New(os.Stderr, id+" ", log.Ltime)

	// The server is named, so a client can say which node pushed it what.
	srv, err := server.New(server.WithID(id), server.WithDeadPeerTimeout(10*time.Second))
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}

	m := member.New(srv, out)
	srv.Watch(m)

	node, err := start(id, raftAddr, peers, m, out, raftLog)
	if err != nil {
		return err
	}

	// Leadership is the only thing the demo does differently on one node
	// than on another.
	go lead(ctx, node, m, out)
	go m.Inject(ctx, node, interval)

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	out.Printf("serving treevial on %s, raft on %s", lis.Addr(), raftAddr)

	go func() {
		<-ctx.Done()

		out.Print("shutting down")
		srv.Stop()

		if err := node.Shutdown().Error(); err != nil {
			out.Printf("raft shutdown: %v", err)
		}
	}()

	if err := srv.Serve(lis); err != nil {
		return err
	}

	return nil
}

// start brings up this node's raft peer.
func start(id, addr, peers string, m *member.Member, out *log.Logger, raftLog bool) (*raft.Raft, error) {
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(id)
	config.SnapshotThreshold = snapshotThreshold
	config.SnapshotInterval = snapshotInterval
	config.TrailingLogs = trailingLogs

	// A node that cannot reach a peer says so several times a second, which
	// buries the lines this demo is about. What it does is printed here
	// instead — elections through LeaderCh, entries as they are applied —
	// so raft is quiet unless somebody asks for it.
	diagnostics := io.Discard
	if raftLog {
		diagnostics = os.Stderr
	}

	config.LogLevel = "WARN"
	config.LogOutput = diagnostics

	advertised, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", addr, err)
	}

	transport, err := raft.NewTCPTransport(addr, advertised, 3, 10*time.Second, diagnostics)
	if err != nil {
		return nil, fmt.Errorf("raft transport on %q: %w", addr, err)
	}

	logs, stable := raft.NewInmemStore(), raft.NewInmemStore()
	snapshots := raft.NewInmemSnapshotStore()

	cluster, err := configuration(peers)
	if err != nil {
		return nil, err
	}

	// Every node bootstraps the same configuration into its own empty log,
	// which is why they can be started in any order and none of them has
	// to be told to add the others.
	if err := raft.BootstrapCluster(config, logs, stable, snapshots, transport, cluster); err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}

	node, err := raft.NewRaft(config, m, logs, stable, snapshots, transport)
	if err != nil {
		return nil, fmt.Errorf("new raft: %w", err)
	}

	out.Printf("raft up with %d peer(s)", len(cluster.Servers))

	return node, nil
}

// configuration reads the cluster out of the -peers flag. The order is the
// order given, so that every node bootstraps the same bytes.
func configuration(peers string) (raft.Configuration, error) {
	var cluster raft.Configuration

	for _, peer := range strings.Split(peers, ",") {
		id, addr, named := strings.Cut(strings.TrimSpace(peer), "=")
		if !named || id == "" || addr == "" {
			return raft.Configuration{}, fmt.Errorf("peer %q is not id=host:port", peer)
		}

		cluster.Servers = append(cluster.Servers, raft.Server{
			Suffrage: raft.Voter,
			ID:       raft.ServerID(id),
			Address:  raft.ServerAddress(addr),
		})
	}

	return cluster, nil
}

// lead reads the state of this node in the cluster.
//
// Winning an election is the moment the database may need reading, and the only
// moment: a member that already holds state got it from the log or from a
// snapshot.
func lead(ctx context.Context, node *raft.Raft, m *member.Member, out *log.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case won, open := <-node.LeaderCh():
			if !open {
				return
			}

			if !won {
				out.Printf("no longer the leader; %s has it", node.Leader())

				continue
			}

			if err := m.LoadOnce(node); err != nil {
				out.Printf("load the database: %v", err)
			}
		}
	}
}
