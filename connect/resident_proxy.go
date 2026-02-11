package main

// FIXME have a multi client in the proxy, set proxy as the generator

import (
	"context"
	// "fmt"
	"net"
	// "time"
	// "sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/proxy"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
)

func DefaultResidentProxyDeviceSettings() *ResidentProxyDeviceSettings {
	return &ResidentProxyDeviceSettings{
		ProxyDeviceDescription: "resident proxy",
		ProxyDeviceSpec:        "resident proxy",
		Mtu:                    1440,
	}
}

type ResidentProxyDeviceSettings struct {
	ProxyDeviceDescription         string
	ProxyDeviceSpec                string
	IngressSecurityPolicyGenerator func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	EgressSecurityPolicyGenerator  func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	Mtu                            int
}

type ResidentProxyDevice struct {
	ctx    context.Context
	cancel context.CancelFunc

	exchange          *Exchange
	clientId          server.Id
	instanceId        server.Id
	proxyDeviceConfig *model.ProxyDeviceConfig

	deviceLocal *sdk.DeviceLocal
	tnet        *proxy.Net
	settings    *ResidentProxyDeviceSettings
}

func NewResidentProxyDeviceWithDefaults(
	ctx context.Context,
	exchange *Exchange,
	clientId server.Id,
	instanceId server.Id,
	proxyDeviceConfig *model.ProxyDeviceConfig,
) (*ResidentProxyDevice, error) {
	return NewResidentProxyDevice(
		ctx,
		exchange,
		clientId,
		instanceId,
		proxyDeviceConfig,
		DefaultResidentProxyDeviceSettings(),
	)
}

func NewResidentProxyDevice(
	ctx context.Context,
	exchange *Exchange,
	clientId server.Id,
	instanceId server.Id,
	proxyDeviceConfig *model.ProxyDeviceConfig,
	settings *ResidentProxyDeviceSettings,
) (*ResidentProxyDevice, error) {
	glog.Infof("[rp]create")

	// this jwt is used to access the services in the network space
	byJwt, err := jwt.LoadByJwtFromClientId(ctx, clientId)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(ctx)

	networkSpace := sdk.NewPlatformNetworkSpace(
		cancelCtx,
		server.RequireEnv(),
		server.RequireDomain(),
	)

	generatorFunc := func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		return newExchangeGenerator(
			cancelCtx,
			exchange,
			byJwt,
			specs,
			[]server.Id{clientId},
			clientId,
			connect.DefaultClientSettings,
			settings,
		)
	}

	deviceLocal, err := sdk.NewPlatformDeviceLocalWithDefaults(
		generatorFunc,
		networkSpace,
		byJwt.Sign(),
		settings.ProxyDeviceDescription,
		settings.ProxyDeviceSpec,
		server.RequireVersion(),
		sdk.RequireIdFromBytes(instanceId.Bytes()),
	)
	if err != nil {
		return nil, err
	}
	deviceLocal.SetIngressSecurityPolicyGenerator(settings.IngressSecurityPolicyGenerator)
	deviceLocal.SetEgressSecurityPolicyGenerator(settings.EgressSecurityPolicyGenerator)

	if initialDeviceState := proxyDeviceConfig.InitialDeviceState; initialDeviceState != nil {
		deviceLocal.SetPerformanceProfile(initialDeviceState.PerformanceProfile)
		deviceLocal.SetConnectLocation(initialDeviceState.Location)
	}

	tnet, err := proxy.CreateNetTun(
		cancelCtx,
		settings.Mtu,
	)
	if err != nil {
		return nil, err
	}

	proxyDevice := &ResidentProxyDevice{
		ctx:               cancelCtx,
		cancel:            cancel,
		exchange:          exchange,
		clientId:          clientId,
		proxyDeviceConfig: proxyDeviceConfig,
		deviceLocal:       deviceLocal,
		tnet:              tnet,
		settings:          settings,
	}
	go server.HandleError(proxyDevice.run)

	return proxyDevice, nil
}

// directly copy between tnet and device
func (self *ResidentProxyDevice) run() {
	go server.HandleError(func() {
		defer self.cancel()
		for {
			select {
			case <-self.ctx.Done():
				return
			default:
			}
			packet, err := self.tnet.Read()
			if err != nil {
				return
			}
			success := self.deviceLocal.SendPacketNoCopy(packet, int32(len(packet)))
			if !success {
				connect.MessagePoolReturn(packet)
			}
		}
	})

	// note the packet is only retained for the duration of the callback
	// use `MessagePoolShareReadOnly` to share it outside of the callback
	receiveCallback := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		self.tnet.Write(packet)
	}
	sub := self.deviceLocal.AddReceivePacketCallback(receiveCallback)
	defer sub()

	select {
	case <-self.ctx.Done():
	}
}

func (self *ResidentProxyDevice) AddTun(header ExchangeHeader) (
	send chan []byte,
	receive chan []byte,
	closeTun func(),
	returnErr error,
) {
	tunCtx, tunCancel := context.WithCancel(self.ctx)

	var conn net.Conn
	conn, returnErr = self.tnet.DialContext(tunCtx, header.TunDial.Network, header.TunDial.Address)
	if returnErr != nil {
		glog.V(1).Infof("[tun]connect (%s, %s) err=%s\n", header.TunDial.Network, header.TunDial.Address, returnErr)
		return
	}
	glog.V(1).Infof("[tun]connect (%s, %s) success\n", header.TunDial.Network, header.TunDial.Address)

	send = make(chan []byte, self.exchange.settings.ExchangeBufferSize)
	receive = make(chan []byte, self.exchange.settings.ExchangeBufferSize)

	go server.HandleError(func() {
		defer func() {
			tunCancel()
			close(send)
		}()
		for {
			packet := connect.MessagePoolGet(self.settings.Mtu)
			n, err := conn.Read(packet)
			if err != nil {
				glog.V(1).Infof("[tun]connect (%s, %s) read err=%s\n", header.TunDial.Network, header.TunDial.Address, err)
				return
			}

			glog.V(1).Infof("[tun]connect (%s, %s) read %d\n", header.TunDial.Network, header.TunDial.Address, n)

			select {
			case <-tunCtx.Done():
				connect.MessagePoolReturn(packet)
				return
			case send <- packet[:n]:
			}
		}
	})

	go server.HandleError(func() {
		defer tunCancel()
		for {
			select {
			case <-tunCtx.Done():
				return
			case packet, ok := <-receive:
				if !ok {
					return
				}
				n := len(packet)
				_, err := conn.Write(packet)
				connect.MessagePoolReturn(packet)
				if err != nil {
					glog.V(1).Infof("[tun]connect (%s, %s) write err=%s\n", header.TunDial.Network, header.TunDial.Address, err)
					return
				}
				glog.V(1).Infof("[tun]connect (%s, %s) write %d\n", header.TunDial.Network, header.TunDial.Address, n)
			}
		}
	})

	closeTun = func() {
		tunCancel()
		conn.Close()
	}
	return
}

func (self *ResidentProxyDevice) Close() {
	self.cancel()

	self.deviceLocal.Close()
	self.tnet.Close()
}

// func newExchangeNetworkSpace(exchange *Exchange) *sdk.NetworkSpace {
// 	// FIXME
// 	return nil

// }
