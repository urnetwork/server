package client

import (
	"context"
	"time"
	// "golang.org/x/mobile/gl"
)

var lvcLog = logFn("login_view_controller")

const defaultNetworkCheckTimeout = 5 * time.Second

type LoginViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	api *BringYourApi

	// networkCheck *networkCheck

	// glViewController
}

func NewLoginViewController(api *BringYourApi) *LoginViewController {
	return newLoginViewControllerWithContext(context.Background(), api)
}

func newLoginViewControllerWithContext(ctx context.Context, api *BringYourApi) *LoginViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &LoginViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		api:    api,
	}
	// vc.drawController = vc
	return vc
}

func (self *LoginViewController) Start() {
	// FIXME
}

func (self *LoginViewController) Stop() {
	// FIXME
}

func (self *LoginViewController) Close() {
	lvcLog("close")

	self.cancel()
}
