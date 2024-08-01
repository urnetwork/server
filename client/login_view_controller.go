package client

import (
	"context"
	"sync"
	"time"

	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
)

var lvcLog = logFn("login_view_controller")

const defaultNetworkCheckTimeout = 5 * time.Second

type LoginViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	api *BringYourApi

	networkCheck *networkCheck

	glViewController
}

func NewLoginViewController(api *BringYourApi) *LoginViewController {
	return newLoginViewControllerWithContext(context.Background(), api)
}

func newLoginViewControllerWithContext(ctx context.Context, api *BringYourApi) *LoginViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &LoginViewController{
		ctx:              cancelCtx,
		cancel:           cancel,
		api:              api,
		networkCheck:     newNetworkCheck(cancelCtx, api, defaultNetworkCheckTimeout),
		glViewController: *newGLViewController(),
	}
	vc.drawController = vc
	return vc
}

func (self *LoginViewController) Start() {
	// FIXME
}

func (self *LoginViewController) Stop() {
	// FIXME
}

func (self *LoginViewController) draw(g gl.Context) {
	// lvcLog("draw")

	g.ClearColor(self.bgRed, self.bgGreen, self.bgBlue, 1.0)
	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
}

func (self *LoginViewController) drawLoopOpen() {
	self.frameRate = 24
}

func (self *LoginViewController) drawLoopClose() {
}

func (self *LoginViewController) Close() {
	lvcLog("close")

	self.cancel()
}

func (self *LoginViewController) NetworkCheck(networkName string, callback NetworkCheckCallback) {
	self.networkCheck.Queue(networkName, callback)
}

type networkCheck struct {
	ctx    context.Context
	cancel context.CancelFunc

	api *BringYourApi

	timeout time.Duration

	stateLock sync.Mutex

	monitor *connect.Monitor

	updateCount int
	networkName string
	callback    NetworkCheckCallback
}

func newNetworkCheck(
	ctx context.Context,
	api *BringYourApi,
	timeout time.Duration,
) *networkCheck {
	cancelCtx, cancel := context.WithCancel(ctx)
	networkCheck := &networkCheck{
		ctx:         cancelCtx,
		cancel:      cancel,
		api:         api,
		timeout:     timeout,
		stateLock:   sync.Mutex{},
		monitor:     connect.NewMonitor(),
		updateCount: 0,
	}
	go networkCheck.run()
	return networkCheck
}

func (self *networkCheck) run() {
	for {
		self.stateLock.Lock()
		notify := self.monitor.NotifyChannel()
		networkName := self.networkName
		updateCount := self.updateCount
		callback := self.callback
		self.stateLock.Unlock()

		if 0 < updateCount {
			done := make(chan struct{})

			self.api.NetworkCheck(
				&NetworkCheckArgs{
					NetworkName: networkName,
				},
				newApiCallback[*NetworkCheckResult](func(result *NetworkCheckResult, err error) {
					self.stateLock.Lock()
					head := (updateCount == self.updateCount)
					self.stateLock.Unlock()
					if head {
						callback.Result(result, err)
					}
					close(done)
				}),
			)

			select {
			case <-self.ctx.Done():
				return
			case <-done:
				// continue
			case <-time.After(self.timeout):
				// continue
			}
		}

		select {
		case <-self.ctx.Done():
			return
		case <-notify:
		}
	}
}

func (self *networkCheck) Queue(networkName string, callback NetworkCheckCallback) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.updateCount += 1
	self.networkName = networkName
	self.callback = callback
	self.monitor.NotifyAll()
}

func (self *networkCheck) Close() {
	self.cancel()
}
