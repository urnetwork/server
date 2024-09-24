package client

import (
	"context"
	"sync"

	"bringyour.com/connect"
)

var nuLog = logFn("network_user_view_controller")

type IsNetworkUserLoadingListener interface {
	StateChanged(bool)
}

type NetworkUserListener interface {
	StateChanged()
}

type NetworkUserUpdateErrorListener interface {
	Message(string)
}

type IsNetworkUserUpdatingListener interface {
	StateChanged(bool)
}

type NetworkUser struct {
	UserId      *Id    `json:"userId"`
	UserName    string `json:"user_name"`
	UserAuth    string `json:"user_auth"`
	Verified    bool   `json:"verified"`
	AuthType    string `json:"auth_type"`
	NetworkName string `json:"network_name"`
}

type NetworkUserViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	networkUser *NetworkUser
	isLoading   bool
	isUpdating  bool

	isLoadingListener              *connect.CallbackList[IsNetworkUserLoadingListener]
	networkUserListener            *connect.CallbackList[NetworkUserListener]
	networkUserUpdateErrorListener *connect.CallbackList[NetworkUserUpdateErrorListener]
	isUpdatingListener             *connect.CallbackList[IsNetworkUserUpdatingListener]
}

func newNetworkUserViewController(ctx context.Context, device *BringYourDevice) *NetworkUserViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &NetworkUserViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		networkUser: nil,
		isLoading:   false,

		isLoadingListener:              connect.NewCallbackList[IsNetworkUserLoadingListener](),
		networkUserListener:            connect.NewCallbackList[NetworkUserListener](),
		networkUserUpdateErrorListener: connect.NewCallbackList[NetworkUserUpdateErrorListener](),
		isUpdatingListener:             connect.NewCallbackList[IsNetworkUserUpdatingListener](),
	}
	return vc
}

func (vc *NetworkUserViewController) Start() {
	go vc.fetchNetworkUser()
}

func (vc *NetworkUserViewController) Stop() {}

func (vc *NetworkUserViewController) Close() {
	nuLog("close")

	vc.cancel()
}

func (vc *NetworkUserViewController) setIsLoading(isLoading bool) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.isLoading = isLoading
	}()

	vc.isLoadingChanged(isLoading)
}

func (vc *NetworkUserViewController) isLoadingChanged(isLoading bool) {
	for _, listener := range vc.isLoadingListener.Get() {
		connect.HandleError(func() {
			listener.StateChanged(isLoading)
		})
	}
}

func (vc *NetworkUserViewController) AddIsLoadingListener(listener IsNetworkUserLoadingListener) Sub {
	callbackId := vc.isLoadingListener.Add(listener)
	return newSub(func() {
		vc.isLoadingListener.Remove(callbackId)
	})
}

func (vc *NetworkUserViewController) GetNetworkUser() *NetworkUser {
	return vc.networkUser
}

func (vc *NetworkUserViewController) networkUserChanged() {
	for _, listener := range vc.networkUserListener.Get() {
		connect.HandleError(func() {
			listener.StateChanged()
		})
	}
}

func (vc *NetworkUserViewController) AddNetworkUserListener(listener NetworkUserListener) Sub {
	callbackId := vc.networkUserListener.Add(listener)
	return newSub(func() {
		vc.networkUserListener.Remove(callbackId)
	})
}

func (vc *NetworkUserViewController) setNetworkUser(nu *NetworkUser) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.networkUser = nu
	}()

	vc.networkUserChanged()
}

func (vc *NetworkUserViewController) fetchNetworkUser() {
	if !vc.isLoading {

		vc.setIsLoading(true)

		vc.device.GetApi().GetNetworkUser(GetNetworkUserCallback(connect.NewApiCallback[*GetNetworkUserResult](
			func(result *GetNetworkUserResult, err error) {

				if err != nil {

					nuLog("fetchNetworkUser go error %s", err.Error())

					vc.setIsLoading(false)
					return
				}

				if result.Error != nil {
					nuLog("fetchNetworkUser response error %s", result.Error.Message)
					vc.setIsLoading(false)
					return
				}

				vc.setNetworkUser(result.NetworkUser)
				vc.setIsLoading(false)

			})))
	}
}

func (vc *NetworkUserViewController) sendNetworkUserUpdateError(msg string) {
	for _, listener := range vc.networkUserUpdateErrorListener.Get() {
		connect.HandleError(func() {
			listener.Message(msg)
		})
	}
}

func (self *NetworkUserViewController) AddNetworkUserUpdateErrorListener(listener NetworkUserUpdateErrorListener) Sub {
	callbackId := self.networkUserUpdateErrorListener.Add(listener)
	return newSub(func() {
		self.networkUserUpdateErrorListener.Remove(callbackId)
	})
}

func (self *NetworkUserViewController) setIsNetworkUserUpdating(updating bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.isUpdating = updating
	}()

	self.isNetworkUserUpdating(updating)
}

func (self *NetworkUserViewController) isNetworkUserUpdating(updating bool) {
	for _, listener := range self.isUpdatingListener.Get() {
		connect.HandleError(func() {
			listener.StateChanged(updating)
		})
	}
}

func (self *NetworkUserViewController) AddIsUpdatingListener(listener IsNetworkUserUpdatingListener) Sub {
	callbackId := self.isUpdatingListener.Add(listener)
	return newSub(func() {
		self.isUpdatingListener.Remove(callbackId)
	})
}

func (self *NetworkUserViewController) UpdateNetworkUser(networkName string, username string) {

	if !self.isUpdating {

		self.setIsNetworkUserUpdating(true)

		self.device.GetApi().NetworkUserUpdate(
			&NetworkUserUpdateArgs{
				NetworkName: networkName,
				UserName:    username,
			},
			NetworkUserUpdateCallback(connect.NewApiCallback[*NetworkUserUpdateResult](
				func(result *NetworkUserUpdateResult, err error) {

					if err != nil {
						self.sendNetworkUserUpdateError("An error occurred updating your profile")
						self.setIsNetworkUserUpdating(false)
						return
					}

					if result.Error != nil {
						self.sendNetworkUserUpdateError(result.Error.Message)
						self.setIsNetworkUserUpdating(false)
						return
					}

					self.stateLock.Lock()
					updatedUser := self.networkUser
					updatedUser.UserName = username
					self.stateLock.Unlock()
					self.setNetworkUser(updatedUser)
					self.setIsNetworkUserUpdating(false)

				}),
			))
	}

}
