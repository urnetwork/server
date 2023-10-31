package client

import (
	"context"

	"bringyour.com/connect"
)


var dvcLog = logFn("device_view_controller")


type DeviceViewController struct {
	ctx context.Context
	cancel context.CancelFunc

	client *connect.Client
}

func newDeviceViewController(ctx context.Context, client *connect.Client) *DeviceViewController {
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
