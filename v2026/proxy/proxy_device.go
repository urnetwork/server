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

	// when set, this overrides the default client security policy for all devices
	// opened by this manager (see ProxyDeviceSettings). Integration tests use it
	// (DisableSecurityPolicyWithStats) to allow local target servers through the
	// device path.
	ClientSecurityPolicyGenerator func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy

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

	// The ip lock, memoized. ValidCaller runs on EVERY accepted connection, so reading
	// the device config from redis each time would put a round-trip on the accept path.
	// The ttl bounds how long a stale lock is enforced after the config changes.
	lockCacheLock sync.Mutex
	lockCache     map[server.Id]proxyLockEntry
}

// proxyLockCacheTtl bounds how long a stale ip lock can be enforced after the proxy
// device config changes.
const proxyLockCacheTtl = 30 * time.Second

type proxyLockEntry struct {
	// found is false when the proxy id has no config at all -- it was deleted, expired,
	// or never existed. That is cached too: an unknown proxy id being hammered must not
	// hit the db on every attempt.
	found       bool
	lockSubnets []netip.Prefix
	expiry      time.Time
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
		lockCache:    map[server.Id]proxyLockEntry{},
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
	settings.ClientSecurityPolicyGenerator = self.settings.ClientSecurityPolicyGenerator
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

// ValidCaller reports whether a caller at `addr` is authorized to use `proxyId`.
//
// This enforces the ip lock the CUSTOMER asked for. `proxy_config.lock_caller_ip` pins a
// proxy to the ip that created it; `proxy_config.lock_ip_list` pins it to an explicit set
// of addresses or CIDRs. Both are recorded as LockSubnets on the proxy device config.
//
// It used to be `// FIXME` returning true, so the lock was never applied AT ALL. A
// customer who explicitly asked that their proxy be usable only from their own ip got no
// restriction whatsoever — anyone holding the signed proxy id could use it from anywhere.
// The feature existed, was requested, was stored, and was then ignored.
//
// A proxy id with no config is DENIED: it was deleted, expired, or never existed, and an
// unknown proxy is not an authorized one. (A redis or db outage panics rather than
// returning nil, so nil genuinely means "not found" — an outage cannot quietly turn this
// into a deny-all.)
func (self *ProxyDeviceManager) ValidCaller(proxyId server.Id, addr netip.Addr) bool {
	entry := self.proxyLock(proxyId)

	if !entry.found {
		glog.Infof("[proxy]caller %s refused: proxy %s has no config\n", addr, proxyId)
		return false
	}

	if len(entry.lockSubnets) == 0 {
		// the customer did not ask for an ip lock
		return true
	}

	for _, lockSubnet := range entry.lockSubnets {
		if subnetContains(lockSubnet, addr) {
			return true
		}
	}

	glog.Infof(
		"[proxy]caller %s refused: outside the ip lock for proxy %s (%v)\n",
		addr, proxyId, entry.lockSubnets,
	)
	return false
}

// proxyLock returns the ip lock for a proxy id, memoized.
func (self *ProxyDeviceManager) proxyLock(proxyId server.Id) proxyLockEntry {
	now := time.Now()

	self.lockCacheLock.Lock()
	entry, ok := self.lockCache[proxyId]
	self.lockCacheLock.Unlock()
	if ok && now.Before(entry.expiry) {
		return entry
	}

	proxyDeviceConfig := model.GetProxyDeviceConfig(self.ctx, proxyId)

	entry = proxyLockEntry{
		found:  proxyDeviceConfig != nil,
		expiry: now.Add(proxyLockCacheTtl),
	}
	if proxyDeviceConfig != nil {
		entry.lockSubnets = proxyDeviceConfig.LockSubnets
	}

	self.lockCacheLock.Lock()
	self.lockCache[proxyId] = entry
	self.lockCacheLock.Unlock()

	return entry
}

// subnetContains reports whether addr falls inside subnet, normalizing v4-mapped-v6.
//
// A dual-stack listener reports an ipv4 peer as ::ffff:a.b.c.d, and netip.Prefix.Contains
// is false across address families. Without this normalization an ipv4 lock would never
// match an ipv4 caller — and the customer would be locked out of their own proxy by the
// very feature meant to protect it.
func subnetContains(subnet netip.Prefix, addr netip.Addr) bool {
	if subnet.Contains(addr) {
		return true
	}

	unmappedAddr := addr.Unmap()
	if unmappedAddr != addr && subnet.Contains(unmappedAddr) {
		return true
	}

	if subnet.Addr().Is4In6() {
		if bits := subnet.Bits() - 96; 0 <= bits {
			if unmappedSubnet, err := subnet.Addr().Unmap().Prefix(bits); err == nil {
				if unmappedSubnet.Contains(unmappedAddr) {
					return true
				}
			}
		}
	}

	return false
}

// ActiveProxyIds returns the proxy ids of open devices whose last activity
// falls within the window. This feeds the per-(host, block) activity set
// that a replacement instance pre-warms from (PROXYDRAIN1.md §3.3).
func (self *ProxyDeviceManager) ActiveProxyIds(window time.Duration) []server.Id {
	pds := func() map[server.Id]*ProxyDevice {
		self.stateLock.RLock()
		defer self.stateLock.RUnlock()
		pds := make(map[server.Id]*ProxyDevice, len(self.proxyDevices))
		for proxyId, pdState := range self.proxyDevices {
			pdState.StateLock.Lock()
			pd := pdState.ProxyDevice
			pdState.StateLock.Unlock()
			if pd != nil {
				pds[proxyId] = pd
			}
		}
		return pds
	}()

	activityStartTime := time.Now().Add(-window)
	proxyIds := []server.Id{}
	for proxyId, pd := range pds {
		select {
		case <-pd.Done():
			continue
		default:
		}
		if activityStartTime.Before(time.Unix(0, pd.lastActivityNanos.Load())) {
			proxyIds = append(proxyIds, proxyId)
		}
	}
	return proxyIds
}

// DeviceCount reports the number of proxy ids with an installed device.
func (self *ProxyDeviceManager) DeviceCount() int {
	self.stateLock.RLock()
	defer self.stateLock.RUnlock()
	return len(self.proxyDevices)
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
	ProxyDeviceDescription        string
	ProxyDeviceSpec               string
	ClientSecurityPolicyGenerator func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	Mtu                           int
	ProxyDeviceIdleTimeout        time.Duration
	SequenceBufferSize            int
	// DisableWindowIdentityPersistence turns off the window identity store
	// (PROXYDRAIN1.md §3.5); a recreated device then mints fresh window
	// client ids, orphaning established inner flows (the pre-persistence
	// behavior).
	DisableWindowIdentityPersistence bool
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

	// rpcListener serves device-rpc websockets relayed from the resident to
	// this hosted device (see PushDeviceRpc). deviceGeneration identifies this
	// device instance so a DeviceRemote detects recreation across reconnects.
	rpcListener      *sdk.HostedDeviceRpcListener
	deviceGeneration server.Id

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
	// persist the window client identities so a recreated device (deploy
	// restart) reuses them against the same providers, keeping established
	// inner flows resumable (PROXYDRAIN1.md §3.5)
	if !settings.DisableWindowIdentityPersistence {
		deviceLocalSettings.MultiClientIdentityStore = newWindowIdentityStore(ctx, proxyDeviceConfig.ProxyId)
	}
	// hosted devices must never route traffic locally or provide: local egress
	// would leave the proxy host's real interface (datacenter LAN, loopback,
	// metadata endpoint). This hard-guards route-local/provide setters on the
	// device and, together with the connectBlockActionOverrides strip, makes a
	// local route override impossible — defense in depth alongside the rpc-layer
	// DisableHostedIncompatible guard installed by StartHostedRpc.
	// It also hard-limits direct mode off (`MultiClientSettings.OverrideAllowDirect` = false):
	// a direct connection would leak that the client is hosted, and where it is
	// hosted, via the host addresses in the direct connection setup.
	deviceLocalSettings.HostedIncompatible = true
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
	deviceLocal.SetClientSecurityPolicyGenerator(settings.ClientSecurityPolicyGenerator)
	// the proxy egresses DNS and HTTP unchanged (pass-through); disable the upgrade mux
	// so each of the many proxy devices avoids the per-device tun/stack it would create
	deviceLocal.SetUpgradeMuxSettings(nil)

	var dnsResolverSettings *connect.DnsResolverSettings
	if initialDeviceState := proxyDeviceConfig.InitialDeviceState; initialDeviceState != nil {
		deviceLocal.SetPerformanceProfile(initialDeviceState.PerformanceProfile)
		deviceLocal.SetConnectLocation(initialDeviceState.Location)
		dnsResolverSettings = initialDeviceState.DnsResolverSettings
	}

	// The manager creates one ProxyDevice per client and closes it on disconnect.
	// Each Tun owns a private gVisor stack that Close() destroys, so a disconnecting
	// client fully reclaims its connections' endpoints. TCP buffers come from these
	// settings (up to 1MB per connection).
	tunSettings := connect.DefaultTunSettingsWithBufferSize(settings.SequenceBufferSize)
	tunSettings.Mtu = settings.Mtu

	// gVisor buffer sizes are LIMITS on what may be queued, not preallocations, so
	// they cost nothing while a connection is idle and everything while it is
	// backlogged. There is one stack PER CLIENT here and one endpoint per
	// connection/flow, so a buffer is multiplied by clients x endpoints.
	//
	// TCP keeps the full default (1MiB max per direction). tcp throughput is bounded
	// by window/RTT, so capping the window directly caps a single connection's
	// speed: at 128kib it would be ~10 Mbps on a 100ms path, ~21 Mbps on 50ms. That
	// is a user-visible performance cost, and it is not worth the memory. A
	// backlogged tcp endpoint costs ~184 KiB (measured, connect's
	// TestTunEndpointCapacityTcp).
	//
	// UDP does NOT have that trade-off, so it does not keep the default. The socks
	// associate relay caps datagrams at 2kib and drains every flow with a dedicated
	// reader, so it cannot use a deep queue at all: the 1MiB default is ~500
	// datagrams of headroom a prompt reader never fills. Measured cost of a
	// BACKLOGGED flow (connect's TestTunEndpointCapacityUdpSmallBuffers):
	//
	//	1MiB   -> 576 KiB/flow        128KiB -> 277 KiB/flow
	//	64KiB  -> 142 KiB/flow         32KiB ->  76 KiB/flow
	//
	// 128kib is still ~90 MTU-sized datagrams of burst headroom.
	tunSettings.UdpReceiveBufferByteCount = 128 * 1024
	tunSettings.UdpSendBufferByteCount = 128 * 1024

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

	// the hosted rpc listener lets a DeviceRemote (e.g. a browser over the
	// platform websocket) control this device. It is fed by the resident
	// bridge via PushDeviceRpc; the device generation identifies this instance
	// so the remote can detect a recreate.
	deviceGeneration := server.NewId()
	rpcListener := sdk.NewHostedDeviceRpcListener(cancelCtx)
	deviceLocal.StartHostedRpc(rpcListener, deviceGeneration.String())

	proxyDevice := &ProxyDevice{
		ctx:               cancelCtx,
		cancel:            cancel,
		clientId:          proxyDeviceConfig.ClientId,
		instanceId:        proxyDeviceConfig.InstanceId,
		proxyDeviceConfig: proxyDeviceConfig,
		deviceLocal:       deviceLocal,
		tun:               tun,
		settings:          settings,
		receiveMonitor:    connect.NewMonitor(),
		rpcListener:       rpcListener,
		deviceGeneration:  deviceGeneration,
	}
	proxyDevice.lastActivityNanos.Store(time.Now().UnixNano())

	glog.Infof("[pd]using api=%s connect=%s\n", networkSpace.GetApiUrl(), networkSpace.GetPlatformUrl())

	return proxyDevice, nil
}

// PushDeviceRpc serves a device-rpc websocket (relayed from the resident) to
// this hosted device. An attached rpc session keeps the device alive: activity
// is bumped for the session's duration so the idle reaper does not reap a
// device a remote is actively controlling. Blocks until the session ends.
func (self *ProxyDevice) PushDeviceRpc(ws sdk.DeviceRpcWs) error {
	if !self.UpdateActivity() {
		return fmt.Errorf("proxy device closed")
	}

	sessionCtx, sessionCancel := context.WithCancel(self.ctx)
	defer sessionCancel()
	// keep the device non-idle while the rpc session is attached
	go server.HandleError(func() {
		ticker := time.NewTicker(self.settings.ProxyDeviceIdleTimeout / 2)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				self.UpdateActivity()
			}
		}
	})

	return self.rpcListener.ServeWs(ws)
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
