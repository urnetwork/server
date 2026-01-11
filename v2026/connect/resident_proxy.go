package main

// FIXME have a multi client in the proxy, set proxy as the generator

import (
	"context"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
)

const ProxyDeviceDescription = "resident proxy"
const ProxyDeviceSpec = "resident proxy"

type ResidentProxyDevice struct {
	ctx    context.Context
	cancel context.CancelFunc

	exchange          *Exchange
	clientId          server.Id
	instanceId        server.Id
	proxyDeviceConfig *model.ProxyDeviceConfig

	deviceLocal *sdk.DeviceLocal
}

func NewResidentProxyDevice(
	ctx context.Context,
	exchange *Exchange,
	clientId server.Id,
	instanceId server.Id,
	proxyDeviceConfig *model.ProxyDeviceConfig,
) (*ResidentProxyDevice, error) {

	// this jwt is used to access the services in the network space
	byJwt, err := jwt.LoadByJwtFromClientId(ctx, clientId)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(ctx)

	networkSpace := newExchangeNetworkSpace(exchange)

	generatorFunc := func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		return newExchangeGenerator(
			cancelCtx,
			exchange,
			byJwt,
			specs,
			[]server.Id{clientId},
			clientId,
			connect.DefaultClientSettings,
		)
	}

	deviceLocal, err := sdk.NewPlatformDeviceLocalWithDefaults(
		generatorFunc,
		networkSpace,
		byJwt.Sign(),
		ProxyDeviceDescription,
		ProxyDeviceSpec,
		server.RequireVersion(),
		sdk.RequireIdFromBytes(instanceId.Bytes()),
	)
	if err != nil {
		return nil, err
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
	}

	return proxyDevice, nil
}

func (self *ResidentProxyDevice) AddTun() (
	send chan []byte,
	receive chan []byte,
	closeTun func(),
) {
	send = make(chan []byte, self.exchange.settings.ExchangeBufferSize)
	receive = make(chan []byte, self.exchange.settings.ExchangeBufferSize)

	tunCtx, tunCancel := context.WithCancel(self.ctx)

	server.HandleError(func() {
		defer tunCancel()
		for {
			select {
			case <-tunCtx.Done():
				return
			case packet := <-receive:
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

func newExchangeNetworkSpace(exchange *Exchange) *sdk.NetworkSpace {
	// FIXME
	return nil
}
