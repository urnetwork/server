package client

import (
	"context"

	"bringyour.com/connect"
)


var avcLog = logFn("account_view_controller")


type AccountViewController struct {
	ctx context.Context
	cancel context.CancelFunc

	client *connect.Client
}

func newAccountViewController(ctx context.Context, client *connect.Client) *AccountViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	vc := &AccountViewController{
		ctx: cancelCtx,
		cancel: cancel,
	}
	return vc
}

func (self *AccountViewController) Start() {
	// FIXME
}

func (self *AccountViewController) Stop() {
	// FIXME	
}

func (self *AccountViewController) Close() {
	avcLog("close")

	self.cancel()
}


// FIXME validate wallet address callback and queue. similar to network name check
// FIXME add all the wallet apis to api.go
