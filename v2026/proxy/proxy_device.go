package proxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/proxy/v2026"
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

	// NetworkSpace, when set, overrides the default platform network space.
	// Integration tests use this to point proxy devices at local api/connect
	// servers (see sdk.Testing_NewNetworkSpaceWithUrls).
	NetworkSpace *sdk.NetworkSpace
}

type ProxyDeviceManager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *ProxyDeviceManagerSettings

	networkSpace *sdk.NetworkSpace

	stateLock    sync.Mutex
	proxyDevices map[server.Id]*proxyDeviceState
}

func NewProxyDeviceManagerWithDefaults(ctx context.Context) *ProxyDeviceManager {
	return NewProxyDeviceManager(ctx, DefaultProxyDeviceManagerSettings())
}

func NewProxyDeviceManager(ctx context.Context, settings *ProxyDeviceManagerSettings) *ProxyDeviceManager {
	cancelCtx, cancel := context.WithCancel(ctx)

	// share one network space across all clients
	// this reuses the client strategy and keep alive connections
	networkSpace := settings.NetworkSpace
	if networkSpace == nil {
		connectSettings := connect.DefaultConnectSettings()
		// FIXME use only ipv4 when communicating back to the platform
		connectSettings.DisableIpv6 = true
		networkSpace = sdk.NewPlatformNetworkSpace(
			cancelCtx,
			server.RequireEnv(),
			server.RequireDomain(),
			connectSettings,
		)
	}

	return &ProxyDeviceManager{
		ctx:          cancelCtx,
		cancel:       cancel,
		settings:     settings,
		networkSpace: networkSpace,
		proxyDevices: map[server.Id]*proxyDeviceState{},
	}
}

func (self *ProxyDeviceManager) OpenProxyDevice(proxyId server.Id) (*ProxyDevice, error) {
	nextProxyDevice := func() (*ProxyDevice, error) {
		proxyDeviceConfig := model.GetProxyDeviceConfig(self.ctx, proxyId)
		if proxyDeviceConfig == nil {
			return nil, fmt.Errorf("Proxy device does not exist.")
		}

		settings := DefaultProxyDeviceSettingsWithBufferSize(self.settings.SequenceBufferSize)
		pd, err := NewProxyDevice(self.ctx, proxyDeviceConfig, self.networkSpace, settings)
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
		if pd.UpdateActivity() {
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
	tun         *proxy.Tun
	settings    *ProxyDeviceSettings

	stateLock        sync.Mutex
	lastActivityTime time.Time

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

	deviceLocal, err := sdk.NewPlatformDeviceLocalWithDefaults(
		nil,
		networkSpace,
		byJwt.Sign(),
		settings.ProxyDeviceDescription,
		settings.ProxyDeviceSpec,
		server.RequireVersion(),
		sdk.RequireIdFromBytes(proxyDeviceConfig.InstanceId.Bytes()),
	)
	if err != nil {
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

	tunSettings := proxy.DefaultTunSettingsWithBufferSize(settings.SequenceBufferSize)
	tunSettings.Mtu = settings.Mtu

	tun, err := proxy.CreateTunWithResolver(
		cancelCtx,
		tunSettings,
		dnsResolverSettings,
	)
	if err != nil {
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

	for {
		if !self.UpdateActivity() {
			return
		}
		packet, err := self.tun.Read()
		if err != nil {
			return
		}
		if !self.UpdateActivity() {
			return
		}
		success := self.deviceLocal.SendPacketNoCopy(packet, int32(len(packet)))
		if !success {
			connect.MessagePoolReturn(packet)
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

func (self *ProxyDevice) Tun() *proxy.Tun {
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
