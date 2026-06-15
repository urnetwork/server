package proxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
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

	stateLock    sync.Mutex
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
	nextProxyDevice := func() (*ProxyDevice, error) {
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
				self.stateLock.Lock()
				defer self.stateLock.Unlock()

				pd.Close()
				if pdState, ok := self.proxyDevices[proxyId]; ok {
					func() {
						pdState.StateLock.Lock()
						defer pdState.StateLock.Unlock()

						if pd == pdState.ProxyDevice {
							delete(self.proxyDevices, proxyId)
						}
					}()
				}
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

	pdState := func() *proxyDeviceState {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		pdState, ok := self.proxyDevices[proxyId]
		if !ok {
			pdState = &proxyDeviceState{}
			self.proxyDevices[proxyId] = pdState
		}
		return pdState
	}()

	pdState.StateLock.Lock()
	defer pdState.StateLock.Unlock()

	if pd := pdState.ProxyDevice; pd != nil {
		if pd.Active() {
			pd.UpdateActivity()
			return pd, nil
		} else {
			pd.Cancel()
			pdState.ProxyDevice = nil
		}
	}

	pd, err := nextProxyDevice()
	if err != nil {
		return nil, err
	}

	pdState.ProxyDevice = pd
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

	stateLock        sync.Mutex
	lastActivityTime time.Time
	// set once the egress window has been satisfied at least once. A device that
	// was ready and has since lost its window is dead and must be recreated; a
	// device that has never been ready is still warming up and is kept.
	everReady bool

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

	var dnsResolverSettings *connect.DnsResolverSettings
	if initialDeviceState := proxyDeviceConfig.InitialDeviceState; initialDeviceState != nil {
		deviceLocal.SetPerformanceProfile(initialDeviceState.PerformanceProfile)
		deviceLocal.SetConnectLocation(initialDeviceState.Location)
		dnsResolverSettings = initialDeviceState.DnsResolverSettings
	}

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
		lastActivityTime:  time.Now(),
		receiveMonitor:    connect.NewMonitor(),
	}

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
				select {
				case <-self.ctx.Done():
					return
				case receive <- packet:
					self.UpdateActivity()
					return
				case <-notify:
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
// device that can no longer carry traffic — and because each reuse bumps
// lastActivityTime, the idle timer would never fire to recycle it. Gating reuse
// on the actual egress window lets OpenProxyDevice recreate the device instead.
func (self *ProxyDevice) Active() bool {
	select {
	case <-self.ctx.Done():
		return false
	default:
	}

	// read window status outside stateLock (DeviceLocal has its own lock)
	minSatisfied := self.deviceLocal.GetWindowStatus().MinSatisfied

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if minSatisfied {
		self.everReady = true
		return true
	}
	// keep a device that has not yet had a chance to connect; only a device that
	// was ready and lost its window is treated as dead
	return !self.everReady
}

func (self *ProxyDevice) UpdateActivity() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	select {
	case <-self.ctx.Done():
		return false
	default:
		self.lastActivityTime = time.Now()
		return true
	}
}

func (self *ProxyDevice) CancelIfIdle() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	idleTimeout := time.Now().Sub(self.lastActivityTime)
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
