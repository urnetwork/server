package sidecart3

// A T3 world's parts for networks that are not one owner's personal network, and for chaos at api and at a device
// (the end-to-end tests' control-plane case; §7.5, §7.9):
//
//	team networks   AddNetworkOf registers a network made outside the world, a team's by POST /network/team-create
//	                through api, with its first Owner's credential and member_id, funded and gated as AddNetwork's;
//	                AddSidecarsWith (world_networks.go) enrolls a member's own devices under that member's credential,
//	                or tagged devices under a tag owner's; and Follow has devices a test admitted by statements of
//	                its own trust the genesis and follow the log, as Admit's second half does
//	retired devices a device revoked, or whose member left, follows nothing more, so Retire takes it out of every Sync
//	device restart  T3Device.Restart ends a device's Sidecar and its instance, as a crash or a host restart ends a
//	                daemon, and starts a new one on the same store and key store (T3Sidecar.Restart), with new key and
//	                evs clients and a follower from its kept anchor
//	api restart     RestartApi drops api's process state, its sequencer with its caches, its gate cache and the
//	                installs over it (the exchange's scopes, approvals, the issuer's worker, rev/'s store), and builds
//	                them again on the same stores, as a restarted api process does; T3Api.SetApiHook lets a test hold,
//	                lose or answer a request around it. api's write budget is kept across the restart, one process id,
//	                so its report stays one row a ledger reads
//
// Safe for concurrent use, but a device's Restart must not run beside another use of that device.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
)

// A network made outside the world, which AddNetworkOf registers.
type T3NetworkOf struct {
	// What its sidecars' names start with.
	Name      string
	NetworkId server.Id
	// The user whose credential Root is: a team's first Owner.
	OwnerUserId server.Id
	// That owner's member_id: a team's first Owner's network_member row, which Genesis names as its owner member.
	OwnerMemberId sidecar.Id
	// "Bearer <jwt>", the owner's network credential for NetworkId, with its member_id: what makes the owner's auth
	// keys.
	Root string
}

// Registers a network made outside the world, a team's: in preview_networks and in every gated service's gate,
// funded in the Net's quota and in a transfer balance of server's as AddNetwork's, with no sidecar yet. Its owner's
// devices enroll under of's Root (AddSidecars), and its first one bootstraps it with of's OwnerMemberId as the
// Genesis's owner member.
func (self *T3World) AddNetworkOf(ctx context.Context, of *T3NetworkOf) (*T3Network, error) {
	if of.NetworkId == (server.Id{}) || of.OwnerMemberId == (sidecar.Id{}) || of.Root == "" {
		return nil, errors.New("a network the world registers needs its id, its owner's member_id and its owner's credential")
	}
	network := &T3Network{
		world:         self,
		Name:          of.Name,
		NetworkId:     of.NetworkId,
		OwnerUserId:   of.OwnerUserId,
		configRepoId:  sidecar.NewEvsRepoId(),
		root:          of.Root,
		ownerMemberId: of.OwnerMemberId,
	}
	network.heads = sidecartest.NewFakeConfigHeads(network.configRepoId)
	self.net.Fakes().Quota.SetBalance(connect.Id(network.NetworkId), self.settings.NetworkBalanceByteCount)
	now := server.NowUtc()
	if err := model.AddBasicTransferBalance(ctx, network.NetworkId, model.ByteCount(self.settings.NetworkBalanceByteCount), now, now.Add(self.settings.NetworkBalanceDuration)); err != nil {
		return nil, fmt.Errorf("%s's transfer balance: %w", of.Name, err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errors.New("the world has closed")
	}
	self.networks = append(self.networks, network)
	self.gateNetworksWithLock()
	return network, nil
}

// Has each device, which a test's own statements admitted (a member's first device admitted by an admin, a tagged one
// by its tag owner), trust the network's genesis and follow its log until it stands, then syncs at the owner's tip:
// what Admit does once its admissions are filed.
func (self *T3Network) Follow(ctx context.Context, devices []*T3Device) error {
	owner := self.Owner()
	genesis, err := self.genesisChain()
	if err != nil || owner == nil {
		return fmt.Errorf("%s's devices follow nothing before its bootstrap: %w", self.Name, err)
	}
	client := owner.Follower().Client()
	if client == nil {
		return fmt.Errorf("%s's follower no longer stands", owner.Name)
	}
	if err := client.CatchUp(ctx); err != nil {
		return err
	}
	tip := client.Pin().GetVersion()
	for _, device := range devices {
		if device.Network != self {
			return fmt.Errorf("%s is no sidecar of %s", device.Name, self.Name)
		}
		if err := device.follow(ctx, *genesis); err != nil {
			return err
		}
	}
	return self.Sync(ctx, tip)
}

// Takes device out of every Sync from now on: a revoked device, or one whose member left, follows nothing more. It
// keeps running until the world closes or the test closes it.
func (self *T3Network) Retire(device *T3Device) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.retired == nil {
		self.retired = map[*T3Device]bool{}
	}
	self.retired[device] = true
}

// Whether Retire took device out of the syncs.
func (self *T3Network) Retired(device *T3Device) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.retired[device]
}

// The devices Sync still waits on, in order.
func (self *T3Network) LiveDevices() []*T3Device {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.DeleteFunc(slices.Clone(self.devices), func(device *T3Device) bool { return self.retired[device] })
}

// Restarts the device keeping its store (T3Sidecar.Restart): its EvsClient, follower and key clients end with the old
// Sidecar, the new Sidecar comes Online from the kept enrollment, and the device takes new key clients and follows the
// log again from its kept anchor until it stands. A device that never followed comes back Online alone. Nothing else
// may use the device meanwhile.
func (self *T3Device) Restart(ctx context.Context) error {
	var evs *sidecar.EvsClient
	var follower *sidecar.SidecarConfigFollower
	var keys *t3WorldKeys
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		evs, follower, keys = self.evs, self.follower, self.keys
		self.evs, self.follower, self.keys = nil, nil, nil
	}()
	if evs != nil {
		evs.Close()
	}
	if follower != nil {
		follower.Close()
	}
	if keys != nil {
		keys.close()
	}
	if err := self.T3Sidecar.Restart(ctx); err != nil {
		return err
	}
	if err := self.buildKeys(); err != nil {
		return fmt.Errorf("%s's key clients after its restart: %w", self.Name, err)
	}
	if follower == nil {
		return nil
	}
	genesis, err := self.Network.genesisChain()
	if err != nil {
		return err
	}
	return self.follow(ctx, *genesis)
}

// api's process state: its gate cache and what its installs undo.
type t3WorldApiRun struct {
	gate *t3WorldGate
	pop  func()
}

// api's sequencer over its first cluster, a gate cache of its own, and what api's process installs over that cache:
// the exchange's scopes and installT3WorldApi's parts. Each run's pop undoes its installs, newest first, and ends its
// gate cache.
func (self *T3World) startApi() (*t3WorldApiRun, error) {
	gate, err := self.addGate("api")
	if err != nil {
		return nil, err
	}
	configLogSettings := model.NewApiConfigLogSettings()
	if self.settings.AdjustConfigLog != nil {
		self.settings.AdjustConfigLog(configLogSettings)
	}
	model.SetConfigLogSequencer(controller.NewHostedConfigLogSequencer(self.apiHost, self.apiFirst, configLogSettings))
	pops := []func(){func() { model.SetConfigLogSequencer(nil) }}
	pops = append(pops, oauth.SetExchangeScopeSource(oauth.NewProjectionExchangeScopeSource(gate.cache, &oauth.ExchangeScopeSettings{Clock: self.host.Clock})))
	pops = append(pops, installT3WorldApi(self.ctx, self.apiHost, self.apiFirst, gate.cache))
	return &t3WorldApiRun{
		gate: gate,
		pop: func() {
			for index := len(pops) - 1; 0 <= index; index -= 1 {
				pops[index]()
			}
			self.removeGate(gate)
			gate.close()
		},
	}, nil
}

// Ends api's current run, if one runs. Safe to call again.
func (self *T3World) stopApi() {
	run := func() *t3WorldApiRun {
		self.apiLock.Lock()
		defer self.apiLock.Unlock()
		run := self.apiRun
		self.apiRun = nil
		return run
	}()
	if run != nil {
		run.pop()
	}
}

// The chaos hook at api: drops api's process state, its sequencer with its caches, its gate cache and the installs
// over it, as a host restart drops a process, and builds them again on the same stores; between the two, api answers
// what a process with nothing installed answers. A test holds or loses the requests around it with T3Api.SetApiHook.
// api's write budget is kept (the file's header).
func (self *T3World) RestartApi() error {
	self.stopApi()
	run, err := self.startApi()
	if err != nil {
		return fmt.Errorf("api's restart: %w", err)
	}
	stale := func() *t3WorldApiRun {
		self.apiLock.Lock()
		defer self.apiLock.Unlock()
		stale := self.apiRun
		self.apiRun = run
		return stale
	}()
	if stale != nil {
		stale.pop()
	}
	return nil
}
