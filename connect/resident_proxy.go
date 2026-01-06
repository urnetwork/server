package main

// FIXME have a multi client in the proxy, set proxy as the generator


import (
	"context"

	"github.com/urnetwork/sdk"

)

type ResidentProxyDevice struct {
	ctx context.Context
	cancel context.CancelFun

	deviceLocal *sdk.DeviceLocal


}

func CreateResidentProxyDevice(
	ctx context.Context,
	exchange *Exchange,
	clientId server.Id,
	instanceId server.Id,
) *ResidentProxyDevice {

	// FIXME create multi client
	// FIXME no local user nat

	generator := newExchangeGenerator(ctx, exchange)

	networkSpace := newExchangeNetworkSpace(ctx, exchange)
	// this jwt is used to access the services in the network space
	byJwt := CREATEJWT(clientId)

	sdk.NewPlatformDeviceLocalWithDefaults(
		generator,
		networkSpace,
		byJwt,
		deviceDescription,
		deviceSpec,
		appVersion,
		sdk.IdFromBytes(instanceId.Bytes()),
	)

}

// FIXME
func (self *ResidentProxyDevice) AddTun() (
	send chan []byte,
	receive chan []byte,
	closeTun func(),
) func() {
	tunCtx, tunCancel := context.WithCancel(ctx)

	for {
		select {
		case packet := <- receive:
			self.deviceLocal.Write(packet)
			RELEASE(packet)

		}
	}

	unsub := device.AddPacketReceiveListener(func(packet []byte) {
		select {
		case send <- packet:
		}
	})

	closeTun = func() {
		tunCancel()
		unsub()

		// note `send` is not closed. This channel is left open.
	}
}

func (self *ResidentProxyDevice) Close() {
	self.cancel()

	self.deviceLocal.Close()
}

