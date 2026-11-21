//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's clients (world_test.go): M networks of N full sidecars each, both WorldSettings', every sidecar a
// sidecar.Sidecar enrolled through server's api from an auth key its network's owner made and Online on server's
// exchange (sidecart3's T3Network), its service origins at the world's blocks. Each network's first sidecar
// bootstraps it through api, signing its Genesis and base 1, and admits the others, and every sidecar then follows
// the network's log with its own key client, so each holds the verified state a scenario's statements build on.

package sidecare2e

import (
	"context"
	"fmt"
	"slices"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server/sidecart3"
)

// One network of the world: its T3Network and its sidecars, the first its owner's bootstrapping device.
type WorldNetwork struct {
	*sidecart3.T3Network
	// Its sidecars in enrollment order.
	Clients []*sidecart3.T3Device
}

// The device that bootstrapped the network and admits the rest.
func (self *WorldNetwork) Owner() *sidecart3.T3Device {
	return self.Clients[0]
}

// Adds a network named name with clientCount sidecars, the first bootstrapping it and admitting the others, each
// following the log once this returns. settings, called for each sidecar, adjusts its settings past the defaults;
// nil for none.
func (self *World) AddNetwork(ctx context.Context, name string, clientCount int, settings func(name string, sidecarSettings *sidecar.SidecarSettings)) (*WorldNetwork, error) {
	if clientCount < 1 {
		return nil, fmt.Errorf("a network of %d sidecars has no owner's device", clientCount)
	}
	network, err := self.t3.AddNetwork(ctx, name)
	if err != nil {
		return nil, err
	}
	clients, err := network.AddSidecars(ctx, clientCount, settings)
	if err != nil {
		return nil, fmt.Errorf("%s's sidecars: %w", name, err)
	}
	if err := network.Bootstrap(ctx, clients[0]); err != nil {
		return nil, err
	}
	if 1 < len(clients) {
		if err := network.Admit(ctx, clients[1:]); err != nil {
			return nil, err
		}
	}
	added := &WorldNetwork{T3Network: network, Clients: clients}
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.networks = append(self.networks, added)
		return true
	}()
	if !kept {
		return nil, errWorldClosed
	}
	return added, nil
}

// WorldSettings' M networks of N sidecars each, named a, b, c and on, one after another: what a scenario's clients
// are. settings as AddNetwork's.
func (self *World) AddNetworks(ctx context.Context, settings func(name string, sidecarSettings *sidecar.SidecarSettings)) ([]*WorldNetwork, error) {
	networks := []*WorldNetwork{}
	for index := range self.settings.NetworkCount {
		network, err := self.AddNetwork(ctx, worldNetworkName(index), self.settings.ClientsPerNetwork, settings)
		if err != nil {
			return nil, err
		}
		networks = append(networks, network)
	}
	return networks, nil
}

// The index-th network's name: a, b, ... z, then aa, ab and on.
func worldNetworkName(index int) string {
	name := ""
	for {
		name = string(rune('a'+index%26)) + name
		index = index/26 - 1
		if index < 0 {
			return name
		}
	}
}

// Every network added so far, in order.
func (self *World) Networks() []*WorldNetwork {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.networks)
}

// A general repo of the network's, created through evs by its owner's device, as `ur evs create` does (§11.12), with
// its member granted role: every sidecar of the network's to read or write it once this returns.
func (self *WorldNetwork) CreateSharedRepo(ctx context.Context, role protocol.RepoGrantRole) (sidecar.Id, error) {
	repo, err := self.CreateRepo(ctx, self.Owner(), protocol.RepoKind_REPO_KIND_GENERAL)
	if err != nil {
		return sidecar.Id{}, err
	}
	if _, err := self.Grant(ctx, self.Owner(), repo.RepoId(), self.MemberId(), role); err != nil {
		return sidecar.Id{}, err
	}
	return repo.RepoId(), nil
}
