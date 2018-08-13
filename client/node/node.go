// Package node defines a backend for a sharding-enabled, Ethereum blockchain.
// It defines a struct which handles the lifecycle of services in the
// sharding system, providing a bridge to the main Ethereum blockchain,
// as well as instantiating peer-to-peer networking for shards.
package node

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/prysmaticlabs/prysm/client/attester"
	"github.com/prysmaticlabs/prysm/client/beacon"
	"github.com/prysmaticlabs/prysm/client/params"
	"github.com/prysmaticlabs/prysm/client/proposer"
	"github.com/prysmaticlabs/prysm/client/rpcclient"
	"github.com/prysmaticlabs/prysm/client/txpool"
	"github.com/prysmaticlabs/prysm/client/types"
	"github.com/prysmaticlabs/prysm/shared"
	"github.com/prysmaticlabs/prysm/shared/cmd"
	"github.com/prysmaticlabs/prysm/shared/database"
	"github.com/prysmaticlabs/prysm/shared/debug"
	"github.com/prysmaticlabs/prysm/shared/p2p"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

var log = logrus.WithField("prefix", "node")

const shardChainDBName = "shardchaindata"

// ShardEthereum is a service that is registered and started when geth is launched.
// it contains APIs and fields that handle the different components of the sharded
// Ethereum network.
type ShardEthereum struct {
	shardConfig *params.Config // Holds necessary information to configure shards.

	// Lifecycle and service stores.
	services *shared.ServiceRegistry
	lock     sync.RWMutex
	stop     chan struct{} // Channel to wait for termination notifications.
	db       *database.DB
}

// NewShardInstance creates a new sharding-enabled Ethereum instance. This is called in the main
// geth sharding entrypoint.
func NewShardInstance(ctx *cli.Context) (*ShardEthereum, error) {
	registry := shared.NewServiceRegistry()
	shardEthereum := &ShardEthereum{
		services: registry,
		stop:     make(chan struct{}),
	}

	// Configure shardConfig by loading the default.
	shardEthereum.shardConfig = params.DefaultConfig()

	if err := shardEthereum.startDB(ctx); err != nil {
		return nil, err
	}

	if err := shardEthereum.registerP2P(); err != nil {
		return nil, err
	}

	actorFlag := ctx.GlobalString(types.ActorFlag.Name)
	if err := shardEthereum.registerTXPool(actorFlag); err != nil {
		return nil, err
	}

	if err := shardEthereum.registerRPCClientService(ctx); err != nil {
		return nil, err
	}

	if err := shardEthereum.registerBeaconService(); err != nil {
		return nil, err
	}

	if err := shardEthereum.registerActorService(actorFlag); err != nil {
		return nil, err
	}

	return shardEthereum, nil
}

// Start the ShardEthereum service and kicks off the p2p and actor's main loop.
func (s *ShardEthereum) Start() {
	s.lock.Lock()

	log.Info("Starting sharding node")

	s.services.StartAll()

	stop := s.stop
	s.lock.Unlock()

	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigc)
		<-sigc
		log.Info("Got interrupt, shutting down...")
		go s.Close()
		for i := 10; i > 0; i-- {
			<-sigc
			if i > 1 {
				log.Info("Already shutting down, interrupt more to panic.", "times", i-1)
			}
		}
		debug.Exit() // Ensure trace and CPU profile data are flushed.
		panic("Panic closing the sharding node")
	}()

	// Wait for stop channel to be closed.
	<-stop
}

// Close handles graceful shutdown of the system.
func (s *ShardEthereum) Close() {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.db.Close()
	s.services.StopAll()
	log.Info("Stopping sharding node")

	close(s.stop)
}

// startDB attaches a LevelDB wrapped object to the shardEthereum instance.
func (s *ShardEthereum) startDB(ctx *cli.Context) error {
	path := ctx.GlobalString(cmd.DataDirFlag.Name)
	config := &database.DBConfig{DataDir: path, Name: shardChainDBName, InMemory: false}
	db, err := database.NewDB(config)
	if err != nil {
		return err
	}

	s.db = db
	return nil
}

// registerP2P attaches a p2p server to the ShardEthereum instance.
func (s *ShardEthereum) registerP2P() error {
	shardp2p, err := p2p.NewServer()
	if err != nil {
		return fmt.Errorf("could not register shardp2p service: %v", err)
	}
	return s.services.RegisterService(shardp2p)
}

// registerTXPool creates a service that
// can spin up a transaction pool that will relay incoming transactions via an
// event feed. For our first releases, this can just relay test/fake transaction data
// the proposer can serialize into collation blobs.
// TODO: design this txpool system for our first release.
func (s *ShardEthereum) registerTXPool(actor string) error {
	if actor != "proposer" {
		return nil
	}
	var shardp2p *p2p.Server
	if err := s.services.FetchService(&shardp2p); err != nil {
		return err
	}
	pool, err := txpool.NewTXPool(shardp2p)
	if err != nil {
		return fmt.Errorf("could not register shard txpool service: %v", err)
	}
	return s.services.RegisterService(pool)
}

// registerBeaconService registers a service that fetches streams from a beacon node
// via RPC.
func (s *ShardEthereum) registerBeaconService() error {
	var rpcService *rpcclient.Service
	if err := s.services.FetchService(&rpcService); err != nil {
		return err
	}
	b := beacon.NewBeaconClient(context.TODO(), beacon.DefaultConfig(), rpcService)
	return s.services.RegisterService(b)
}

// registerActorService registers the actor according to CLI flags. Either attester/proposer.
func (s *ShardEthereum) registerActorService(actor string) error {
	var beaconService *beacon.Service
	if err := s.services.FetchService(&beaconService); err != nil {
		return err
	}

	switch actor {
	case "attester":
		att := attester.NewAttester(context.TODO(), beaconService)
		return s.services.RegisterService(att)
	case "proposer":
		prop := proposer.NewProposer(context.TODO(), beaconService)
		return s.services.RegisterService(prop)
	}
	return nil
}

// registerRPCClientService registers a new RPC client that connects to a beacon node.
func (s *ShardEthereum) registerRPCClientService(ctx *cli.Context) error {
	endpoint := ctx.GlobalString(types.BeaconRPCProviderFlag.Name)
	rpcService := rpcclient.NewRPCClient(context.TODO(), &rpcclient.Config{
		Endpoint: endpoint,
	})
	return s.services.RegisterService(rpcService)
}