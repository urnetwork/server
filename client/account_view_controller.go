package client

import (
	"context"
	"sync"
	"time"

	"bringyour.com/connect"
)

var avcLog = logFn("account_view_controller")

const defaultAccountCheckTimeout = 5 * time.Second

type AccountViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device *BringYourDevice

	walletValidateAddress *walletValidateAddress
}

func newAccountViewController(ctx context.Context, device *BringYourDevice) *AccountViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	vc := &AccountViewController{
		ctx:                   cancelCtx,
		cancel:                cancel,
		device:                device,
		walletValidateAddress: newWalletValidateAddress(cancelCtx, device.Api(), defaultAccountCheckTimeout),
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

func (self *AccountViewController) WalletValidateAddress(address string, callback WalletValidateAddressCallback) {
	self.walletValidateAddress.Queue(address, callback)
}

type walletValidateAddress struct {
	ctx    context.Context
	cancel context.CancelFunc

	api *BringYourApi

	timeout time.Duration

	stateLock sync.Mutex

	monitor *connect.Monitor

	updateCount int
	address     string
	chain       string
	callback    WalletValidateAddressCallback
}

func newWalletValidateAddress(
	ctx context.Context,
	api *BringYourApi,
	timeout time.Duration,
) *walletValidateAddress {
	cancelCtx, cancel := context.WithCancel(ctx)
	walletValidateAddress := &walletValidateAddress{
		ctx:         cancelCtx,
		cancel:      cancel,
		api:         api,
		timeout:     timeout,
		stateLock:   sync.Mutex{},
		monitor:     connect.NewMonitor(),
		updateCount: 0,
	}
	go walletValidateAddress.run()
	return walletValidateAddress
}

func (self *walletValidateAddress) run() {
	for {
		self.stateLock.Lock()
		notify := self.monitor.NotifyChannel()
		address := self.address
		chain := self.chain
		updateCount := self.updateCount
		callback := self.callback
		self.stateLock.Unlock()

		if 0 < updateCount {
			done := make(chan struct{})

			self.api.WalletValidateAddress(
				&WalletValidateAddressArgs{
					Address: address,
					Chain:   chain,
				},
				newApiCallback[*WalletValidateAddressResult](func(result *WalletValidateAddressResult, err error) {
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

func (self *walletValidateAddress) Queue(address string, callback WalletValidateAddressCallback) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.updateCount += 1
	self.address = address
	self.callback = callback
	self.monitor.NotifyAll()
}

func (self *walletValidateAddress) Close() {
	self.cancel()
}
