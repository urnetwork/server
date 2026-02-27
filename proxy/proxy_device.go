package main

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/proxy"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
)

func DefaultProxyDeviceManagerSettings() *ProxyDeviceManagerSettings {
	return &ProxyDeviceManagerSettings{
		CheckProxyDeviceIdleTimeout: 1 * time.Minute,
	}
}

type ProxyDeviceManagerSettings struct {
	CheckProxyDeviceIdleTimeout time.Duration
}

type ProxyDeviceManager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *ProxyDeviceManagerSettings

	networkSpace *sdk.NetworkSpace

	stateLock    sync.Mutex
	proxyDevices map[server.Id]*ProxyDevice
}

func NewProxyDeviceManagerWithDefaults(ctx context.Context) *ProxyDeviceManager {
	return NewProxyDeviceManager(ctx, DefaultProxyDeviceManagerSettings())
}

func NewProxyDeviceManager(ctx context.Context, settings *ProxyDeviceManagerSettings) *ProxyDeviceManager {
	cancelCtx, cancel := context.WithCancel(ctx)

	// share one network space across all clients
	// this reuses the client strategy and keep alive connections
	connectSettings := connect.DefaultConnectSettings()
	// FIXME use only ipv4 when communicating back to the platform
	connectSettings.DisableIpv6 = true
	networkSpace := sdk.NewPlatformNetworkSpace(
		cancelCtx,
		server.RequireEnv(),
		server.RequireDomain(),
		connectSettings,
	)

	return &ProxyDeviceManager{
		ctx:          cancelCtx,
		cancel:       cancel,
		settings:     settings,
		networkSpace: networkSpace,
		proxyDevices: map[server.Id]*ProxyDevice{},
	}
}

func (self *ProxyDeviceManager) OpenProxyDevice(proxyId server.Id) (*ProxyDevice, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	nextProxyDevice := func() (*ProxyDevice, error) {
		proxyDeviceConfig := model.GetProxyDeviceConfig(self.ctx, proxyId)
		if proxyDeviceConfig == nil {
			return nil, fmt.Errorf("Proxy device does not exist.")
		}

		pd, err := NewProxyDeviceWithDefaults(self.ctx, proxyDeviceConfig, self.networkSpace)
		if err != nil {
			return nil, err
		}

		go server.HandleError(func() {
			defer func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				glog.Infof("[pd]cancel")
				// note we don't call close here because only the sender should call close
				pd.Cancel()
				if currentPd := self.proxyDevices[proxyId]; pd == currentPd {
					delete(self.proxyDevices, proxyId)
				}
			}()
			pd.Run()
		})

		go server.HandleError(func() {
			defer self.cancel()
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

	if pd, ok := self.proxyDevices[proxyId]; ok && pd.UpdateActivity() {
		return pd, nil
	}

	pd, err := nextProxyDevice()
	if err != nil {
		return nil, err
	}

	if replacedPd, ok := self.proxyDevices[proxyId]; ok {
		replacedPd.Cancel()
	}
	self.proxyDevices[proxyId] = pd

	return pd, nil
}

func (self *ProxyDeviceManager) ValidCaller(proxyId server.Id, addr netip.Addr) bool {
	// FIXME
	return true
}

func (self *ProxyDeviceManager) Close() {
	self.cancel()
}

func DefaultProxyDeviceSettings() *ProxyDeviceSettings {
	return &ProxyDeviceSettings{
		ProxyDeviceDescription: "resident proxy",
		ProxyDeviceSpec:        "resident proxy",
		Mtu:                    1440,
		ProxyDeviceIdleTimeout: 90 * time.Minute,
	}
}

type ProxyDeviceSettings struct {
	ProxyDeviceDescription         string
	ProxyDeviceSpec                string
	IngressSecurityPolicyGenerator func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	EgressSecurityPolicyGenerator  func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	Mtu                            int
	ProxyDeviceIdleTimeout         time.Duration
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

	tunSettings := proxy.DefaultTunSettings()
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
					return
				case <-notify:
				}
			} else {
				self.tun.Write(packet)
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
		success := self.deviceLocal.SendPacketNoCopy(packet, int32(len(packet)))
		if !success {
			connect.MessagePoolReturn(packet)
		}
	}
}

func (self *ProxyDevice) Send(packet []byte) bool {
	return self.deviceLocal.SendPacketNoCopy(packet, int32(len(packet)))
}

func (self *ProxyDevice) SetReceive(receive chan []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
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

func (self *ProxyDevice) WaitForReady(ctx context.Context, timeout time.Duration) bool {
	readyCtx, readyCancel := context.WithCancel(self.ctx)
	defer readyCancel()
	go server.HandleError(func() {
		defer readyCancel()
		select {
		case <-self.ctx.Done():
		case <-ctx.Done():
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

func (self *ProxyDevice) Close() {
	self.cancel()

	self.deviceLocal.Close()
	self.tun.Close()
}
