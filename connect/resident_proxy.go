package main

// FIXME have a multi client in the proxy, set proxy as the generator

import (
	"context"
	// "fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
)

func DefaultResidentProxyDeviceSettings() *ResidentProxyDeviceSettings {
	return &ResidentProxyDeviceSettings{
		ProxyDeviceDescription: "resident proxy",
		ProxyDeviceSpec:        "resident proxy",
	}
}

type ResidentProxyDeviceSettings struct {
	ProxyDeviceDescription         string
	ProxyDeviceSpec                string
	IngressSecurityPolicyGenerator func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	EgressSecurityPolicyGenerator  func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
}

type ResidentProxyDevice struct {
	ctx    context.Context
	cancel context.CancelFunc

	exchange          *Exchange
	clientId          server.Id
	instanceId        server.Id
	proxyDeviceConfig *model.ProxyDeviceConfig

	deviceLocal *sdk.DeviceLocal
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

	networkSpace := sdk.NewPlatformNetworkSpace(ctx, server.RequireEnv(), server.RequireHost())

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
	if settings.IngressSecurityPolicyGenerator != nil {
		deviceLocal.SetIngressSecurityPolicyGenerator(settings.IngressSecurityPolicyGenerator)
	}
	if settings.EgressSecurityPolicyGenerator != nil {
		deviceLocal.SetEgressSecurityPolicyGenerator(settings.EgressSecurityPolicyGenerator)
	}

	if initialDeviceState := proxyDeviceConfig.InitialDeviceState; initialDeviceState != nil {
		deviceLocal.SetPerformanceProfile(initialDeviceState.PerformanceProfile)
		deviceLocal.SetConnectLocation(initialDeviceState.Location)
	}

	proxyDevice := &ResidentProxyDevice{
		ctx:               cancelCtx,
		cancel:            cancel,
		exchange:          exchange,
		clientId:          clientId,
		proxyDeviceConfig: proxyDeviceConfig,
		deviceLocal:       deviceLocal,
		settings:          settings,
	}

	return proxyDevice, nil
}

func (self *ResidentProxyDevice) AddTun() (
	send chan []byte,
	receive chan []byte,
	closeTun func(),
) {
	glog.Infof("[rp]add tun")

	send = make(chan []byte, self.exchange.settings.ExchangeBufferSize)
	receive = make(chan []byte, self.exchange.settings.ExchangeBufferSize)

	tunCtx, tunCancel := context.WithCancel(self.ctx)

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
				self.deviceLocal.SendPacketNoCopy(packet, int32(len(packet)))
			case <-time.After(self.exchange.settings.WriteTimeout):
				// drop
			}
		}
	})

	receiveCallback := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		select {
		case <-tunCtx.Done():
			return
		case send <- packet:
		case <-time.After(self.exchange.settings.WriteTimeout):
		}
	}
	unsub := self.deviceLocal.AddReceivePacketCallback(receiveCallback)

	closeTun = func() {
		tunCancel()
		unsub()

		// note `send` is not closed. This channel is left open.
	}

	return
}

func (self *ResidentProxyDevice) Close() {
	self.cancel()

	self.deviceLocal.Close()
}

// func newExchangeNetworkSpace(exchange *Exchange) *sdk.NetworkSpace {
// 	// FIXME
// 	return nil

// }
