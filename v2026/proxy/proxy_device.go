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
		DeviceMemoryTargetByteCount: proxyDeviceMemoryTargetByteCountFromConfig(),
	}
}

const defaultProxyDeviceMemoryTargetByteCount = model.ByteCount(24 * model.Mib)

// proxyDeviceMemoryTargetByteCountFromConfig loads the single DeviceLocal
// steady-state target. Older environments without proxy.yml retain the 24 MiB
// default; a present but invalid value fails startup instead of silently
// restoring the process-global carrier budget.
func proxyDeviceMemoryTargetByteCountFromConfig() model.ByteCount {
	resource, err := server.Config.SimpleResource("proxy.yml")
	if err != nil {
		return defaultProxyDeviceMemoryTargetByteCount
	}
	values := resource.String("device_memory_budget")
	if len(values) == 0 {
		return defaultProxyDeviceMemoryTargetByteCount
	}
	if len(values) != 1 {
		panic(fmt.Errorf("proxy.yml: device_memory_budget must have exactly one value"))
	}
	byteCount, err := model.ParseByteCount(values[0])
	if err != nil {
		panic(fmt.Errorf(
			"proxy.yml: invalid device_memory_budget %q: %w",
			values[0],
			err,
		))
	}
	if byteCount <= 0 {
		panic(fmt.Errorf(
			"proxy.yml: device_memory_budget must be positive, got %q",
			values[0],
		))
	}
	return byteCount
}

type ProxyDeviceManagerSettings struct {
	CheckProxyDeviceIdleTimeout time.Duration
	SequenceBufferSize          int
	DeviceMemoryTargetByteCount model.ByteCount

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

	// Every production device borrows one manager-owned NetworkSpace. Its API
	// request core and client strategy are shared; sdk.DeviceLocal isolates the
	// mutable hosted credential session and all memory budgets per device.
	networkSpaceOnce    sync.Once
	networkSpace        *sdk.NetworkSpace
	networkSpaceBuilder func(context.Context) *sdk.NetworkSpace

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
	manager := &ProxyDeviceManager{
		ctx:          cancelCtx,
		cancel:       cancel,
		settings:     settings,
		networkSpace: settings.NetworkSpace,
		proxyDevices: map[server.Id]*proxyDeviceState{},
		lockCache:    map[server.Id]proxyLockEntry{},
	}
	manager.networkSpaceBuilder = newProxyDeviceManagerNetworkSpace
	return manager
}

// newProxyDeviceManagerNetworkSpace builds the one production NetworkSpace
// owned by a manager. It is lazy so construction-only unit tests do not need
// environment configuration.
func newProxyDeviceManagerNetworkSpace(ctx context.Context) *sdk.NetworkSpace {
	connectSettings := connect.DefaultConnectSettings()
	// FIXME use only ipv4 when communicating back to the platform
	connectSettings.DisableIpv6 = true
	// Embedded devices must be silent: this host runs thousands of clients.
	connectSettings.Log = connect.NewNoopLogger()
	return sdk.NewPlatformNetworkSpace(
		ctx,
		server.RequireEnv(),
		server.RequireDomain(),
		connectSettings,
	)
}

// networkSpaceForDevice returns the single manager-owned NetworkSpace. sync.Once
// makes simultaneous cold device opens share exactly one strategy/API core.
func (self *ProxyDeviceManager) networkSpaceForDevice() *sdk.NetworkSpace {
	self.networkSpaceOnce.Do(func() {
		if self.networkSpace == nil {
			self.networkSpace = self.networkSpaceBuilder(self.ctx)
		}
	})
	return self.networkSpace
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
			// The proxy or its DeviceLocal lifecycle ended. A merely unsatisfied
			// window stays installed and keeps refilling under the same device.
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

	networkSpace := self.networkSpaceForDevice()

	settings := DefaultProxyDeviceSettingsWithBufferSize(self.settings.SequenceBufferSize)
	settings.ClientSecurityPolicyGenerator = self.settings.ClientSecurityPolicyGenerator
	settings.MemoryTargetByteCount = self.settings.DeviceMemoryTargetByteCount
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

	// log the peppered hash of the caller, not the raw address (and not the
	// lock subnets, which for a LockCallerIp config are the customer's /32) —
	// the rest of the request path (transport_announce, rate limits) already
	// logs hex(ClientIpHash), so a refused caller stays correlatable with its
	// other log lines without putting the address itself in the logs
	if !entry.found {
		callerHash := server.ClientIpHashForAddr(addr)
		glog.Infof("[proxy]caller %x refused: proxy %s has no config\n", callerHash[:8], proxyId)
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

	callerHash := server.ClientIpHashForAddr(addr)
	glog.Infof(
		"[proxy]caller %x refused: outside the ip lock for proxy %s (%d subnets)\n",
		callerHash[:8], proxyId, len(entry.lockSubnets),
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
		Mtu:                    connect.DefaultMtu,
		ProxyDeviceIdleTimeout: 90 * time.Minute,
		SequenceBufferSize:     bufferSize,
		MemoryTargetByteCount:  defaultProxyDeviceMemoryTargetByteCount,
	}
}

type ProxyDeviceSettings struct {
	ProxyDeviceDescription        string
	ProxyDeviceSpec               string
	ClientSecurityPolicyGenerator func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	Mtu                           int
	ProxyDeviceIdleTimeout        time.Duration
	SequenceBufferSize            int
	// MemoryTargetByteCount is the one SDK DeviceLocal target from which DNS,
	// mux, transfer, P2P, and carrier budgets are derived.
	MemoryTargetByteCount model.ByteCount
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
	// deviceState is the lifecycle/readiness surface used by selection and
	// readiness. Production points it at deviceLocal; tests replace it with an
	// exact transition source.
	deviceState proxyDeviceStateSource
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
	// stateLock guards only the receive-attachment fields below (swapped rarely),
	// not the activity/liveness state above.
	stateLock      sync.Mutex
	receiveMonitor *connect.Monitor
	receiveNotify  chan struct{}
	receive        chan []byte
	receiveAddr    netip.Addr

	// Nil in production. Ownership tests replace the final asynchronous sends
	// while retaining the same borrowed-to-owned copy boundary.
	sendOwnedPacketForTest  func([]byte) bool
	sendOwnedPacketsForTest func([][]byte) int
	// Stops a forced-full receive delivery immediately before its blocking
	// handoff, allowing a deterministic backpressure regression test.
	receiveBackpressureForTest func()
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

	deviceLocalSettings := newProxyDeviceLocalSettings(ctx, proxyDeviceConfig, settings)
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
		deviceState:       deviceLocal,
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

// newProxyDeviceLocalSettings builds the immutable hosted-device policy used
// by every server/proxy device. HostedIncompatible pins the SDK carrier to H1
// and blocks Auto, H3, DNS, direct, local-route, and provider reconfiguration.
func newProxyDeviceLocalSettings(
	ctx context.Context,
	proxyDeviceConfig *model.ProxyDeviceConfig,
	proxyDeviceSettings *ProxyDeviceSettings,
) *sdk.DeviceLocalSettings {
	deviceLocalSettings := sdk.DefaultDeviceLocalSettings()
	// embedded devices must be silent: this host runs thousands of clients
	deviceLocalSettings.DisableLogging = true
	// The SDK derives every DeviceLocal-owned memory area from this one target,
	// including the private platform carrier budget. Hosted devices cannot
	// provide, so their provider share folds into their client transfer share.
	if proxyDeviceSettings.MemoryTargetByteCount <= 0 {
		panic("proxy DeviceLocal memory target must be positive")
	}
	deviceLocalSettings.MemoryTargetByteCount =
		proxyDeviceSettings.MemoryTargetByteCount
	// persist the window client identities so a recreated device (deploy
	// restart) reuses them against the same providers, keeping established
	// inner flows resumable (PROXYDRAIN1.md §3.5)
	if !proxyDeviceSettings.DisableWindowIdentityPersistence {
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
	return deviceLocalSettings
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

	receivePacketsCallback := func(
		source connect.TransferPath,
		provideMode protocol.ProvideMode,
		ipPath *connect.IpPath,
		packets [][]byte,
	) {
		self.deliverReturnPackets(packets)
	}
	sub := self.deviceLocal.AddReceivePacketsCallback(receivePacketsCallback)
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
		self.deviceLocal.SendPacketsNoCopy(packets[:n])
	}
}

// A callback batch is borrowed for this call. Return packets addressed to the
// WireGuard peer are copied into its receive channel; all other packets stay on
// the private gVisor Tun used by HTTP and SOCKS. The old global mode switch
// could only serve one of those paths at a time: any overlapping Tun dial
// silently stole every WireGuard return packet, while late Tun packets were
// handed to WireGuard. Destination demultiplexing keeps all paths live.
func (self *ProxyDevice) deliverReturnPackets(packets [][]byte) {
	if !self.UpdateActivity() {
		return
	}
	receive, receiveAddr, receiveNotify := self.receiveWithNotify()
	if receive == nil {
		_, _ = self.tun.WriteBatch(packets)
		self.UpdateActivity()
		return
	}

	// Flush every Tun run before a WireGuard handoff can wait for capacity.
	// This keeps a busy process-wide WireGuard queue from delaying HTTP/SOCKS
	// returns on the same device, without allocating a partition slice.
	tunStart := -1
	flushTun := func(end int) {
		if tunStart < 0 {
			return
		}
		_, _ = self.tun.WriteBatch(packets[tunStart:end])
		tunStart = -1
	}
	for i, packet := range packets {
		if proxyPacketMatchesReceiveAddress(packet, receiveAddr) {
			flushTun(i)
			continue
		}
		if tunStart < 0 {
			tunStart = i
		}
	}
	flushTun(len(packets))

	for _, packet := range packets {
		if !proxyPacketMatchesReceiveAddress(packet, receiveAddr) {
			continue
		}
		if !self.deliverWireGuardReturn(receive, receiveNotify, packet) {
			return
		}
	}
	self.UpdateActivity()
}

// deliverWireGuardReturn preserves the device-side Tun loss model: provider
// NAT has already consumed upstream TCP bytes and cannot reconstruct a segment
// dropped here. This callback belongs to one DeviceLocal, so waiting on the
// fixed process queue propagates bounded backpressure only into that device;
// cancellation or an attachment change still releases it immediately.
func observeElapsedSeconds(start time.Time, now func() time.Time, observe func(float64)) {
	observe(now().Sub(start).Seconds())
}

func (self *ProxyDevice) deliverWireGuardReturn(receive chan []byte, receiveNotify chan struct{}, packet []byte) bool {
	sharedPacket := connect.MessagePoolShareReadOnly(packet)
	select {
	case <-self.ctx.Done():
		connect.MessagePoolReturn(sharedPacket)
		return false
	case <-receiveNotify:
		connect.MessagePoolReturn(sharedPacket)
		return false
	case receive <- sharedPacket:
		self.UpdateActivity()
		return true
	default:
	}
	backpressureStart := time.Now()
	proxyWireGuardReturnBackpressureCounter.Inc()
	defer func() {
		observeElapsedSeconds(backpressureStart, time.Now, proxyWireGuardReturnBackpressureDuration.Observe)
	}()
	if self.receiveBackpressureForTest != nil {
		self.receiveBackpressureForTest()
	}
	select {
	case <-self.ctx.Done():
		connect.MessagePoolReturn(sharedPacket)
		return false
	case <-receiveNotify:
		connect.MessagePoolReturn(sharedPacket)
		return false
	case receive <- sharedPacket:
		self.UpdateActivity()
		return true
	}
}

func (self *ProxyDevice) Send(packet []byte) bool {
	if !self.UpdateActivity() {
		return false
	}
	ownedPacket := connect.MessagePoolCopy(packet)
	sent := false
	if self.sendOwnedPacketForTest != nil {
		sent = self.sendOwnedPacketForTest(ownedPacket)
	} else {
		sent = self.deviceLocal.SendPacketNoCopy(ownedPacket, int32(len(ownedPacket)))
	}
	if sent {
		return true
	}
	connect.MessagePoolReturn(ownedPacket)
	return false
}

// Copies one borrowed userwireguard burst into Connect-owned buffers before
// handing it to the asynchronous DeviceLocal group path.
func (self *ProxyDevice) SendBorrowedBatch(packets [][]byte, offset int) int {
	if !self.UpdateActivity() {
		return 0
	}
	ownedPackets := make([][]byte, len(packets))
	for packetIndex, packet := range packets {
		ownedPackets[packetIndex] = connect.MessagePoolCopy(packet[offset:])
	}
	if self.sendOwnedPacketsForTest != nil {
		sentPacketCount := min(
			max(0, self.sendOwnedPacketsForTest(ownedPackets)),
			len(ownedPackets),
		)
		for _, ownedPacket := range ownedPackets[sentPacketCount:] {
			connect.MessagePoolReturn(ownedPacket)
		}
		return sentPacketCount
	}
	// DeviceLocal's batch contract consumes every pooled packet, including
	// members rejected by the selected route.
	return self.deviceLocal.SendPacketsNoCopy(ownedPackets)
}

func (self *ProxyDevice) SetReceive(receive chan []byte) {
	self.SetReceiveForAddress(netip.Addr{}, receive)
}

// SetReceiveForAddress routes return packets for one WireGuard client address
// to receive without disabling the Tun return path. An invalid address retains
// SetReceive's legacy all-packets behavior for non-address-aware callers.
func (self *ProxyDevice) SetReceiveForAddress(receiveAddr netip.Addr, receive chan []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if receive == nil {
		receiveAddr = netip.Addr{}
	}
	if self.receive == receive && self.receiveAddr == receiveAddr {
		// already attached; avoid monitor churn / waking the receive callback
		return
	}
	self.receiveMonitor.NotifyAll()
	self.receive = receive
	self.receiveAddr = receiveAddr
	self.receiveNotify = self.receiveMonitor.NotifyChannel()
}

func (self *ProxyDevice) receiveWithNotify() (chan []byte, netip.Addr, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.receive, self.receiveAddr, self.receiveNotify
}

// proxyPacketMatchesReceiveAddress is allocation-free because it runs once per
// returned packet. IPv4 and IPv6 destination offsets are fixed in their base
// headers; extension headers do not change the IPv6 destination position.
func proxyPacketMatchesReceiveAddress(packet []byte, receiveAddr netip.Addr) bool {
	if !receiveAddr.IsValid() {
		// Backward-compatible SetReceive means the external consumer owns all
		// packets. Production WireGuard always supplies its assigned address.
		return true
	}
	if len(packet) == 0 {
		return false
	}
	var destination []byte
	switch packet[0] >> 4 {
	case 4:
		if len(packet) < 20 {
			return false
		}
		destination = packet[16:20]
	case 6:
		if len(packet) < 40 {
			return false
		}
		destination = packet[24:40]
	default:
		return false
	}
	addr, ok := netip.AddrFromSlice(destination)
	return ok && addr == receiveAddr
}

func (self *ProxyDevice) Tun() *connect.Tun {
	return self.tun
}

// DialContext dials through the device's private Tun. WireGuard return packets
// are independently selected by destination address in Run, so starting a Tun
// connection must not detach or interrupt an active WireGuard peer.
func (self *ProxyDevice) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	return self.tun.DialContext(ctx, network, addr)
}

func (self *ProxyDevice) WaitForReady(ctx context.Context, timeout time.Duration) bool {
	deviceState := self.proxyDeviceState()
	if deviceState == nil {
		return false
	}
	if timeout == 0 {
		windowStatus := deviceState.GetWindowStatus()
		return windowStatus != nil && windowStatus.MinSatisfied
	}

	var timeoutChannel <-chan time.Time
	var timer *time.Timer
	if 0 < timeout {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timeoutChannel = timer.C
	}
	return waitForProxyDeviceReady(
		ctx,
		self.ctx,
		deviceState,
		timeoutChannel,
	)
}

// proxyDeviceWindowStatusSource is the narrow DeviceLocal readiness surface.
// Keeping the wait independent of the concrete device makes every ordering
// edge deterministic to test without a live provider window.
type proxyDeviceWindowStatusSource interface {
	GetWindowStatus() *sdk.WindowStatus
	AddWindowStatusChangeListener(sdk.WindowStatusChangeListener) sdk.Sub
}

// proxyDeviceStateSource distinguishes lifecycle death from a temporarily
// unsatisfied provider window. Only lifecycle death makes a device unusable;
// window readiness is observed by WaitForReady while its refill keeps running.
type proxyDeviceStateSource interface {
	proxyDeviceWindowStatusSource
	GetDone() bool
}

func (self *ProxyDevice) proxyDeviceState() proxyDeviceStateSource {
	if self.deviceState != nil {
		return self.deviceState
	}
	if self.deviceLocal == nil {
		return nil
	}
	return self.deviceLocal
}

// waitForProxyDeviceReady subscribes before reading readiness so a transition
// between those operations is retained by the buffered callback edge. A nil
// timeout channel waits indefinitely; only readiness returns true.
func waitForProxyDeviceReady(
	callerCtx context.Context,
	deviceCtx context.Context,
	windowStatusSource proxyDeviceWindowStatusSource,
	timeoutChannel <-chan time.Time,
) bool {
	ready := make(chan struct{}, 1)
	sub := windowStatusSource.AddWindowStatusChangeListener(&windowStatusChangeListener{
		callback: func(windowStatus *sdk.WindowStatus) {
			if windowStatus != nil && windowStatus.MinSatisfied {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		},
	})
	defer sub.Close()

	windowStatus := windowStatusSource.GetWindowStatus()
	if windowStatus != nil && windowStatus.MinSatisfied {
		return true
	}

	select {
	case <-ready:
		return true
	case <-deviceCtx.Done():
		return false
	case <-callerCtx.Done():
		return false
	case <-timeoutChannel:
		return false
	}
}

// conforms to `sdk.WindowStatusChangeListener`
type windowStatusChangeListener struct {
	callback func(*sdk.WindowStatus)
}

func (self *windowStatusChangeListener) WindowStatusChanged(windowStatus *sdk.WindowStatus) {
	self.callback(windowStatus)
}

// Active reports whether both owning lifecycles remain live. Window readiness
// is deliberately not a liveness gate: quality/rotation/provider loss can make
// MinSatisfied false temporarily, and the multi-window must keep refilling
// forever under the same hosted device. Recreating it on the next request
// cancels that retry machinery and creates the production recreation loop.
func (self *ProxyDevice) Active() bool {
	if self.ctx == nil {
		return false
	}
	select {
	case <-self.ctx.Done():
		return false
	default:
	}
	deviceState := self.proxyDeviceState()
	return deviceState != nil && !deviceState.GetDone()
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
	if self.cancel != nil {
		self.cancel()
	}

	if self.deviceLocal != nil {
		self.deviceLocal.Close()
	}
	var closeErr error
	if self.tun != nil {
		closeErr = self.tun.Close()
	}
	return closeErr
}
