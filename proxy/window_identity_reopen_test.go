package proxy

// A hosted proxy reopened within two hours of its device's close restores
// the window identities it had instead of minting new ones (P90SPEED.md §4.1,
// fix 4). The window identity snapshot used to expire ten minutes after the
// window's last change, so a sweep or a returning customer reopened on fresh
// mints, each one a registration the hosted mint admission could refuse.
//
// These tests drive connect's real window client generator through the
// proxy's window identity store, in fake time. The store's persistence is a
// map with the ttl model.SetProxyWindowIdentities and
// model.RefreshProxyWindowIdentities give the Redis key; the model's own test
// pins those ttls against Redis.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Window identity snapshots by proxy id, each restorable until its expiry, as
// Redis keeps the model's key.
type reopenTestPersistence struct {
	stateLock sync.Mutex
	snapshots map[server.Id]reopenTestSnapshot
}

type reopenTestSnapshot struct {
	identities []*model.ProxyWindowClientIdentity
	expiry     time.Time
}

func newReopenTestPersistence() *reopenTestPersistence {
	return &reopenTestPersistence{
		snapshots: map[server.Id]reopenTestSnapshot{},
	}
}

// As model.GetProxyWindowIdentities.
func (self *reopenTestPersistence) load(_ context.Context, proxyId server.Id) []*model.ProxyWindowClientIdentity {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	snapshot, ok := self.snapshots[proxyId]
	if !ok || !time.Now().Before(snapshot.expiry) {
		return nil
	}
	return snapshot.identities
}

// As model.SetProxyWindowIdentities.
func (self *reopenTestPersistence) store(_ context.Context, proxyId server.Id, identities []*model.ProxyWindowClientIdentity) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(identities) == 0 {
		delete(self.snapshots, proxyId)
		return
	}
	self.snapshots[proxyId] = reopenTestSnapshot{
		identities: identities,
		expiry:     time.Now().Add(model.ProxyWindowIdentityTtl()),
	}
}

// As model.RefreshProxyWindowIdentities.
func (self *reopenTestPersistence) refresh(_ context.Context, proxyId server.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	snapshot, ok := self.snapshots[proxyId]
	if !ok || !time.Now().Before(snapshot.expiry) {
		delete(self.snapshots, proxyId)
		return false
	}
	snapshot.expiry = time.Now().Add(model.ProxyWindowIdentityTtl())
	self.snapshots[proxyId] = snapshot
	return true
}

// The hosted device's local authority as the window generator sees it:
// counts registrations (mints) and retirements.
type reopenTestCredentials struct {
	mintCount   atomic.Int32
	retireCount atomic.Int32
}

func (self *reopenTestCredentials) AuthNetworkClient(_ context.Context, _ *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
	self.mintCount.Add(1)
	// the generator reads only the client id, unverified
	token := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"client_id": server.NewId().String(),
	})
	byClientJwt, err := token.SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		return nil, err
	}
	return &connect.AuthNetworkClientResult{ByClientJwt: byClientJwt}, nil
}

func (self *reopenTestCredentials) RemoveNetworkClient(_ context.Context, _ *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
	self.retireCount.Add(1)
	return &connect.RemoveNetworkClientResult{}, nil
}

// One hosted proxy, whose devices open and close in turn over the same
// persistence and authority.
type reopenTestProxy struct {
	t           *testing.T
	proxyId     server.Id
	persistence *reopenTestPersistence
	credentials *reopenTestCredentials
	strategy    *connect.ClientStrategy
}

func newReopenTestProxy(t *testing.T) *reopenTestProxy {
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.RequestTimeout = time.Second
	strategy := connect.NewClientStrategy(t.Context(), strategySettings)
	t.Cleanup(strategy.Close)
	return &reopenTestProxy{
		t:           t,
		proxyId:     server.NewId(),
		persistence: newReopenTestPersistence(),
		credentials: &reopenTestCredentials{},
		strategy:    strategy,
	}
}

// One device of the proxy: its window generator, with the device's window
// identity store as the proxy device builds it.
type reopenTestDevice struct {
	proxy     *reopenTestProxy
	ctx       context.Context
	cancel    context.CancelFunc
	store     *windowIdentityStore
	generator *connect.ApiMultiClientGenerator
	args      []*connect.MultiClientGeneratorClientArgs
}

// A device whose location is the best available providers.
func (self *reopenTestProxy) openDevice() *reopenTestDevice {
	return self.openDeviceAt([]*connect.ProviderSpec{{BestAvailable: true}}, nil)
}

// A device of a proxy whose location pins one provider, as a unique-exit
// proxy's does.
func (self *reopenTestProxy) openPinnedDevice(pinnedExitClientId connect.Id) *reopenTestDevice {
	pinned := server.Id(pinnedExitClientId)
	return self.openDeviceAt([]*connect.ProviderSpec{{ClientId: &pinnedExitClientId}}, &pinned)
}

func (self *reopenTestProxy) openDeviceAt(specs []*connect.ProviderSpec, pinnedExitClientId *server.Id) *reopenTestDevice {
	ctx, cancel := context.WithCancel(self.t.Context())
	store := &windowIdentityStore{
		ctx:                     self.t.Context(),
		proxyId:                 self.proxyId,
		restoreGate:             newWindowIdentityRestoreGate(false),
		pinnedExitClientId:      pinnedExitClientId,
		loadWindowIdentities:    self.persistence.load,
		storeWindowIdentities:   self.persistence.store,
		refreshWindowIdentities: self.persistence.refresh,
	}
	settings := connect.DefaultApiMultiClientGeneratorSettings()
	settings.ClientCredentials = self.credentials
	sourceClientId := connect.NewId()
	generator := connect.NewApiMultiClientGenerator(
		ctx,
		specs,
		self.strategy,
		nil,
		"https://api.reopen.example",
		"synthetic-proxy-token",
		"wss://platform.reopen.example",
		"resident proxy",
		"resident proxy",
		"test",
		&sourceClientId,
		connect.DefaultClientSettings,
		settings,
	)
	generator.SetIdentityStore(store)
	return &reopenTestDevice{
		proxy:     self,
		ctx:       ctx,
		cancel:    cancel,
		store:     store,
		generator: generator,
	}
}

// The window's candidates for the destinations, as its expansion asks for
// them: a restored identity where the device has one, else a mint.
func (self *reopenTestDevice) dial(destinations []connect.MultiHopId) []*connect.MultiClientGeneratorClientArgs {
	t := self.proxy.t
	t.Helper()
	dialed := []*connect.MultiClientGeneratorClientArgs{}
	for _, destination := range destinations {
		args, err := self.generator.NewClientArgsForDestinationContext(self.ctx, destination)
		if err != nil {
			t.Fatal(err)
		}
		dialed = append(dialed, args)
	}
	// the snapshot write is asynchronous
	synctest.Wait()
	self.args = append(self.args, dialed...)
	return dialed
}

// The window gives up a client it built on the candidate while the device
// runs, as it does for one the platform refused or whose exit did not answer.
// A client the window built may have reached its exit, so its identity is
// retired rather than parked for another candidate.
func (self *reopenTestDevice) giveUp(args *connect.MultiClientGeneratorClientArgs) {
	client := connect.NewClient(self.proxy.t.Context(), args.ClientId, connect.NewNoContractClientOob(), connect.DefaultClientSettings())
	self.generator.RemoveClientWithArgs(client, args)
	client.Cancel()
	synctest.Wait()
}

// Closes the device as the proxy device does: the DeviceLocal's context ends
// before its window hands every candidate back, then the device's close
// restarts the snapshot's ttl.
func (self *reopenTestDevice) close() {
	t := self.proxy.t
	t.Helper()
	self.cancel()
	for _, args := range self.args {
		self.generator.RemoveClientArgs(args)
	}
	if err := self.generator.CloseAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	self.store.deviceClosed()
}

func reopenTestDestinations(count int) []connect.MultiHopId {
	destinations := []connect.MultiHopId{}
	for range count {
		destinations = append(destinations, connect.RequireMultiHopId(connect.NewId()))
	}
	return destinations
}

func reopenTestClientIds(args []*connect.MultiClientGeneratorClientArgs) map[connect.Id]bool {
	clientIds := map[connect.Id]bool{}
	for _, a := range args {
		clientIds[a.ClientId] = true
	}
	return clientIds
}

// A proxy reopened 1h59m after its device closed gets the window clients it
// had, with no mint; one reopened two hours after its last device closed
// mints again, as the snapshot has expired.
func TestReopenWithinTwoHoursRestoresWindowClientsWithoutMinting(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	connect.GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		proxy := newReopenTestProxy(t)
		// a hosted window's quality and speed exits
		destinations := reopenTestDestinations(6)

		first := proxy.openDevice()
		firstArgs := first.dial(destinations)
		if mints := proxy.credentials.mintCount.Load(); mints != 6 {
			t.Fatalf("the first device minted %d window clients, want 6", mints)
		}
		first.close()
		if retires := proxy.credentials.retireCount.Load(); retires != 0 {
			t.Fatalf("closing the device retired %d window clients, want 0: they stay registered for the reopen", retires)
		}

		time.Sleep(time.Hour + 59*time.Minute)

		second := proxy.openDevice()
		secondArgs := second.dial(destinations)
		if mints := proxy.credentials.mintCount.Load() - 6; mints != 0 {
			t.Fatalf("the reopen 1h59m after the close minted %d window clients, want 0", mints)
		}
		firstClientIds := reopenTestClientIds(firstArgs)
		for i, args := range secondArgs {
			if !firstClientIds[args.ClientId] {
				t.Fatalf("the reopen's window client %d for %s is not the one the closed device had", i, destinations[i])
			}
		}
		second.close()

		// the second device's close restarts the two hours; past them the
		// snapshot is gone and the window mints
		time.Sleep(2 * time.Hour)

		third := proxy.openDevice()
		thirdArgs := third.dial(destinations)
		if mints := proxy.credentials.mintCount.Load() - 6; mints != 6 {
			t.Fatalf("the reopen two hours after the close minted %d window clients, want 6", mints)
		}
		for i, args := range thirdArgs {
			if firstClientIds[args.ClientId] {
				t.Fatalf("the reopen two hours after the close reused window client %d of an expired snapshot", i)
			}
		}
		third.close()
	})
}

// The two hours count from the device's close, not from its window's last
// change: a device whose window last changed 1h50m before it closed still
// reopens on its window clients 20 minutes after the close.
func TestDeviceCloseRestartsWindowIdentityTtl(t *testing.T) {
	connect.GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		proxy := newReopenTestProxy(t)
		destinations := reopenTestDestinations(2)

		first := proxy.openDevice()
		firstArgs := first.dial(destinations)
		// a device that carries traffic for 1h50m on the same exits
		time.Sleep(time.Hour + 50*time.Minute)
		first.close()

		time.Sleep(20 * time.Minute)

		second := proxy.openDevice()
		secondArgs := second.dial(destinations)
		if mints := proxy.credentials.mintCount.Load() - 2; mints != 0 {
			t.Fatalf("the reopen 20 minutes after the close minted %d window clients, want 0", mints)
		}
		firstClientIds := reopenTestClientIds(firstArgs)
		for i, args := range secondArgs {
			if !firstClientIds[args.ClientId] {
				t.Fatalf("the reopen's window client %d is not the one the closed device had", i)
			}
		}
		second.close()
	})
}

// A window client the device's window gave up before the device closed, as it
// does for one the platform refused or whose exit stopped answering, is
// retired and leaves the snapshot: the reopen mints for its exit and restores
// the others. A restored client the window gives up in turn is retired the
// same way, and its exit is minted anew.
func TestReopenMintsForWindowClientsTheWindowGaveUp(t *testing.T) {
	connect.GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		proxy := newReopenTestProxy(t)
		destinations := reopenTestDestinations(3)

		first := proxy.openDevice()
		firstArgs := first.dial(destinations)
		first.giveUp(firstArgs[0])
		first.args = first.args[1:]
		if retires := proxy.credentials.retireCount.Load(); retires != 1 {
			t.Fatalf("the given up window client was retired %d times, want 1", retires)
		}
		first.close()

		time.Sleep(30 * time.Minute)

		second := proxy.openDevice()
		secondArgs := second.dial(destinations)
		if mints := proxy.credentials.mintCount.Load() - 3; mints != 1 {
			t.Fatalf("the reopen minted %d window clients, want 1 for the exit whose client was retired", mints)
		}
		if secondArgs[0].ClientId == firstArgs[0].ClientId {
			t.Fatal("the reopen restored a window client that was retired")
		}
		for i := 1; i < len(destinations); i++ {
			if secondArgs[i].ClientId != firstArgs[i].ClientId {
				t.Fatalf("the reopen did not restore window client %d", i)
			}
		}

		// the platform refuses a restored client (removed or deactivated
		// since): the window gives it up, it is retired, and its exit mints
		second.giveUp(secondArgs[1])
		second.args = []*connect.MultiClientGeneratorClientArgs{secondArgs[0], secondArgs[2]}
		if retires := proxy.credentials.retireCount.Load(); retires != 2 {
			t.Fatalf("the refused restored window client was retired %d times in all, want 2", retires)
		}
		replacement := second.dial(destinations[1:2])[0]
		if mints := proxy.credentials.mintCount.Load() - 3; mints != 2 {
			t.Fatalf("the refused restored window client's exit was not minted anew: %d mints since the first device, want 2", mints)
		}
		if replacement.ClientId == secondArgs[1].ClientId {
			t.Fatal("the window reused a refused restored window client")
		}
		second.close()
	})
}

// The proxy device's close restarts the snapshot's ttl once, after its
// DeviceLocal has closed (the window's last snapshot write comes before), and
// not while the deployment gate holds restoration or once the device manager
// is shutting down.
func TestProxyDeviceCloseRefreshesWindowIdentitiesAfterDeviceLocal(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		gateHeld      bool
		ownerStopped  bool
		wantRefreshes int
	}{
		{name: "open gate", wantRefreshes: 1},
		{name: "held gate", gateHeld: true},
		{name: "owner shutting down", ownerStopped: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ownerCtx, ownerCancel := context.WithCancel(t.Context())
			defer ownerCancel()
			deviceLocalClosed := false
			refreshes := 0
			proxyId := server.NewId()
			store := &windowIdentityStore{
				ctx:         ownerCtx,
				proxyId:     proxyId,
				restoreGate: newWindowIdentityRestoreGate(testCase.gateHeld),
				refreshWindowIdentities: func(_ context.Context, refreshedProxyId server.Id) bool {
					if !deviceLocalClosed {
						t.Error("the snapshot's ttl was restarted before the DeviceLocal closed")
					}
					if refreshedProxyId != proxyId {
						t.Errorf("refreshed proxy %s, want %s", refreshedProxyId, proxyId)
					}
					refreshes += 1
					return true
				},
			}
			if testCase.ownerStopped {
				ownerCancel()
			}
			ctx, cancel := context.WithCancel(ownerCtx)
			device := &ProxyDevice{
				ctx:                 ctx,
				cancel:              cancel,
				windowIdentityStore: store,
				closeDeviceLocalForTest: func() {
					deviceLocalClosed = true
				},
			}
			if err := device.Close(); err != nil {
				t.Fatal(err)
			}
			// one completion for repeated calls
			if err := device.Close(); err != nil {
				t.Fatal(err)
			}
			if refreshes != testCase.wantRefreshes {
				t.Fatalf("the close restarted the snapshot's ttl %d times, want %d", refreshes, testCase.wantRefreshes)
			}
		})
	}
}

// A unique exit replaced while its proxy has no device (a sweep, or the
// owner's rotate) repins the proxy's stored location, and the snapshot still
// names the exit it replaced. The reopened device dials only its current
// exit: the replaced exit is never listed, and the new one mints. The next
// reopen on the same exit restores it.
func TestReopenAfterRepinDialsOnlyTheCurrentExit(t *testing.T) {
	connect.GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		proxy := newReopenTestProxy(t)
		replacedExit := connect.NewId()
		currentExit := connect.NewId()

		first := proxy.openPinnedDevice(replacedExit)
		first.dial([]connect.MultiHopId{connect.RequireMultiHopId(replacedExit)})
		first.close()

		time.Sleep(10 * time.Minute)

		second := proxy.openPinnedDevice(currentExit)
		destinations, err := second.generator.NextDestinationsContext(second.ctx, 4, nil, "quality")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := destinations[connect.RequireMultiHopId(replacedExit)]; ok {
			t.Fatal("the reopened device listed the exit its proxy's unique exit replaced")
		}
		if _, ok := destinations[connect.RequireMultiHopId(currentExit)]; !ok || len(destinations) != 1 {
			t.Fatalf("the reopened device listed %d destinations, want only its current exit", len(destinations))
		}
		secondArgs := second.dial([]connect.MultiHopId{connect.RequireMultiHopId(currentExit)})
		if mints := proxy.credentials.mintCount.Load(); mints != 2 {
			t.Fatalf("%d window clients minted, want 2: one for each exit", mints)
		}
		second.close()

		time.Sleep(10 * time.Minute)

		third := proxy.openPinnedDevice(currentExit)
		thirdArgs := third.dial([]connect.MultiHopId{connect.RequireMultiHopId(currentExit)})
		if mints := proxy.credentials.mintCount.Load(); mints != 2 {
			t.Fatalf("the reopen on the same exit minted, %d window clients in all, want 2", mints)
		}
		if thirdArgs[0].ClientId != secondArgs[0].ClientId {
			t.Fatal("the reopen on the same exit did not restore its window client")
		}
		third.close()
	})
}

// The pinned exit is read from the device config's location: a location that
// names a provider pins it, any other location (a country, a group, the best
// available) pins nothing.
func TestWindowIdentityPinnedExitFromDeviceConfig(t *testing.T) {
	providerClientId := sdk.NewId()
	pinnedConfig := &model.ProxyDeviceConfig{
		InitialDeviceState: &model.ProxyDeviceState{
			Location: &sdk.ConnectLocation{
				ConnectLocationId: &sdk.ConnectLocationId{ClientId: providerClientId},
			},
		},
	}
	pinned := windowIdentityPinnedExitClientId(pinnedConfig)
	if pinned == nil || *pinned != server.RequireIdFromBytes(providerClientId.Bytes()) {
		t.Fatalf("a location naming a provider pinned %v, want the provider", pinned)
	}
	for name, config := range map[string]*model.ProxyDeviceConfig{
		"no state":    {},
		"no location": {InitialDeviceState: &model.ProxyDeviceState{}},
		"country": {InitialDeviceState: &model.ProxyDeviceState{Location: &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{LocationId: sdk.NewId()},
		}}},
		"best available": {InitialDeviceState: &model.ProxyDeviceState{Location: &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
		}}},
	} {
		if pinned := windowIdentityPinnedExitClientId(config); pinned != nil {
			t.Fatalf("%s: pinned %s, want none", name, pinned)
		}
	}
}
