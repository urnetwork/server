// A hosted proxy's device opens on the proxy's first use, not at its creation:
// a SOCKS sign-in or dial, an HTTP request, or the first packet of a WireGuard
// session. These tests drive the production wiring (the proxy client
// notification, the SOCKS and HTTP dial callbacks, the WireGuard packet entry
// and the manager's idle close) on the synctest clock, with devices that need
// no platform.
package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
	proxylib "github.com/urnetwork/proxy"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The (host, block) the tests' proxy client notification watches.
const lazyOpenTestProxyHost = "lazy-proxy"
const lazyOpenTestBlock = "g1"

// Prepares what the tests share across the process, before any clock bubble:
// the repository's synthetic vault (local/testdata, public throwaway
// WireGuard keys) that WireGuard clients are registered against, synthetic
// signing and plan data, and the notification's (host, block).
func configureLazyOpenTest(t *testing.T) {
	t.Helper()
	vaultHome, err := filepath.Abs(filepath.Join("..", "local", "testdata", "vault"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", vaultHome)
	t.Setenv("WARP_HOST", lazyOpenTestProxyHost)
	t.Setenv("WARP_BLOCK", lazyOpenTestBlock)
	t.Setenv("WARP_DOMAIN", "example")
	configureHttpBudgetTest(t)
	// the process loads the proxy config once; load it outside the bubble
	model.LoadServerProxyConfig()
}

// A window whose exits the test adds. Every listener hears each change, so
// several first connections can wait on it at once. Safe for concurrent use.
type lazyOpenTestWindow struct {
	stateLock      sync.Mutex
	status         sdk.WindowStatus
	listeners      map[int]sdk.WindowStatusChangeListener
	nextListenerId int
}

func newLazyOpenTestWindow() *lazyOpenTestWindow {
	return &lazyOpenTestWindow{
		listeners: map[int]sdk.WindowStatusChangeListener{},
	}
}

func (self *lazyOpenTestWindow) GetWindowStatus() *sdk.WindowStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	status := self.status
	return &status
}

func (self *lazyOpenTestWindow) GetDone() bool {
	return false
}

func (self *lazyOpenTestWindow) AddWindowStatusChangeListener(listener sdk.WindowStatusChangeListener) sdk.Sub {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	listenerId := self.nextListenerId
	self.nextListenerId += 1
	self.listeners[listenerId] = listener
	return &lazyOpenTestWindowSub{close: func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.listeners, listenerId)
	}}
}

// The window's first (or next) exit, as its first provider joining it.
func (self *lazyOpenTestWindow) addExit() {
	status, listeners := func() (sdk.WindowStatus, []sdk.WindowStatusChangeListener) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.status.ProviderStateAdded += 1
		listeners := make([]sdk.WindowStatusChangeListener, 0, len(self.listeners))
		for _, listener := range self.listeners {
			listeners = append(listeners, listener)
		}
		return self.status, listeners
	}()
	for _, listener := range listeners {
		listener.WindowStatusChanged(&status)
	}
}

type lazyOpenTestWindowSub struct {
	close func()
}

func (self *lazyOpenTestWindowSub) Close() {
	self.close()
}

// Builds the hosted devices of one test, as the manager's proxyDeviceBuilder.
// A device needs no platform: it reports the test's window, carries its dials
// over a pipe and counts its WireGuard packets, and counts its close. Hold
// keeps each construction waiting once it has started. Safe for concurrent
// use.
type lazyOpenTestDevices struct {
	manager *ProxyDeviceManager
	window  *lazyOpenTestWindow
	hold    chan struct{}

	startCount atomic.Int32
	startOnce  sync.Once
	started    chan struct{}

	stateLock       sync.Mutex
	devices         []*ProxyDevice
	dialDevices     []*ProxyDevice
	dialPipes       []net.Conn
	sentPacketCount int
	closedDevices   []*ProxyDevice
}

// The pipes the devices dialed are closed when the test ends.
func newLazyOpenTestDevices(t *testing.T, manager *ProxyDeviceManager, window *lazyOpenTestWindow) *lazyOpenTestDevices {
	devices := &lazyOpenTestDevices{
		manager: manager,
		window:  window,
		started: make(chan struct{}),
	}
	manager.proxyDeviceBuilder = devices.build
	t.Cleanup(func() {
		devices.stateLock.Lock()
		defer devices.stateLock.Unlock()
		for _, dialPipe := range devices.dialPipes {
			_ = dialPipe.Close()
		}
	})
	return devices
}

func (self *lazyOpenTestDevices) build(proxyId server.Id) (*ProxyDevice, error) {
	self.startCount.Add(1)
	self.startOnce.Do(func() { close(self.started) })
	if self.hold != nil {
		<-self.hold
	}

	ctx, cancel := context.WithCancel(self.manager.ctx)
	var device *ProxyDevice
	device = &ProxyDevice{
		ctx:            ctx,
		cancel:         cancel,
		deviceState:    self.window,
		settings:       DefaultProxyDeviceSettings(),
		receiveMonitor: connect.NewMonitor(),
		runForTest:     func() { <-ctx.Done() },
		closeDeviceLocalForTest: func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.closedDevices = append(self.closedDevices, device)
		},
		sendOwnedPacketsForTest: func(packets [][]byte) int {
			for _, packet := range packets {
				connect.MessagePoolReturn(packet)
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.sentPacketCount += len(packets)
			return len(packets)
		},
		dialForTest: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			conn, dialPipe := net.Pipe()
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.dialDevices = append(self.dialDevices, device)
			self.dialPipes = append(self.dialPipes, dialPipe)
			return conn, nil
		},
	}
	// as NewProxyDevice records its construction: activity, not traffic
	device.lastActivityNanos.Store(time.Now().UnixNano())

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.devices = append(self.devices, device)
	return device, nil
}

// The constructions started, including any held.
func (self *lazyOpenTestDevices) starts() int {
	return int(self.startCount.Load())
}

func (self *lazyOpenTestDevices) built() []*ProxyDevice {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*ProxyDevice{}, self.devices...)
}

// The device of each dial, in order.
func (self *lazyOpenTestDevices) dials() []*ProxyDevice {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*ProxyDevice{}, self.dialDevices...)
}

func (self *lazyOpenTestDevices) sent() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.sentPacketCount
}

func (self *lazyOpenTestDevices) closed() []*ProxyDevice {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*ProxyDevice{}, self.closedDevices...)
}

// Carries one byte from the client to the destination over conn, the relay
// connection of the devices' last dial: user traffic on the device it was
// dialed through.
func (self *lazyOpenTestDevices) carry(t *testing.T, conn net.Conn) {
	t.Helper()
	dialPipe := func() net.Conn {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.dialPipes) == 0 {
			return nil
		}
		return self.dialPipes[len(self.dialPipes)-1]
	}()
	if dialPipe == nil {
		t.Fatal("no dial to carry traffic over")
	}
	read := make(chan error, 1)
	go func() {
		_, err := dialPipe.Read(make([]byte, 1))
		read <- err
	}()
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}

// Serves the test's proxy clients as the change log of the watched host: each
// client's change id is its position, from 1. Safe for concurrent use.
type lazyOpenTestProxyClientChanges struct {
	stateLock    sync.Mutex
	proxyClients []*model.ProxyClient
}

func (self *lazyOpenTestProxyClientChanges) add(proxyClient *model.ProxyClient) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.proxyClients = append(self.proxyClients, proxyClient)
}

func (self *lazyOpenTestProxyClientChanges) getProxyClientsSince(
	ctx context.Context,
	proxyHost string,
	block string,
	changeId int64,
) (map[server.Id]*model.ProxyClient, int64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	proxyClients := map[server.Id]*model.ProxyClient{}
	maxChangeId := changeId
	if proxyHost != lazyOpenTestProxyHost || block != lazyOpenTestBlock {
		return proxyClients, maxChangeId, nil
	}
	for i, proxyClient := range self.proxyClients {
		clientChangeId := int64(i + 1)
		if changeId <= clientChangeId {
			proxyClients[proxyClient.ProxyId] = proxyClient
			maxChangeId = max(maxChangeId, clientChangeId)
		}
	}
	return proxyClients, maxChangeId, nil
}

// The production WireGuard device, made outside the clock bubble (see
// wgAdmissionTestProxy) with on-demand peers, as the proxy runs it: a
// registered client gets no device peer until it handshakes.
func newLazyOpenTestWgProxy(t *testing.T) *proxylib.WgProxy {
	t.Helper()
	settings := proxylib.DefaultWgProxySettings()
	settings.Log = connect.NewNoopLogger()
	settings.PeersOnDemand = true
	// no eviction worker outside the bubble
	settings.PeerEvictInterval = 0
	wg := proxylib.NewWgProxy(server.WithoutPostgres(context.Background()), settings)
	t.Cleanup(func() { _ = wg.Close() })
	return wg
}

// This instance's wg server around the bubble's manager, registered with a
// proxy client notification of the test's change log, as cli/proxy main
// registers it. The registered clients are removed as the bubble ends, so
// the tuns opened inside it are dropped inside it.
func watchLazyOpenTestProxyClients(
	t *testing.T,
	manager *ProxyDeviceManager,
	wg *proxylib.WgProxy,
	changes *lazyOpenTestProxyClientChanges,
) (*wgServer, *proxyClientNotification, *ProxySettings) {
	t.Helper()
	settings := DefaultProxySettings()
	// the full set reconcile is not under test
	settings.ReconcileTimeout = 0
	wgOwner := &wgServer{
		ctx:                manager.ctx,
		proxyDeviceManager: manager,
		settings:           settings,
		wgProxy:            wg,
	}
	t.Cleanup(func() {
		if err := wg.SetClients(map[netip.Addr]*proxylib.WgClient{}); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(manager.ctx)
	t.Cleanup(cancel)
	notification := newProxyClientNotification(
		ctx,
		settings,
		newTestProxyChangeReadSubscriptionSource(),
		changes.getProxyClientsSince,
	)
	unwatch := wgOwner.WatchProxyClients(notification)
	t.Cleanup(unwatch)
	return wgOwner, notification, settings
}

// A new proxy reaches this instance through the proxy client notification,
// which registers its WireGuard client and opens no device: the device opens
// on first use. The notification used to open the device of every client
// created in the last 30 minutes, at the initial sync and on each later
// delivery, and each such device's windows filled with exits and refilled for
// ten minutes whether or not the proxy was ever used. On 2026-11-13 a sign-up
// bot's 57k proxies took the proxy hosts from 12-17k live devices to 38k.
func TestNewProxyClientsOpenNoDevice(t *testing.T) {
	configureLazyOpenTest(t)
	wg := newLazyOpenTestWgProxy(t)
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		devices := newLazyOpenTestDevices(t, manager, newLazyOpenTestWindow())
		changes := &lazyOpenTestProxyClientChanges{}
		// created just now, so well inside the old 30-minute warmup
		changes.add(newTestProxyClient(t, netip.MustParseAddr("10.40.0.1")))
		_, notification, settings := watchLazyOpenTestProxyClients(t, manager, wg, changes)

		synctest.Wait()
		select {
		case <-notification.InitialSyncDone():
		default:
			t.Fatal("the initial sync was not applied")
		}
		if count := wg.ClientCount(); count != 1 {
			t.Fatalf("the initial sync registered %d WireGuard clients, want 1", count)
		}
		if devices.starts() != 0 || manager.DeviceCount() != 0 {
			t.Fatalf("the initial sync started %d device constructions for a proxy no one has used (%d devices), want none", devices.starts(), manager.DeviceCount())
		}

		// a proxy created after the initial sync, delivered by the next poll
		changes.add(newTestProxyClient(t, netip.MustParseAddr("10.40.0.2")))
		time.Sleep(settings.NotificationTimeout)
		synctest.Wait()
		if count := wg.ClientCount(); count != 2 {
			t.Fatalf("the next delivery registered %d WireGuard clients in all, want 2", count)
		}
		if devices.starts() != 0 || manager.DeviceCount() != 0 {
			t.Fatalf("a new proxy's delivery started %d device constructions (%d devices), want none", devices.starts(), manager.DeviceCount())
		}
	})
}

// A WireGuard proxy's device opens with the first packet of its client's
// session, the first one the device decrypts after the client's handshake.
// Registering the client opens nothing; the first packet opens the device and
// is carried through it, and the session's later packets reuse it.
func TestWgFirstSessionPacketOpensTheDevice(t *testing.T) {
	configureLazyOpenTest(t)
	wg := newLazyOpenTestWgProxy(t)
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		devices := newLazyOpenTestDevices(t, manager, newLazyOpenTestWindow())
		changes := &lazyOpenTestProxyClientChanges{}
		clientAddr := netip.MustParseAddr("10.40.0.3")
		proxyClient := newTestProxyClient(t, clientAddr)
		changes.add(proxyClient)
		watchLazyOpenTestProxyClients(t, manager, wg, changes)

		synctest.Wait()
		if count := wg.ClientCount(); count != 1 {
			t.Fatalf("%d WireGuard clients registered, want 1", count)
		}
		if devices.starts() != 0 {
			t.Fatalf("registering the WireGuard client started %d device constructions, want none", devices.starts())
		}

		packet := buildWgTestPacket(t, clientAddr.String(), "198.51.100.21", "first session packet")
		if count, err := wg.Write([][]byte{packet}, 0); count != 1 || err != nil {
			t.Fatalf("the first session packet: count=%d err=%v, want it carried", count, err)
		}
		built := devices.built()
		if devices.starts() != 1 || len(built) != 1 || manager.DeviceCount() != 1 {
			t.Fatalf("the first session packet started %d device constructions, want 1", devices.starts())
		}
		if got, err := manager.OpenProxyDevice(proxyClient.ProxyId); err != nil || got != built[0] {
			t.Fatalf("the proxy's device is %p (err=%v), want the one its first packet opened", got, err)
		}

		for range 3 {
			if count, err := wg.Write([][]byte{packet}, 0); count != 1 || err != nil {
				t.Fatalf("a later session packet: count=%d err=%v", count, err)
			}
		}
		if devices.starts() != 1 || devices.sent() != 4 {
			t.Fatalf("the session's packets started %d constructions and carried %d packets, want 1 and 4", devices.starts(), devices.sent())
		}
	})
}

// The dial result of one first connection.
type lazyOpenTestDial struct {
	conn net.Conn
	err  error
}

// An HTTP CONNECT through the production dial callback, from 192.0.2.1.
func lazyOpenTestHttpDial(ctx context.Context, core *proxylib.HttpProxy, proxyId server.Id) lazyOpenTestDial {
	r := httptest.NewRequest(http.MethodConnect, "https://192.0.2.10:443", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("Proxy-Authorization", httpConnectTestBasic(proxyId))
	conn, err := core.ConnectDialContextWithRequest(ctx, r, "tcp", "192.0.2.10:443")
	return lazyOpenTestDial{conn: conn, err: err}
}

// The first CONNECTs of a proxy with no device open it once. The first starts
// the construction and a concurrent one waits for it; both then wait, within
// the connect budget, for the window's first exit rather than fail, and dial
// through the one device.
func TestHttpConcurrentFirstConnectsOpenOneDevice(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		window := newLazyOpenTestWindow()
		devices := newLazyOpenTestDevices(t, manager, window)
		devices.hold = make(chan struct{})
		core := newHttpConnectTestProxy(manager)

		results := make(chan lazyOpenTestDial, 2)
		go func() { results <- lazyOpenTestHttpDial(manager.ctx, core, proxyId) }()
		<-devices.started
		go func() { results <- lazyOpenTestHttpDial(manager.ctx, core, proxyId) }()
		synctest.Wait()
		if devices.starts() != 1 {
			t.Fatalf("two first CONNECTs started %d device constructions, want 1", devices.starts())
		}

		close(devices.hold)
		synctest.Wait()
		// the device is open, and its window has no exit yet
		if manager.DeviceCount() != 1 || len(results) != 0 || len(devices.dials()) != 0 {
			t.Fatalf("before the first exit: %d devices, %d CONNECTs answered, %d dials; want 1, 0, 0", manager.DeviceCount(), len(results), len(devices.dials()))
		}

		window.addExit()
		for range 2 {
			result := <-results
			if result.err != nil {
				t.Fatalf("a first CONNECT failed once the window had an exit: %v", result.err)
			}
			_ = result.conn.Close()
		}
		built, dials := devices.built(), devices.dials()
		if devices.starts() != 1 || len(built) != 1 || len(dials) != 2 || dials[0] != built[0] || dials[1] != built[0] {
			t.Fatalf("%d constructions; the CONNECTs dialed through %v, want both through the one device %p", devices.starts(), dials, built)
		}
	})
}

// A SOCKS session's sign-in is its first use: it starts the device, so the
// device's construction and its window's first exit overlap the client's
// request round trip, and the request's dial joins that one construction. A
// sign-in the proxy refuses starts nothing, and a sign-in to an open device
// starts nothing more.
func TestSocksSignInStartsTheDevice(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId, lockedProxyId := server.NewId(), server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		// locked to a network the caller is not in
		seedHttpConnectTestLock(manager, lockedProxyId, true, netip.MustParsePrefix("198.51.100.0/24"))
		window := newLazyOpenTestWindow()
		devices := newLazyOpenTestDevices(t, manager, window)
		devices.hold = make(chan struct{})
		core := (&socks5Server{
			ctx:                manager.ctx,
			settings:           DefaultProxySettings(),
			proxyDeviceManager: manager,
		}).newSocksProxy()

		if core.ValidUser(model.SignProxyId(lockedProxyId), "x", "192.0.2.1:1234") {
			t.Fatal("a caller outside the proxy's ip lock signed in")
		}
		synctest.Wait()
		if devices.starts() != 0 {
			t.Fatal("a refused sign-in started a device")
		}

		if !core.ValidUser(model.SignProxyId(proxyId), "x", "192.0.2.1:1234") {
			t.Fatal("the proxy's caller did not sign in")
		}
		synctest.Wait()
		select {
		case <-devices.started:
		default:
			t.Fatal("the sign-in did not start the device; its first dial will")
		}

		request := &proxylib.Request{
			AuthContext: &proxylib.AuthContext{Payload: map[string]string{"username": model.SignProxyId(proxyId)}},
			DestAddr:    &proxylib.AddrSpec{FQDN: "target.example", Port: 443},
		}
		result := make(chan lazyOpenTestDial, 1)
		go func() {
			conn, err := core.ConnectDialWithRequest(manager.ctx, request, "tcp", "target.example:443")
			result <- lazyOpenTestDial{conn: conn, err: err}
		}()
		synctest.Wait()
		close(devices.hold)
		synctest.Wait()
		if manager.DeviceCount() != 1 || len(result) != 0 {
			t.Fatalf("before the first exit: %d devices, %d dials answered; want 1, 0", manager.DeviceCount(), len(result))
		}
		window.addExit()
		dial := <-result
		if dial.err != nil {
			t.Fatalf("the first dial failed once the window had an exit: %v", dial.err)
		}
		_ = dial.conn.Close()
		built, dials := devices.built(), devices.dials()
		if devices.starts() != 1 || len(built) != 1 || len(dials) != 1 || dials[0] != built[0] {
			t.Fatalf("%d constructions for a sign-in and its dial, want 1 carrying the dial", devices.starts())
		}

		if !core.ValidUser(model.SignProxyId(proxyId), "x", "192.0.2.1:1234") {
			t.Fatal("the proxy's caller did not sign in again")
		}
		synctest.Wait()
		if devices.starts() != 1 {
			t.Fatalf("a sign-in to an open device started a construction (%d in all)", devices.starts())
		}
	})
}

// A device that its first connection opened and used keeps the idle close:
// with no traffic for the idle timeout it is closed and forgotten, and the next
// connection opens a new one.
func TestProxyDeviceOpenedOnFirstUseClosesWhenIdle(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		window := newLazyOpenTestWindow()
		window.addExit()
		devices := newLazyOpenTestDevices(t, manager, window)
		core := newHttpConnectTestProxy(manager)

		first := lazyOpenTestHttpDial(manager.ctx, core, proxyId)
		if first.err != nil {
			t.Fatalf("the first CONNECT failed: %v", first.err)
		}
		devices.carry(t, first.conn)
		_ = first.conn.Close()
		if devices.starts() != 1 || manager.DeviceCount() != 1 {
			t.Fatalf("the first CONNECT started %d constructions, want 1", devices.starts())
		}

		idleTimeout := DefaultProxyDeviceSettings().ProxyDeviceIdleTimeout
		checkTimeout := manager.settings.CheckProxyDeviceIdleTimeout
		time.Sleep(idleTimeout - checkTimeout)
		synctest.Wait()
		if manager.DeviceCount() != 1 || len(devices.closed()) != 0 {
			t.Fatal("the device closed before its idle timeout")
		}
		time.Sleep(2 * checkTimeout)
		synctest.Wait()
		built := devices.built()
		if closed := devices.closed(); manager.DeviceCount() != 0 || len(closed) != 1 || closed[0] != built[0] {
			t.Fatalf("after %s idle: %d devices installed, %d closed; want the used device closed and forgotten", idleTimeout+checkTimeout, manager.DeviceCount(), len(devices.closed()))
		}

		next := lazyOpenTestHttpDial(manager.ctx, core, proxyId)
		if next.err != nil {
			t.Fatalf("the next CONNECT failed: %v", next.err)
		}
		_ = next.conn.Close()
		built = devices.built()
		if devices.starts() != 2 || len(built) != 2 || built[1] == built[0] {
			t.Fatalf("the next CONNECT after the idle close started %d constructions in all, want a second device", devices.starts())
		}
	})
}
