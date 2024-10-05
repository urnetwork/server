package client

import (
	"context"
	"sync"

	"bringyour.com/connect"
)

var referralCodeLog = logFn("referral_code_view_controller")

type ReferralCodeListener interface {
	ReferralCodeUpdated(string)
}

type ReferralCodeViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	stateLock sync.Mutex

	isFetching bool

	device *BringYourDevice

	referralCodeListeners *connect.CallbackList[ReferralCodeListener]
}

func newReferralCodeViewController(ctx context.Context, device *BringYourDevice) *ReferralCodeViewController {

	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ReferralCodeViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		isFetching: false,

		referralCodeListeners: connect.NewCallbackList[ReferralCodeListener](),
	}
	return vc
}

func (self *ReferralCodeViewController) AddReferralCodeListener(listener ReferralCodeListener) Sub {
	callbackId := self.referralCodeListeners.Add(listener)
	return newSub(func() {
		self.referralCodeListeners.Remove(callbackId)
	})
}

func (self *ReferralCodeViewController) referralCodeChanged(code string) {
	for _, listener := range self.referralCodeListeners.Get() {
		connect.HandleError(func() {
			listener.ReferralCodeUpdated(code)
		})
	}
}

func (self *ReferralCodeViewController) Start() {
	go self.fetchNetworkReferralCode()
}

func (self *ReferralCodeViewController) Stop() {}

func (self *ReferralCodeViewController) Close() {
	referralCodeLog("close")

	self.cancel()
}

func (self *ReferralCodeViewController) setIsFetching(isFetching bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.isFetching = isFetching
	}()
}

func (self *ReferralCodeViewController) fetchNetworkReferralCode() {

	if !self.isFetching {
		self.device.GetApi().GetNetworkReferralCode(
			GetNetworkReferralCodeCallback(
				connect.NewApiCallback[*GetNetworkReferralCodeResult](
					func(result *GetNetworkReferralCodeResult, err error) {
						if err != nil {
							self.setIsFetching(false)
							referralCodeLog("error fetching referral code: %s", err.Error())
							return
						}

						if result != nil && result.ReferralCode != "" {
							self.referralCodeChanged(result.ReferralCode)
						}

						self.setIsFetching(false)
					},
				),
			),
		)
	}
}
