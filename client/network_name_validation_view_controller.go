package client

import (
	"context"
	"sync"
	"time"

	"bringyour.com/connect"
)

type NetworkNameValidationViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	api *BringYourApi

	// device *BringYourDevice

	stateLock sync.Mutex

	isValidating bool
	networkCheck *networkCheck
}

func NewNetworkNameValidationViewController(api *BringYourApi) *NetworkNameValidationViewController {
	return newNetworkNameValidationViewController(context.Background(), api)
}

func newNetworkNameValidationViewController(ctx context.Context, api *BringYourApi) *NetworkNameValidationViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	vc := &NetworkNameValidationViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		api:    api,

		isValidating: false,
		networkCheck: newNetworkCheck(cancelCtx, api, defaultNetworkCheckTimeout),
	}
	return vc
}

func (self *NetworkNameValidationViewController) Start() {
	// FIXME
}

func (self *NetworkNameValidationViewController) Stop() {
	// FIXME
}

func (self *NetworkNameValidationViewController) Close() {
	lvcLog("close")

	self.cancel()
}

func (self *NetworkNameValidationViewController) NetworkCheck(networkName string, callback NetworkCheckCallback) {
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
	go connect.HandleError(networkCheck.run)
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
				connect.NewApiCallback[*NetworkCheckResult](func(result *NetworkCheckResult, err error) {
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
