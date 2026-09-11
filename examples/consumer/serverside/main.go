// Command serverside is a minimal treevial server living in a module of its
// own. It imports the server side, the object store and the struct walker, and
// never mentions the client package.
package main

import (
	"flag"
	"log"
	"net"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/holoplot/treevial"
	"github.com/holoplot/treevial/objects"
	"github.com/holoplot/treevial/server"
	"github.com/holoplot/treevial/structtree"

	"github.com/holoplot/treevial-consumer-example/settings"
)

// provider hands each client settings of its own, built when it connects and
// dropped when it leaves.
type provider struct {
	mu   sync.Mutex
	held map[string]*settings.Settings
}

// Prepare implements server.Provider.
func (p *provider) Prepare(clientID string) (*objects.Store, plumbing.Hash, error) {
	s := &settings.Settings{
		Owner:   settings.Owner{Name: clientID, Team: "field-ops"},
		Display: settings.Display{Brightness: 80, Rotation: 0},
		Seen:    timestamppb.New(time.Unix(1700000000, 0)),
	}

	store := objects.NewStore()

	root, err := structtree.Build(store, s)
	if err != nil {
		return nil, plumbing.ZeroHash, err
	}

	p.mu.Lock()
	p.held[clientID] = s
	p.mu.Unlock()

	log.Printf("prepared %s -> %s", treevial.RefFor(clientID), root)

	return store, root, nil
}

// Release implements server.Provider.
func (p *provider) Release(clientID string) {
	p.mu.Lock()
	delete(p.held, clientID)
	p.mu.Unlock()

	log.Printf("released %s", clientID)
}

func main() {
	addr := flag.String("listen", "127.0.0.1:9418", "address to listen on")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("listening on %s", lis.Addr())

	srv := server.New(&provider{held: map[string]*settings.Settings{}})

	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}
