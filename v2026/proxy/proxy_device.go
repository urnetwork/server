package proxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
)

func DefaultProxyDeviceManagerSettings() *ProxyDeviceManagerSettings {
	return &ProxyDeviceManagerSettings{
		CheckProxyDeviceIdleTimeout: 1 * time.Minute,
		SequenceBufferSize:          2048,
	}
}

type ProxyDeviceManagerSettings struct {
	CheckProxyDeviceIdleTimeout time.Duration
	SequenceBufferSize          int

	// when set, these override the default device security policies for all
	// devices opened by this manager (see ProxyDeviceSettings). Integration
	// tests use this to allow local target servers through the device path.
	IngressSecurityPolicyGenerator func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	EgressSecurityPolicyGenerator  func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy

	// NetworkSpace, when set, overrides the default platform network space.
	// Integration tests use this to point proxy devices at local api/connect
	// servers (see sdk.Testing_NewNetworkSpaceWithUrls).
	NetworkSpace *sdk.NetworkSpace
}

type ProxyDeviceManager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *ProxyDeviceManagerSettings

	// networkSpace *sdk.NetworkSpace

	// stateLock guards the proxyDevices map. It is read-mostly: every
	// OpenProxyDevice looks up an existing pdState (RLock, concurrent), and only
	// the first open for a proxy id — or a teardown removing an entry — takes the
	// write lock. This keeps the new-connection / new-client path from
	// serializing on one global mutex.
	stateLock    sync.RWMutex
	proxyDevices map[server.Id]*proxyDeviceState
}

func NewProxyDeviceManagerWithDefaults(ctx context.Context) *ProxyDeviceManager {
	return NewProxyDeviceManager(ctx, DefaultProxyDeviceManagerSettings())
}

func NewProxyDeviceManager(ctx context.Context, settings *ProxyDeviceManagerSettings) *ProxyDeviceManager {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &ProxyDeviceManager{
		ctx:      cancelCtx,
		cancel:   cancel,
		settings: settings,
		// networkSpace: networkSpace,
		proxyDevices: map[server.Id]*proxyDeviceState{},
	}
}

func (self *ProxyDeviceManager) OpenProxyDevice(proxyId server.Id) (*ProxyDevice, error) {
	pdState := func() *proxyDeviceState {
		// fast path: an existing entry, read concurrently (the common case)
		self.stateLock.RLock()
		pdState, ok := self.proxyDevices[proxyId]
		self.stateLock.RUnlock()
		if ok {
			return pdState
		}
		// slow path: create the entry under the write lock, double-checking in
		// case another opener created it between the RUnlock and the Lock
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if pdState, ok = self.proxyDevices[proxyId]; !ok {
			pdState = &proxyDeviceState{}
			self.proxyDevices[proxyId] = pdState
		}
		return pdState
	}()

	for {
		pdState.StateLock.Lock()

		// reuse a live device
		if pd := pdState.ProxyDevice; pd != nil {
			if pd.Active() && pd.UpdateActivity() {
				pdState.StateLock.Unlock()
				return pd, nil
			}
			// idled out, or the egress window collapsed: drop the dead device
			pd.Cancel()
			pdState.ProxyDevice = nil
		}

		// if another opener is already creating a device for this proxy id, wait
		// for its result instead of creating a duplicate — and wait WITHOUT
		// holding any lock, so a slow creation never serializes other proxy ids
		// (or, via the wg data path, other clients).
		if c := pdState.creating; c != nil {
			pdState.StateLock.Unlock()
			select {
			case <-c.done:
			case <-self.ctx.Done():
				return nil, fmt.Errorf("Proxy device manager closed.")
			}
			if c.err != nil {
				return nil, c.err
			}
			// a device was published; loop to validate and adopt it
			continue
		}

		// become the creator: publish an in-flight marker, release the lock, then
		// create the device (db load + DeviceLocal + tun) with NO lock held so the
		// cold start never blocks other proxy ids/clients (fix E).
		c := &deviceCreation{done: make(chan struct{})}
		pdState.creating = c
		pdState.StateLock.Unlock()

		pd, err := self.newProxyDevice(proxyId)

		pdState.StateLock.Lock()
		pdState.creating = nil
		if err == nil {
			pdState.ProxyDevice = pd
		}
		pdState.StateLock.Unlock()

		// waiters re-read pdState.ProxyDevice (re-validating liveness) on wake, so
		// only the error needs to be shared directly
		c.err = err
		close(c.done)

		if err != nil {
			return nil, err
		}
		return pd, nil
	}
}

// newProxyDevice creates a fresh proxy device for the proxy id and starts its
// run + idle-check goroutines. It does db + network + tun setup, so it must be
// called WITHOUT holding any manager or pdState lock (see OpenProxyDevice).
func (self *ProxyDeviceManager) newProxyDevice(proxyId server.Id) (*ProxyDevice, error) {
	proxyDeviceConfig := model.GetProxyDeviceConfig(self.ctx, proxyId)
	if proxyDeviceConfig == nil {
		return nil, fmt.Errorf("Proxy device does not exist.")
	}

	networkSpace := self.settings.NetworkSpace
	if networkSpace == nil {
		connectSettings := connect.DefaultConnectSettings()
		// FIXME use only ipv4 when communicating back to the platform
		connectSettings.DisableIpv6 = true
		// embedded devices must be silent: this host runs thousands of clients.
		// the network space logger silences the shared client strategy.
		connectSettings.Log = connect.NewNoopLogger()
		networkSpace = sdk.NewPlatformNetworkSpace(
			self.ctx,
			server.RequireEnv(),
			server.RequireDomain(),
			connectSettings,
		)
	}

	settings := DefaultProxyDeviceSettingsWithBufferSize(self.settings.SequenceBufferSize)
	settings.IngressSecurityPolicyGenerator = self.settings.IngressSecurityPolicyGenerator
	settings.EgressSecurityPolicyGenerator = self.settings.EgressSecurityPolicyGenerator
	pd, err := NewProxyDevice(self.ctx, proxyDeviceConfig, networkSpace, settings)
	if err != nil {
		return nil, err
	}

	go server.HandleError(func() {
		defer func() {
			// forget the device (if it is still the installed one), then close it
			// OUTSIDE the manager lock: deviceLocal/tun close can block, and holding
			// stateLock across it would stall OpenProxyDevice for every other proxy
			// id — and, via the wg data path, every other client (fix C).
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()

				if pdState, ok := self.proxyDevices[proxyId]; ok {
					pdState.StateLock.Lock()
					defer pdState.StateLock.Unlock()

					if pd == pdState.ProxyDevice {
						pdState.ProxyDevice = nil
					}
					// drop the empty entry to keep the map bounded under churn, but
					// only when nothing is installed and no creation is in flight (a
					// concurrent opener may hold this pdState, about to publish).
					if pdState.ProxyDevice == nil && pdState.creating == nil {
						delete(self.proxyDevices, proxyId)
					}
				}
			}()
			pd.Close()
		}()
		pd.Run()
	})

	go server.HandleError(func() {
		for {
			if pd.CancelIfIdle() {
				return
			}

			select {
			case <-pd.Done():
				return
			case <-time.After(self.settings.CheckProxyDeviceIdleTimeout):
			}
		}
	})

	return pd, nil
}

func (self *ProxyDeviceManager) ValidCaller(proxyId server.Id, addr netip.Addr) bool {
	// FIXME
	return true
}

func (self *ProxyDeviceManager) Close() {
	self.cancel()
}

type proxyDeviceState struct {
	StateLock   sync.Mutex
	ProxyDevice *ProxyDevice
	// creating is non-nil while an opener is creating a device for this proxy id.
	// Other openers wait on it instead of creating a duplicate, and without
	// holding StateLock across the (slow) creation.
	creating *deviceCreation
}

// deviceCreation lets concurrent openers for the same proxy id wait on an
// in-flight device creation: done is closed when it finishes, and err carries a
// creation failure to the waiters (success is read back from pdState.ProxyDevice).
type deviceCreation struct {
	done chan struct{}
	err  error
}

func DefaultProxyDeviceSettings() *ProxyDeviceSettings {
	return DefaultProxyDeviceSettingsWithBufferSize(32)
}

func DefaultProxyDeviceSettingsWithBufferSize(bufferSize int) *ProxyDeviceSettings {
	return &ProxyDeviceSettings{
		ProxyDeviceDescription: "resident proxy",
		ProxyDeviceSpec:        "resident proxy",
		Mtu:                    1440,
		ProxyDeviceIdleTimeout: 90 * time.Minute,
		SequenceBufferSize:     bufferSize,
	}
}

type ProxyDeviceSettings struct {
	ProxyDeviceDescription         string
	ProxyDeviceSpec                string
	IngressSecurityPolicyGenerator func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	EgressSecurityPolicyGenerator  func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	Mtu                            int
	ProxyDeviceIdleTimeout         time.Duration
	SequenceBufferSize             int
}

type ProxyDevice struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientId          server.Id
	instanceId        server.Id
	proxyDeviceConfig *model.ProxyDeviceConfig

	deviceLocal *sdk.DeviceLocal
	tun         *connect.Tun
	settings    *ProxyDeviceSettings

	// liveness/activity are tracked with atomics, not stateLock, so the wg
	// per-packet hot path (activateClient -> Active/UpdateActivity) takes no
	// per-device lock and — crucially — is never serialized under the wg proxy's
	// single global state lock.
	lastActivityNanos atomic.Int64
	// set once the egress window has been satisfied at least once. A device that
	// was ready and has since lost its window is dead and must be recreated; a
	// device that has never been ready is still warming up and is kept.
	everReady atomic.Bool

	// stateLock guards only the receive-mode fields below (swapped rarely, via
	// SetReceive), not the activity/liveness state above.
	stateLock      sync.Mutex
	receiveMonitor *connect.Monitor
	receiveNotify  chan struct{}
	receive        chan []byte
}

func NewProxyDeviceWithDefaults(
	ctx context.Context,
	proxyDeviceConfig *model.ProxyDeviceConfig,
	networkSpace *sdk.NetworkSpace,
) (*ProxyDevice, error) {
	return NewProxyDevice(
		ctx,
		proxyDeviceConfig,
		networkSpace,
		DefaultProxyDeviceSettings(),
	)
}

func NewProxyDevice(
	ctx context.Context,
	proxyDeviceConfig *model.ProxyDeviceConfig,
	networkSpace *sdk.NetworkSpace,
	settings *ProxyDeviceSettings,
) (*ProxyDevice, error) {
	// this jwt is used to access the services in the network space
	byJwt, err := jwt.LoadByJwtFromClientId(ctx, proxyDeviceConfig.ClientId)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(ctx)

	deviceLocalSettings := sdk.DefaultDeviceLocalSettings()
	// embedded devices must be silent: this host runs thousands of clients
	deviceLocalSettings.DisableLogging = true
	deviceLocal, err := sdk.NewPlatformDeviceLocal(
		nil,
		networkSpace,
		byJwt.Sign(),
		settings.ProxyDeviceDescription,
		settings.ProxyDeviceSpec,
		server.RequireVersion(),
		sdk.RequireIdFromBytes(proxyDeviceConfig.InstanceId.Bytes()),
		deviceLocalSettings,
	)
	if err != nil {
		cancel()
		return nil, err
	}
	deviceLocal.SetIngressSecurityPolicyGenerator(settings.IngressSecurityPolicyGenerator)
	deviceLocal.SetEgressSecurityPolicyGenerator(settings.EgressSecurityPolicyGenerator)
	// the proxy egresses DNS and HTTP unchanged (pass-through); disable the upgrade mux
	// so each of the many proxy devices avoids the per-device tun/stack it would create
	deviceLocal.SetUpgradeMuxSettings(nil)

	var dnsResolverSettings *connect.DnsResolverSettings
	if initialDeviceState := proxyDeviceConfig.InitialDeviceState; initialDeviceState != nil {
		deviceLocal.SetPerformanceProfile(initialDeviceState.PerformanceProfile)
		deviceLocal.SetConnectLocation(initialDeviceState.Location)
		dnsResolverSettings = initialDeviceState.DnsResolverSettings
	}

	// the proxy runs on the shared gVisor stack, so its TCP buffers come from the default
	// settings (up to 1MB per connection) rather than a per-tun override.
	tunSettings := connect.DefaultTunSettingsWithBufferSize(settings.SequenceBufferSize)
	tunSettings.Mtu = settings.Mtu

	tun, err := connect.CreateTunWithResolver(
		cancelCtx,
		tunSettings,
		dnsResolverSettings,
	)
	if err != nil {
		// release in the same order as `Close`
		cancel()
		deviceLocal.Close()
		return nil, err
	}

	proxyDevice := &ProxyDevice{
		ctx:               cancelCtx,
		cancel:            cancel,
		proxyDeviceConfig: proxyDeviceConfig,
		deviceLocal:       deviceLocal,
		tun:               tun,
		settings:          settings,
		receiveMonitor:    connect.NewMonitor(),
	}
	proxyDevice.lastActivityNanos.Store(time.Now().UnixNano())

	glog.Infof("[pd]using api=%s connect=%s\n", networkSpace.GetApiUrl(), networkSpace.GetPlatformUrl())

	return proxyDevice, nil
}

// directly copy between tun and device
func (self *ProxyDevice) Run() {
	defer self.cancel()

	// note the packet is only retained for the duration of the callback
	// use `MessagePoolShareReadOnly` to share it outside of the callback
	receiveCallback := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		if !self.UpdateActivity() {
			return
		}
		for {
			receive, notify := self.receiveWithNotify()
			if receive != nil {
				// the callback only borrows packet (ownership stays with the
				// caller, which recycles it after we return). share a copy so
				// ownership transfers to the receive consumer on a successful
				// send; the consumer returns it to the pool after use.
				sharedPacket := connect.MessagePoolShareReadOnly(packet)
				select {
				case <-self.ctx.Done():
					connect.MessagePoolReturn(sharedPacket)
					return
				case receive <- sharedPacket:
					self.UpdateActivity()
					return
				case <-notify:
					connect.MessagePoolReturn(sharedPacket)
				}
			} else {
				self.tun.Write(packet)
				self.UpdateActivity()
				return
			}
		}
	}
	sub := self.deviceLocal.AddReceivePacketCallback(receiveCallback)
	defer sub()

	// read in batches to reduce wakeups under load
	packets := make([][]byte, 64)
	for {
		if !self.UpdateActivity() {
			return
		}
		n, err := self.tun.ReadBatch(packets)
		if err != nil {
			return
		}
		if !self.UpdateActivity() {
			return
		}
		for _, packet := range packets[0:n] {
			success := self.deviceLocal.SendPacketNoCopy(packet, int32(len(packet)))
			if !success {
				connect.MessagePoolReturn(packet)
			}
		}
	}
}

func (self *ProxyDevice) Send(packet []byte) bool {
	if !self.UpdateActivity() {
		return false
	}
	return self.deviceLocal.SendPacketNoCopy(packet, int32(len(packet)))
}

func (self *ProxyDevice) SetReceive(receive chan []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.receive == receive {
		// already in this mode; avoid monitor churn / waking the receive callback
		return
	}
	self.receiveMonitor.NotifyAll()
	self.receive = receive
	self.receiveNotify = self.receiveMonitor.NotifyChannel()
}

func (self *ProxyDevice) receiveWithNotify() (chan []byte, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.receive, self.receiveNotify
}

func (self *ProxyDevice) Tun() *connect.Tun {
	return self.tun
}

// DialContext dials a connection through the device's tun. A tun-based dial
// means the device is being used as an http/socks proxy, so it first resets any
// wg "receive" mode (set via SetReceive). A device serves one proxy mode at a
// time — in practice a device is only ever wg, http, or socks — but resetting
// the mode on a new call lets the same device be reused if the mode changes,
// instead of stranding outbound packets on a stale wg receive channel.
func (self *ProxyDevice) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	self.SetReceive(nil)
	return self.tun.DialContext(ctx, network, addr)
}

func (self *ProxyDevice) WaitForReady(ctx context.Context, timeout time.Duration) bool {
	readyCtx, readyCancel := context.WithCancel(self.ctx)
	defer readyCancel()
	go server.HandleError(func() {
		select {
		case <-readyCtx.Done():
		case <-ctx.Done():
			readyCancel()
		}
	})

	windowStatus := self.deviceLocal.GetWindowStatus()
	if windowStatus.MinSatisfied {
		return true
	}

	if timeout == 0 {
		return false
	}

	sub := self.deviceLocal.AddWindowStatusChangeListener(&windowStatusChangeListener{
		callback: func(windowStatus *sdk.WindowStatus) {
			if windowStatus.MinSatisfied {
				readyCancel()
			}
		},
	})
	defer sub.Close()

	if 0 < timeout {
		select {
		case <-readyCtx.Done():
			return true
		case <-time.After(timeout):
			return false
		}
	} else {
		select {
		case <-readyCtx.Done():
			return true
		}
	}
}

// conforms to `sdk.WindowStatusChangeListener`
type windowStatusChangeListener struct {
	callback func(*sdk.WindowStatus)
}

func (self *windowStatusChangeListener) WindowStatusChanged(windowStatus *sdk.WindowStatus) {
	self.callback(windowStatus)
}

// Active reports whether the device can still serve traffic and is the gate for
// reusing an existing device in OpenProxyDevice. The context must be live (not
// idled out via CancelIfIdle, closed, or torn down), and the device must either
// still be warming up — it has never reached a satisfied egress window — or
// currently have a satisfied window.
//
// A device that reached ready and has since lost its egress window is NOT active:
// the egress path was dropped (e.g. the resident moved or the connection idled
// out and the window collapsed). None of those cancel the device context, so
// UpdateActivity alone (which only checks the context) would keep handing back a
// device that can no longer carry traffic — and because each reuse bumps the
// activity timestamp, the idle timer would never fire to recycle it. Gating
// reuse on the actual egress window lets OpenProxyDevice recreate the device.
func (self *ProxyDevice) Active() bool {
	select {
	case <-self.ctx.Done():
		return false
	default:
	}

	// the window status is authoritative (DeviceLocal has its own lock). It is
	// read here rather than cached because a device whose egress collapses does
	// not always emit a window-status event (e.g. deviceLocal.Close nils the
	// client), and a stale "satisfied" cache would keep handing back a dead
	// device. everReady is a sticky atomic so this whole check takes no lock.
	if self.deviceLocal.GetWindowStatus().MinSatisfied {
		self.everReady.Store(true)
		return true
	}
	// keep a device that has not yet had a chance to connect; only a device that
	// was ready and lost its window is treated as dead
	return !self.everReady.Load()
}

func (self *ProxyDevice) UpdateActivity() bool {
	select {
	case <-self.ctx.Done():
		return false
	default:
		self.lastActivityNanos.Store(time.Now().UnixNano())
		return true
	}
}

func (self *ProxyDevice) CancelIfIdle() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
	}

	idleTimeout := time.Since(time.Unix(0, self.lastActivityNanos.Load()))
	if self.settings.ProxyDeviceIdleTimeout <= idleTimeout {
		self.cancel()
		return true
	}
	return false
}

func (self *ProxyDevice) Done() <-chan struct{} {
	return self.ctx.Done()
}

// TODO connect device remote control connection

func (self *ProxyDevice) Cancel() {
	self.cancel()
}

func (self *ProxyDevice) Close() error {
	self.cancel()

	self.deviceLocal.Close()
	return self.tun.Close()
}
