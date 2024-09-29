package client

import (
	"context"
	"sync"

	"bringyour.com/connect"
)

var apvcLog = logFn("account_preferences_view_controller")

type AllowProductUpdatesListener interface {
	StateChanged(bool)
}

type AccountPreferencesViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device *BringYourDevice

	stateLock sync.Mutex

	allowProductUpdates bool
	isFetching          bool
	isUpdating          bool

	allowProductUpdatesListeners *connect.CallbackList[AllowProductUpdatesListener]
}

func newAccountPreferencesViewController(ctx context.Context, device *BringYourDevice) *AccountPreferencesViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	vc := &AccountPreferencesViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		allowProductUpdates: false,
		isFetching:          false,
		isUpdating:          false,

		allowProductUpdatesListeners: connect.NewCallbackList[AllowProductUpdatesListener](),
	}
	return vc
}

func (self *AccountPreferencesViewController) Start() {
	go self.fetchAllowProductUpdates()
}

func (self *AccountPreferencesViewController) Stop() {
	// FIXME
}

func (self *AccountPreferencesViewController) Close() {
	apvcLog("close")

	self.cancel()
}

func (self *AccountPreferencesViewController) AddAllowProductUpdatesListener(listener AllowProductUpdatesListener) Sub {
	callbackId := self.allowProductUpdatesListeners.Add(listener)
	return newSub(func() {
		self.allowProductUpdatesListeners.Remove(callbackId)
	})
}

func (self *AccountPreferencesViewController) allowProductUpdatesChanged(allow bool) {
	for _, listener := range self.allowProductUpdatesListeners.Get() {
		connect.HandleError(func() {
			listener.StateChanged(allow)
		})
	}
}

func (self *AccountPreferencesViewController) setIsFetching(isFetching bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.isFetching = isFetching
	}()
}

func (self *AccountPreferencesViewController) setIsUpdating(isUpdating bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.isUpdating = isUpdating
	}()
}

func (self *AccountPreferencesViewController) UpdateAllowProductUpdates(allow bool) {

	if !self.isUpdating {

		self.setIsUpdating(true)

		self.device.GetApi().AccountPreferencesUpdate(
			&AccountPreferencesSetArgs{
				ProductUpdates: allow,
			},
			connect.NewApiCallback[*AccountPreferencesSetResult](
				func(result *AccountPreferencesSetResult, err error) {

					if err != nil {
						apvcLog("error updating account preferences: %s", err.Error())
						self.setIsUpdating(false)
						return
					}

					self.setAllowProductUpdates(allow)

				}))

	}

}

func (self *AccountPreferencesViewController) GetAllowProductUpdates() bool {
	return self.allowProductUpdates
}

func (self *AccountPreferencesViewController) setAllowProductUpdates(allow bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.allowProductUpdates = allow
	}()

	self.allowProductUpdatesChanged(allow)

}

func (self *AccountPreferencesViewController) fetchAllowProductUpdates() {

	if !self.isFetching {

		self.setIsFetching(true)

		self.device.GetApi().AccountPreferencesGet(AccountPreferencesGetCallback(connect.NewApiCallback[*AccountPreferencesGetResult](
			func(result *AccountPreferencesGetResult, err error) {

				if err != nil {
					apvcLog("error fetching account preferences: %s", err.Error())
					self.setIsFetching(false)
					return
				}

				if result == nil {
					self.setAllowProductUpdates(false)
				} else {
					self.setAllowProductUpdates(result.ProductUpdates)
				}

				self.setIsFetching(false)

			})))
	}

}
