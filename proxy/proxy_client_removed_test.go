// Pins the client index of the device manager and the closing of a removed
// client's devices (CLIENTCLOSE1.md section 5).
package proxy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The keyspace channel of a client's removed flag, from the model's pattern.
func clientRemovedTestChannel(db int, clientId server.Id) string {
	return strings.Replace(model.ClientRemovedKeyEventPattern(db), "*", clientId.String(), 1)
}

// The key of a client's removed flag, from the model's pattern.
func clientRemovedTestKey(clientId server.Id) string {
	channel := clientRemovedTestChannel(0, clientId)
	return channel[strings.Index(channel, ":")+1:]
}

// A manager whose devices have no DeviceLocal or Tun. Each device's Run
// returns when the device is canceled, and its Close is observable.
type clientRemovedTestFixture struct {
	t       testing.TB
	manager *ProxyDeviceManager

	stateLock      sync.Mutex
	proxyClientIds map[server.Id]server.Id
	closed         map[*ProxyDevice]chan struct{}
}

func newClientRemovedTestFixture(t testing.TB, ctx context.Context, clientRemovedSettings *ClientRemovedSettings) *clientRemovedTestFixture {
	t.Helper()
	manager := NewProxyDeviceManager(ctx, &ProxyDeviceManagerSettings{
		CheckProxyDeviceIdleTimeout: time.Minute,
		NetworkSpace:                &sdk.NetworkSpace{},
		ClientRemoved:               clientRemovedSettings,
	})
	fixture := &clientRemovedTestFixture{
		t:              t,
		manager:        manager,
		proxyClientIds: map[server.Id]server.Id{},
		closed:         map[*ProxyDevice]chan struct{}{},
	}
	manager.proxyDeviceBuilder = fixture.build
	return fixture
}

func (self *clientRemovedTestFixture) close() {
	closeProxyDeviceManager(self.t, self.manager)
}

// Makes the proxy's device a device of the client.
func (self *clientRemovedTestFixture) setClient(proxyId server.Id, clientId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.proxyClientIds[proxyId] = clientId
}

func (self *clientRemovedTestFixture) newDevice(clientId server.Id) *ProxyDevice {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	deviceCtx, deviceCancel := context.WithCancel(self.manager.ctx)
	closed := make(chan struct{})
	pd := &ProxyDevice{
		ctx:         deviceCtx,
		cancel:      deviceCancel,
		clientId:    clientId,
		deviceState: newProxyDeviceReadinessTestSource(true),
		settings:    DefaultProxyDeviceSettings(),
		runForTest: func() {
			<-deviceCtx.Done()
		},
		closeDeviceLocalForTest: func() {
			close(closed)
		},
	}
	pd.UpdateActivity()
	self.closed[pd] = closed
	return pd
}

func (self *clientRemovedTestFixture) clientOf(proxyId server.Id) (server.Id, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clientId, ok := self.proxyClientIds[proxyId]
	return clientId, ok
}

// The manager's device builder.
func (self *clientRemovedTestFixture) build(proxyId server.Id) (*ProxyDevice, error) {
	clientId, ok := self.clientOf(proxyId)
	if !ok {
		return nil, fmt.Errorf("proxy %s has no client", proxyId)
	}
	return self.newDevice(clientId), nil
}

// Opens the proxy's device, a device of the client.
func (self *clientRemovedTestFixture) open(proxyId server.Id, clientId server.Id) *ProxyDevice {
	self.t.Helper()
	self.setClient(proxyId, clientId)
	pd, err := self.manager.OpenProxyDevice(proxyId)
	if err != nil {
		self.t.Fatal(err)
	}
	return pd
}

// Installs a device of the client as an open does, without its workers.
func (self *clientRemovedTestFixture) install(proxyId server.Id, clientId server.Id) *ProxyDevice {
	pd := self.newDevice(clientId)
	pdState := &proxyDeviceState{}
	self.manager.stateLock.Lock()
	defer self.manager.stateLock.Unlock()
	self.manager.proxyDevices[proxyId] = pdState
	pdState.StateLock.Lock()
	defer pdState.StateLock.Unlock()
	pdState.ProxyDevice = pd
	self.manager.addClientDevice(proxyId, pd)
	return pd
}

// Closed when the device's Close runs.
func (self *clientRemovedTestFixture) closedNotify(pd *ProxyDevice) chan struct{} {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closed[pd]
}

// Waits for the device's Close, which its worker runs after uninstalling it.
func (self *clientRemovedTestFixture) waitClosed(pds ...*ProxyDevice) {
	self.t.Helper()
	timeout := time.After(10 * time.Second)
	for _, pd := range pds {
		select {
		case <-self.closedNotify(pd):
		case <-timeout:
			self.t.Fatalf("device %p of client %s did not close", pd, pd.clientId)
		}
	}
}

// Waits for the device to be canceled. A device installed without its
// workers is never closed.
func (self *clientRemovedTestFixture) waitCanceled(pds ...*ProxyDevice) {
	self.t.Helper()
	timeout := time.After(10 * time.Second)
	for _, pd := range pds {
		select {
		case <-pd.Done():
		case <-timeout:
			self.t.Fatalf("device %p of client %s was not canceled", pd, pd.clientId)
		}
	}
}

func (self *clientRemovedTestFixture) requireLive(pds ...*ProxyDevice) {
	self.t.Helper()
	for _, pd := range pds {
		if !pd.Active() {
			self.t.Fatalf("device %p of client %s was closed", pd, pd.clientId)
		}
	}
}

type clientDeviceIndex = map[server.Id]map[server.Id]*ProxyDevice

func equalClientDevices(a clientDeviceIndex, b clientDeviceIndex) bool {
	return maps.EqualFunc(a, b, func(aDevices map[server.Id]*ProxyDevice, bDevices map[server.Id]*ProxyDevice) bool {
		return maps.Equal(aDevices, bDevices)
	})
}

func formatClientDevices(devices clientDeviceIndex) string {
	parts := []string{}
	for clientId, proxyDevices := range devices {
		for proxyId, pd := range proxyDevices {
			parts = append(parts, fmt.Sprintf("%s/%s=%p", clientId, proxyId, pd))
		}
	}
	slices.Sort(parts)
	return "{" + strings.Join(parts, " ") + "}"
}

// The manager's client index.
func (self *clientRemovedTestFixture) indexed() clientDeviceIndex {
	self.manager.clientLock.Lock()
	defer self.manager.clientLock.Unlock()
	indexed := clientDeviceIndex{}
	for clientId, proxyDevices := range self.manager.clientDevices {
		indexed[clientId] = maps.Clone(proxyDevices)
	}
	return indexed
}

// The index that the installed devices imply.
func (self *clientRemovedTestFixture) installed() clientDeviceIndex {
	self.manager.stateLock.RLock()
	defer self.manager.stateLock.RUnlock()
	installed := clientDeviceIndex{}
	for proxyId, pdState := range self.manager.proxyDevices {
		pd := func() *ProxyDevice {
			pdState.StateLock.Lock()
			defer pdState.StateLock.Unlock()
			return pdState.ProxyDevice
		}()
		if pd == nil {
			continue
		}
		if _, ok := installed[pd.clientId]; !ok {
			installed[pd.clientId] = map[server.Id]*ProxyDevice{}
		}
		installed[pd.clientId][proxyId] = pd
	}
	return installed
}

// Requires both the index and the installed devices to be `want`.
func (self *clientRemovedTestFixture) requireIndex(want clientDeviceIndex) {
	self.t.Helper()
	if indexed := self.indexed(); !equalClientDevices(indexed, want) {
		self.t.Fatalf("client index = %s, want %s", formatClientDevices(indexed), formatClientDevices(want))
	}
	if installed := self.installed(); !equalClientDevices(installed, want) {
		self.t.Fatalf("installed devices = %s, want %s", formatClientDevices(installed), formatClientDevices(want))
	}
}

// A key event subscription that a test drives in place of Redis.
type clientRemovedTestSubscription struct {
	messages     chan server.RedisMessage
	done         chan struct{}
	endOnce      sync.Once
	unsubscribed chan struct{}
	unsubOnce    sync.Once
}

func newClientRemovedTestSubscription() *clientRemovedTestSubscription {
	return &clientRemovedTestSubscription{
		messages:     make(chan server.RedisMessage, 1024),
		done:         make(chan struct{}),
		unsubscribed: make(chan struct{}),
	}
}

// Ends the subscription, as a lost connection or a topology change does.
func (self *clientRemovedTestSubscription) end() {
	self.endOnce.Do(func() {
		close(self.done)
	})
}

func (self *clientRemovedTestSubscription) unsub() {
	self.unsubOnce.Do(func() {
		close(self.unsubscribed)
	})
}

// Sends one keyspace event, unless the subscriber has unsubscribed.
func (self *clientRemovedTestSubscription) sendMessage(channel string, event string) {
	select {
	case self.messages <- &redis.Message{Channel: channel, Payload: event}:
	case <-self.unsubscribed:
	}
}

// Sends one keyspace event of the client's removed flag.
func (self *clientRemovedTestSubscription) send(clientId server.Id, event string) {
	self.sendMessage(clientRemovedTestChannel(0, clientId), event)
}

// Subscriptions offered to the subscriber, one per subscribe.
type clientRemovedTestSubscriptions chan *clientRemovedTestSubscription

// Offers the next subscription and waits for the subscriber to take it.
func (self clientRemovedTestSubscriptions) offer(t testing.TB) *clientRemovedTestSubscription {
	t.Helper()
	subscription := newClientRemovedTestSubscription()
	select {
	case self <- subscription:
	case <-time.After(10 * time.Second):
		t.Fatal("the subscriber did not subscribe")
	}
	return subscription
}

// Settings whose subscriptions the test offers and whose flag reads it
// answers.
func clientRemovedTestSettings(
	subscriptions clientRemovedTestSubscriptions,
	isClientRemoved func(context.Context, server.Id) bool,
) *ClientRemovedSettings {
	settings := DefaultClientRemovedSettings()
	settings.ResubscribeTimeout = time.Millisecond
	settings.CheckConcurrency = 2
	settings.subscribe = func(ctx context.Context, topologyRefresh time.Duration) (<-chan server.RedisMessage, <-chan struct{}, func(), error) {
		select {
		case subscription := <-subscriptions:
			return subscription.messages, subscription.done, subscription.unsub, nil
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		}
	}
	settings.isClientRemoved = isClientRemoved
	return settings
}

// Flag reads that a test answers, recording each read.
type clientRemovedTestFlags struct {
	stateLock   sync.Mutex
	removed     map[server.Id]bool
	reads       map[server.Id]int
	inFlight    int
	maxInFlight int
	readTime    time.Duration
}

func newClientRemovedTestFlags() *clientRemovedTestFlags {
	return &clientRemovedTestFlags{
		removed: map[server.Id]bool{},
		reads:   map[server.Id]int{},
	}
}

func (self *clientRemovedTestFlags) setRemoved(clientId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removed[clientId] = true
}

func (self *clientRemovedTestFlags) isClientRemoved(ctx context.Context, clientId server.Id) bool {
	readTime := func() time.Duration {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.inFlight += 1
		self.maxInFlight = max(self.maxInFlight, self.inFlight)
		return self.readTime
	}()
	if 0 < readTime {
		select {
		case <-ctx.Done():
		case <-time.After(readTime):
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.inFlight -= 1
	self.reads[clientId] += 1
	return self.removed[clientId]
}

func (self *clientRemovedTestFlags) readCounts() (map[server.Id]int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return maps.Clone(self.reads), self.maxInFlight
}

// The client index follows every install and uninstall: an open adds the
// device, a replacement swaps the proxy id's device in place, and an idle
// close and the manager's close remove them.
func TestProxyDeviceManagerClientIndexFollowsDevices(t *testing.T) {
	fixture := newClientRemovedTestFixture(t, t.Context(), nil)
	defer fixture.close()
	clientA, clientB := server.NewId(), server.NewId()
	proxyA1, proxyA2, proxyB1 := server.NewId(), server.NewId(), server.NewId()

	a1 := fixture.open(proxyA1, clientA)
	a2 := fixture.open(proxyA2, clientA)
	b1 := fixture.open(proxyB1, clientB)
	if reused := fixture.open(proxyA1, clientA); reused != a1 {
		t.Fatal("an open of a live device did not reuse it")
	}
	fixture.requireIndex(clientDeviceIndex{
		clientA: {proxyA1: a1, proxyA2: a2},
		clientB: {proxyB1: b1},
	})

	// a1's lifecycle ends; the next open replaces it under the same proxy id,
	// and a1's own teardown must not remove its replacement
	a1.deviceState.(*proxyDeviceReadinessTestSource).done.Store(true)
	a1Replacement := fixture.open(proxyA1, clientA)
	if a1Replacement == a1 {
		t.Fatal("an open reused a device whose lifecycle ended")
	}
	fixture.waitClosed(a1)
	fixture.requireIndex(clientDeviceIndex{
		clientA: {proxyA1: a1Replacement, proxyA2: a2},
		clientB: {proxyB1: b1},
	})

	// canceled as an idle device is
	b1.Cancel()
	fixture.waitClosed(b1)
	fixture.requireIndex(clientDeviceIndex{
		clientA: {proxyA1: a1Replacement, proxyA2: a2},
	})

	fixture.close()
	fixture.waitClosed(a1Replacement, a2)
	fixture.requireIndex(clientDeviceIndex{})
}

// A SET of a client's removed flag closes exactly that client's devices. The
// flag's other events (the SET's own ttl, its expiry, a delete) and other
// keys' events are not removals.
func TestClientRemovedEventClosesOnlyThatClientsDevices(t *testing.T) {
	subscriptions := make(clientRemovedTestSubscriptions)
	flags := newClientRemovedTestFlags()
	fixture := newClientRemovedTestFixture(t, t.Context(), clientRemovedTestSettings(subscriptions, flags.isClientRemoved))
	defer fixture.close()
	clientA, clientB, clientC := server.NewId(), server.NewId(), server.NewId()
	proxyC1 := server.NewId()
	a1 := fixture.open(server.NewId(), clientA)
	a2 := fixture.open(server.NewId(), clientA)
	b1 := fixture.open(server.NewId(), clientB)
	c1 := fixture.open(proxyC1, clientC)

	subscription := subscriptions.offer(t)
	for _, event := range []string{"expire", "expired", "del", "append"} {
		subscription.send(clientA, event)
	}
	subscription.sendMessage(strings.Replace(clientRemovedTestChannel(0, clientA), "c_removed", "c_close", 1), "set")
	subscription.sendMessage("__keyspace@0__:{not-an-id}c_removed", "set")
	// the subscriber dispatches in order, so once b1 is canceled every event
	// before its removal has been dispatched
	subscription.send(clientB, "set")
	fixture.waitClosed(b1)
	fixture.requireLive(a1, a2, c1)

	subscription.send(clientA, "set")
	fixture.waitClosed(a1, a2)
	fixture.requireLive(c1)
	fixture.requireIndex(clientDeviceIndex{
		clientC: {proxyC1: c1},
	})
}

// Events published while unsubscribed are gone. Each (re)subscribe reads the
// removed flag of every installed device's client, once per client and a
// bounded number at a time, and closes the devices of the removed ones.
func TestClientRemovedResubscribeChecksInstalledClients(t *testing.T) {
	subscriptions := make(clientRemovedTestSubscriptions)
	flags := newClientRemovedTestFlags()
	settings := clientRemovedTestSettings(subscriptions, flags.isClientRemoved)
	fixture := newClientRemovedTestFixture(t, t.Context(), settings)
	defer fixture.close()
	clientIds := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()}
	removedClientId := clientIds[0]
	removedDevices := []*ProxyDevice{
		fixture.install(server.NewId(), removedClientId),
		fixture.install(server.NewId(), removedClientId),
	}
	liveDevices := []*ProxyDevice{}
	for _, clientId := range clientIds[1:] {
		liveDevices = append(liveDevices, fixture.install(server.NewId(), clientId))
	}

	first := subscriptions.offer(t)
	first.end()
	select {
	case <-first.unsubscribed:
	case <-time.After(10 * time.Second):
		t.Fatal("the subscriber did not unsubscribe an ended subscription")
	}
	// set while unsubscribed: no event reaches the subscriber
	flags.setRemoved(removedClientId)
	fixture.requireLive(removedDevices...)

	func() {
		flags.stateLock.Lock()
		defer flags.stateLock.Unlock()
		// long enough for every read to overlap if nothing bounded them
		flags.readTime = 20 * time.Millisecond
		clear(flags.reads)
		flags.maxInFlight = 0
	}()
	subscriptions.offer(t)
	fixture.waitCanceled(removedDevices...)
	deadline := time.Now().Add(10 * time.Second)
	for {
		reads, _ := flags.readCounts()
		if len(reads) == len(clientIds) {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("the check read %d of %d clients", len(reads), len(clientIds))
		}
		time.Sleep(time.Millisecond)
	}
	reads, maxInFlight := flags.readCounts()
	for _, clientId := range clientIds {
		if reads[clientId] != 1 {
			t.Fatalf("the check read client %s %d times, want once", clientId, reads[clientId])
		}
	}
	if settings.CheckConcurrency < maxInFlight {
		t.Fatalf("the check had %d reads in flight, more than its bound of %d", maxInFlight, settings.CheckConcurrency)
	}
	fixture.requireLive(liveDevices...)
}

// A dial whose session was minted before the removal committed can install
// its device after the removal's event found nothing to close. An installed
// device reads its client's removed flag, so it closes too.
func TestClientRemovedDeviceInstalledAfterRemovalCloses(t *testing.T) {
	// never offered: there are no events and no (re)subscribe checks
	subscriptions := make(clientRemovedTestSubscriptions)
	flags := newClientRemovedTestFlags()
	fixture := newClientRemovedTestFixture(t, t.Context(), clientRemovedTestSettings(subscriptions, flags.isClientRemoved))
	defer fixture.close()
	removedClientId, liveClientId := server.NewId(), server.NewId()
	liveProxyId := server.NewId()
	flags.setRemoved(removedClientId)

	live := fixture.open(liveProxyId, liveClientId)
	removed := fixture.open(server.NewId(), removedClientId)
	fixture.waitClosed(removed)
	fixture.requireLive(live)
	fixture.requireIndex(clientDeviceIndex{
		liveClientId: {liveProxyId: live},
	})
}

// A failed subscribe backs off and subscribes again; the subscriber stops
// only with its manager.
func TestClientRemovedSubscribeErrorRetries(t *testing.T) {
	subscriptions := make(clientRemovedTestSubscriptions)
	flags := newClientRemovedTestFlags()
	settings := clientRemovedTestSettings(subscriptions, flags.isClientRemoved)
	subscribe := settings.subscribe
	var subscribeCount atomic.Int32
	settings.subscribe = func(ctx context.Context, topologyRefresh time.Duration) (<-chan server.RedisMessage, <-chan struct{}, func(), error) {
		if subscribeCount.Add(1) <= 2 {
			return nil, nil, nil, errors.New("synthetic subscribe failure")
		}
		return subscribe(ctx, topologyRefresh)
	}
	fixture := newClientRemovedTestFixture(t, t.Context(), settings)
	defer fixture.close()
	clientId := server.NewId()
	pd := fixture.open(server.NewId(), clientId)

	subscriptions.offer(t).send(clientId, "set")
	fixture.waitClosed(pd)
}

// A raise while subscribing (here a redis.yml without its db) returns as an
// error, so that the subscriber backs off and retries instead of ending for
// the life of the process.
func TestClientRemovedSubscribeRaiseIsAnError(t *testing.T) {
	popRedis := server.Vault.PushSimpleResource("redis.yml", []byte("cluster: false\n"))
	defer popRedis()
	messages, done, unsub, err := subscribeClientRemovedKeyEvents(t.Context(), time.Minute)
	if err == nil || messages != nil || done != nil || unsub != nil {
		t.Fatalf("a raised subscribe returned err=%v", err)
	}
}

// Removal events, (re)subscribe checks, install checks, opens, replacements
// and idle closes run at once. Under -race this pins their synchronization.
// After the churn the index again equals the installed devices, and the
// manager's close empties it.
func TestClientRemovedConcurrentEventsAndClosesRace(t *testing.T) {
	subscriptions := make(clientRemovedTestSubscriptions)
	flags := newClientRemovedTestFlags()
	settings := clientRemovedTestSettings(subscriptions, flags.isClientRemoved)
	settings.CheckConcurrency = 4
	fixture := newClientRemovedTestFixture(t, t.Context(), settings)
	defer fixture.close()

	const clientCount = 8
	const proxyCount = 32
	const iterationCount = 256
	clientIds := make([]server.Id, clientCount)
	for i := range clientIds {
		clientIds[i] = server.NewId()
	}
	proxyIds := make([]server.Id, proxyCount)
	for i := range proxyIds {
		proxyIds[i] = server.NewId()
		fixture.setClient(proxyIds[i], clientIds[i%clientCount])
	}
	installedDevice := func() *ProxyDevice {
		proxyId := proxyIds[rand.IntN(proxyCount)]
		fixture.manager.stateLock.RLock()
		defer fixture.manager.stateLock.RUnlock()
		pdState, ok := fixture.manager.proxyDevices[proxyId]
		if !ok {
			return nil
		}
		pdState.StateLock.Lock()
		defer pdState.StateLock.Unlock()
		return pdState.ProxyDevice
	}

	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range iterationCount {
				_, _ = fixture.manager.OpenProxyDevice(proxyIds[rand.IntN(proxyCount)])
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for i := range iterationCount {
			if pd := installedDevice(); pd != nil {
				if i%2 == 0 {
					// canceled as an idle device is
					pd.Cancel()
				} else {
					// its lifecycle ends; the next open replaces it
					pd.deviceState.(*proxyDeviceReadinessTestSource).done.Store(true)
				}
			}
		}
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		for i := range iterationCount {
			if i%64 == 0 {
				flags.setRemoved(clientIds[rand.IntN(clientCount)])
			}
		}
	}()
	subscription := subscriptions.offer(t)
	for i := range iterationCount {
		clientId := clientIds[rand.IntN(clientCount)]
		switch i % 32 {
		case 31:
			subscription.end()
			subscription = subscriptions.offer(t)
		case 1, 2, 3:
			subscription.send(clientId, "expire")
		default:
			subscription.send(clientId, "set")
		}
	}
	workers.Wait()
	// ends the last check
	subscription.end()

	// every canceled device is uninstalled by its worker. A device whose
	// lifecycle ended stays installed until an open replaces it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		installed := fixture.installed()
		settled := true
		for _, proxyDevices := range installed {
			for _, pd := range proxyDevices {
				select {
				case <-pd.Done():
					settled = false
				default:
				}
			}
		}
		if settled && equalClientDevices(fixture.indexed(), installed) {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("client index = %s, installed devices = %s", formatClientDevices(fixture.indexed()), formatClientDevices(installed))
		}
		time.Sleep(time.Millisecond)
	}

	fixture.close()
	fixture.requireIndex(clientDeviceIndex{})
}

// Flag reads that find nothing, so that only an event can close a device.
func clientRemovedTestEventsOnlySettings() *ClientRemovedSettings {
	settings := DefaultClientRemovedSettings()
	settings.ResubscribeTimeout = 10 * time.Millisecond
	settings.isClientRemoved = func(context.Context, server.Id) bool {
		return false
	}
	return settings
}

// Removes the client until its devices close. A new subscription may not be
// live when the first SET lands, and each SET is a new event.
func (self *clientRemovedTestFixture) removeUntilClosed(ctx context.Context, clientId server.Id, pds ...*ProxyDevice) {
	self.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		model.SetClientsRemoved(ctx, []server.Id{clientId})
		retry := time.After(200 * time.Millisecond)
		closed := true
		for _, pd := range pds {
			select {
			case <-self.closedNotify(pd):
			case <-retry:
				closed = false
			}
			if !closed {
				break
			}
		}
		if closed {
			return
		}
		if deadline.Before(time.Now()) {
			self.t.Fatalf("the removal of client %s did not close its devices", clientId)
		}
	}
}

// A removal's SET of the client's flag, seen through the Redis keyspace
// events of the flag, closes exactly that client's devices.
func TestClientRemovedKeyEventClosesClientDevices(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server.Testing_EnableKeyspaceNotifications(ctx)

		fixture := newClientRemovedTestFixture(t, ctx, clientRemovedTestEventsOnlySettings())
		defer fixture.close()
		clientA, clientB := server.NewId(), server.NewId()
		proxyB1 := server.NewId()
		a1 := fixture.open(server.NewId(), clientA)
		a2 := fixture.open(server.NewId(), clientA)
		b1 := fixture.open(proxyB1, clientB)

		fixture.removeUntilClosed(ctx, clientA, a1, a2)
		fixture.requireLive(b1)
		fixture.requireIndex(clientDeviceIndex{
			clientB: {proxyB1: b1},
		})
	})
}

// Records the events of one channel that reach the subscriber, from the Redis
// subscription.
type clientRemovedTestRecorder struct {
	channel string

	stateLock sync.Mutex
	events    []string
}

func (self *clientRemovedTestRecorder) subscribe(ctx context.Context, topologyRefresh time.Duration) (<-chan server.RedisMessage, <-chan struct{}, func(), error) {
	messages, done, unsub, err := subscribeClientRemovedKeyEvents(ctx, topologyRefresh)
	if err != nil {
		return messages, done, unsub, err
	}
	recorded := make(chan server.RedisMessage, 1024)
	go func() {
		defer close(recorded)
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case message, ok := <-messages:
				if !ok {
					return
				}
				if message.Channel == self.channel {
					self.record(message.Payload)
				}
				select {
				case <-ctx.Done():
					return
				case <-done:
					return
				case recorded <- message:
				}
			}
		}
	}()
	return recorded, done, unsub, nil
}

func (self *clientRemovedTestRecorder) record(event string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

func (self *clientRemovedTestRecorder) recordedEvents() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

// Events of a client's removed flag other than a SET (append, expire, expired
// and del here) do not close the client's devices.
func TestClientRemovedKeyEventIgnoresOtherEvents(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server.Testing_EnableKeyspaceNotifications(ctx)

		probeClientId, targetClientId, controlClientId := server.NewId(), server.NewId(), server.NewId()
		recorder := &clientRemovedTestRecorder{
			channel: clientRemovedTestChannel(server.RedisDb(), targetClientId),
		}
		settings := clientRemovedTestEventsOnlySettings()
		settings.subscribe = recorder.subscribe
		fixture := newClientRemovedTestFixture(t, ctx, settings)
		defer fixture.close()
		probe := fixture.open(server.NewId(), probeClientId)
		target := fixture.open(server.NewId(), targetClientId)
		control := fixture.open(server.NewId(), controlClientId)

		// the subscription is live once a removal closes the probe
		fixture.removeUntilClosed(ctx, probeClientId, probe)

		key := clientRemovedTestKey(targetClientId)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Append(ctx, key, "1").Err())
			server.Raise(r.PExpire(ctx, key, time.Minute).Err())
			server.Raise(r.Del(ctx, key).Err())
			server.Raise(r.Append(ctx, key, "1").Err())
			server.Raise(r.PExpire(ctx, key, time.Millisecond).Err())
		})
		// a read of an expired key expires it, which publishes `expired`
		deadline := time.Now().Add(10 * time.Second)
		for {
			var count int64
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				count, err = r.Exists(ctx, key).Result()
				server.Raise(err)
			})
			if count == 0 {
				break
			}
			if deadline.Before(time.Now()) {
				t.Fatal("the flag did not expire")
			}
			time.Sleep(time.Millisecond)
		}

		// one connection's events arrive in order, so once the control's
		// removal closes it every event before it was dispatched
		fixture.removeUntilClosed(ctx, controlClientId, control)
		fixture.requireLive(target)
		events := recorder.recordedEvents()
		for _, event := range []string{"append", "expire", "del", "expired"} {
			if !slices.Contains(events, event) {
				t.Fatalf("the subscriber received %v for the target's flag, without %q", events, event)
			}
		}
		if slices.Contains(events, "set") {
			t.Fatalf("the subscriber received %v for the target's flag, with a set", events)
		}
	})
}

// Events published while unsubscribed are gone. The next subscription reads
// the removed flag (model.IsClientRemoved) of every installed device's client
// and closes the devices of the removed ones.
func TestClientRemovedResubscribeClosesDevicesRemovedWhileUnsubscribed(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server.Testing_EnableKeyspaceNotifications(ctx)

		// the test ends the first subscription, and holds the second until
		// the flag is set
		end := make(chan struct{})
		resubscribing := make(chan struct{})
		hold := make(chan struct{})
		var subscribeCount atomic.Int32
		settings := DefaultClientRemovedSettings()
		settings.ResubscribeTimeout = 10 * time.Millisecond
		settings.subscribe = func(ctx context.Context, topologyRefresh time.Duration) (<-chan server.RedisMessage, <-chan struct{}, func(), error) {
			switch subscribeCount.Add(1) {
			case 1:
				messages, done, unsub, err := subscribeClientRemovedKeyEvents(ctx, topologyRefresh)
				if err != nil {
					return messages, done, unsub, err
				}
				ended := make(chan struct{})
				go func() {
					defer close(ended)
					select {
					case <-done:
					case <-end:
					case <-ctx.Done():
					}
				}()
				return messages, ended, unsub, nil
			case 2:
				close(resubscribing)
				select {
				case <-hold:
				case <-ctx.Done():
					return nil, nil, nil, ctx.Err()
				}
			}
			return subscribeClientRemovedKeyEvents(ctx, topologyRefresh)
		}
		fixture := newClientRemovedTestFixture(t, ctx, settings)
		defer fixture.close()
		clientA, clientB := server.NewId(), server.NewId()
		proxyB1 := server.NewId()
		a1 := fixture.open(server.NewId(), clientA)
		b1 := fixture.open(proxyB1, clientB)

		close(end)
		select {
		case <-resubscribing:
		case <-time.After(10 * time.Second):
			t.Fatal("the subscriber did not resubscribe")
		}
		// the first subscription is gone, so its event reaches no one
		model.SetClientsRemoved(ctx, []server.Id{clientA})
		fixture.requireLive(a1)

		close(hold)
		fixture.waitClosed(a1)
		fixture.requireLive(b1)
		fixture.requireIndex(clientDeviceIndex{
			clientB: {proxyB1: b1},
		})
	})
}
