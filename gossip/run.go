// The operator's gossip service (connect/EXTENDER.md C6).
//
// One replica runs one libp2p node that everything else in the mesh dials: the
// members of every app and every activated extender. It is the only node that
// originates (D6), and it originates nothing of its own -- it drains the
// publish queue the activation, probe and drip writes fill (C1, C2, C3, C4)
// and puts each row on the topic.
//
// The shape is deliberately the opposite of an app's node. It listens with the
// websocket transport on its service port and dials nobody: there is no
// operator to reach, and an extender it dialled would add nothing, since every
// extender already dials here. So the peer target is zero and the only
// connections it holds are the ones brought to it.
//
// Behind nginx, which terminates wss at `gossip.<host>`, the listener speaks
// plain ws. The libp2p websocket transport owns the whole http port, so the
// service serves no status route of its own and its `services.yml` entry
// carries `status: no`.
//
// Identity is `gossip_identity_key_hex` from `extender.yml` and must outlive a
// redeploy: hello publishes the peer id derived from it (C7) and a member that
// finds another identity at the far end of the dial refuses the connection.
// Missing or unreadable configuration is a startup error naming the key, not a
// node that quietly runs under a key nobody can reach it at.

package gossip

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	ma "github.com/multiformats/go-multiaddr"

	"github.com/urnetwork/connect"
	connectgossip "github.com/urnetwork/connect/gossip"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

const (
	// Connection manager watermarks of the operator node. D1 sizes a member at
	// 8/16 and an extender at 16/32; the operator is neither, and those numbers
	// would have it trimming the very links it exists to serve. It is the hub
	// every member and extender dials, so it keeps as many as a single replica
	// can afford and lets the manager trim only far above that.
	connManagerLowWater  = 512
	connManagerHighWater = 1024
)

type RunOptions struct {
	Port int
}

func (self RunOptions) Validate() error {
	if self.Port < 1 || self.Port > 65_535 {
		return fmt.Errorf("gossip port %d is outside [1,65535]", self.Port)
	}
	return nil
}

// Everything the service needs from `extender.yml`, resolved once at startup
// so a fault is one error at the top rather than a node that comes up and then
// cannot do anything.
type operatorConfig struct {
	networkHost     string
	networkHosts    []string
	identityKeySeed []byte
	rootKeySet      *connect.ExtenderRootKeySet
}

// The configuration errors name the missing key, since the whole remedy is to
// put it in the vault resource.
//
// The root keys are required even though only the topic validator reads them:
// without them the node accepts nothing it hears and relays nothing, which is
// a mesh that silently carries no records rather than one that is down.
func newOperatorConfig(config *controller.ExtenderConfig) (*operatorConfig, error) {
	networkHost := strings.TrimSpace(config.NetworkHost)
	if networkHost == "" {
		return nil, errors.New("the extender network has no network_host")
	}
	identityKeySeed, err := config.GossipIdentityKeySeed()
	if err != nil {
		return nil, err
	}
	rootPublicKeyHexes := config.RootPublicKeys()
	if len(rootPublicKeyHexes) == 0 {
		return nil, errors.New("the extender network has no root_public_keys_hex")
	}
	rootKeySet, err := connect.NewExtenderRootKeySetFromHex(rootPublicKeyHexes...)
	if err != nil {
		return nil, err
	}
	// the primary host is always accepted, whether or not network_hosts
	// repeats it, exactly as the allowed host list is built (A5)
	networkHosts := []string{networkHost}
	for _, otherNetworkHost := range config.NetworkHosts {
		if otherNetworkHost = strings.TrimSpace(otherNetworkHost); otherNetworkHost == "" {
			continue
		}
		if !slices.Contains(networkHosts, otherNetworkHost) {
			networkHosts = append(networkHosts, otherNetworkHost)
		}
	}
	return &operatorConfig{
		networkHost:     networkHost,
		networkHosts:    networkHosts,
		identityKeySeed: identityKeySeed,
		rootKeySet:      rootKeySet,
	}, nil
}

// The directory the node validates against and fills. It is held in memory
// only: the operator's durable state is the three tables, and a directory
// loaded from disk would be a second opinion about them.
func newDirectory(ctx context.Context, config *operatorConfig) *connect.ExtenderDirectory {
	settings := connect.DefaultExtenderDirectorySettings()
	settings.NetworkHosts = config.networkHosts
	directory := connect.NewExtenderDirectory(ctx, settings)
	directory.SetRootKeys(config.rootKeySet)
	return directory
}

// The operator node: a websocket listener, no extender listener, no operator
// address and no peer target. The role is the member default because connect
// names only member and extender (D1) and the operator is neither; nothing
// here reads it, and the settings above are what actually shape the node.
func newNode(
	ctx context.Context,
	config *operatorConfig,
	directory *connect.ExtenderDirectory,
	listenAddrs []ma.Multiaddr,
) (*connectgossip.Node, error) {
	settings := connectgossip.DefaultNodeSettings(connectgossip.NodeRoleMember)
	settings.NetworkHost = config.networkHost
	settings.Directory = directory
	settings.IdentityKeySeed = config.identityKeySeed
	settings.ListenAddrs = listenAddrs
	settings.PeerTarget = 0
	settings.ConnManagerLowWater = connManagerLowWater
	settings.ConnManagerHighWater = connManagerHighWater
	return connectgossip.NewNode(ctx, settings)
}

// Run serves the production gossip module until ctx is canceled.
func Run(ctx context.Context, options RunOptions) error {
	return runWithDependencies(ctx, options, router.StartupReadiness, server.StartStatsPusher)
}

// The command and the tests share the startup, drain and publish wiring. Only
// the external readiness gate and the metrics transport are replaceable; the
// node itself is real in both, since a fake one would prove nothing about the
// mesh it is the whole point of.
func runWithDependencies(
	ctx context.Context,
	options RunOptions,
	readiness func(context.Context) error,
	startStatsPusher func(context.Context) func(),
) error {
	if ctx == nil {
		return errors.New("gossip run context is nil")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	extenderConfig, err := controller.EnvExtenderConfig()
	if err != nil {
		return err
	}
	config, err := newOperatorConfig(extenderConfig)
	if err != nil {
		return err
	}
	listenIpv4, _, listenPort := server.RequireListenIpPort(options.Port)
	// plain ws: nginx terminates wss at gossip.<host> and reaches the
	// container over the services network
	listenAddrs, err := connectgossip.WebsocketListenAddrs(listenIpv4, listenPort, false)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	directory := newDirectory(runCtx, config)
	defer directory.Close()
	node, err := newNode(runCtx, config, directory, listenAddrs)
	if err != nil {
		return err
	}
	defer node.Close()

	flushStats := func() {}
	joinPublish := func() {}
	if err := readiness(runCtx); err != nil {
		// the mesh itself needs no database, so an unready replica still
		// carries what it hears; only the drip waits
		glog.Infof("[gossip]not ready (%s)\n", err)
	} else if runCtx.Err() == nil {
		flushStats = startStatsPusher(runCtx)
		publishDone := make(chan struct{})
		go server.HandleError(func() {
			defer close(publishDone)
			runPublish(runCtx, node, publishTimeout, publishBatchSize)
		}, cancel)
		joinPublish = func() { <-publishDone }
	}

	go server.HandleError(func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			if glog.V(1) {
				status := node.Status()
				glog.Infof(
					"[gossip]peers=%d mesh=%d goroutines=%d/%d\n",
					status.PeerCount,
					status.MeshPeerCount,
					runtime.NumGoroutine(),
					runtime.GOMAXPROCS(0),
				)
			}
		}
	}, cancel)

	glog.Infof(
		"[gossip]serving %s %s as %s on *:%d\n",
		server.RequireEnv(),
		server.RequireVersion(),
		node.PeerId(),
		options.Port,
	)
	select {
	case <-ctx.Done():
	case <-runCtx.Done():
	}
	// the drain publishes through the node, so it ends before the deferred
	// close takes the node out from under it
	cancel()
	joinPublish()
	flushStats()
	glog.Infof("[gossip]close\n")
	return nil
}
