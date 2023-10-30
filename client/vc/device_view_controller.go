package vc

import (
	"context"

	"bringyour.com/connect"

	"bringyour.com/client"
)


var dvcLog = client.LogFn("device_view_controller")


type DeviceViewController struct {
	ctx context.Context
	cancel context.CancelFunc

	client *connect.Client
}

func NewDeviceViewController(ctx context.Context, client *connect.Client) *DeviceViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &DeviceViewController{
		ctx: cancelCtx,
		cancel: cancel,
		client: client,
	}
	return vc
}

func (self *DeviceViewController) Close() {
	avcLog("close")

	self.cancel()
}
