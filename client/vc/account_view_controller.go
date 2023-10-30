package vc

import (
	"context"

	"bringyour.com/connect"

	"bringyour.com/client"
)


var avcLog = client.LogFn("account_view_controller")


type AccountViewController struct {
	ctx context.Context
	cancel context.CancelFunc

	client *connect.Client
}

func NewAccountViewController(ctx context.Context, client *connect.Client) *AccountViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	vc := &AccountViewController{
		ctx: cancelCtx,
		cancel: cancel,
	}
	return vc
}

func (self *AccountViewController) Close() {
	avcLog("close")

	self.cancel()
}
