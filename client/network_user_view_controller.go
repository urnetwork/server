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

type NetworkUser struct {
	UserId   *Id    `json:"userId"`
	UserName string `json:"userName"`
	UserAuth string `json:"userAuth"`
	Verified bool   `json:"verified"`
	AuthType string `json:"authType"`
}

type NetworkUserViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	networkUser *NetworkUser
	isLoading   bool

	isLoadingListener   *connect.CallbackList[IsNetworkUserLoadingListener]
	networkUserListener *connect.CallbackList[NetworkUserListener]
}

func newNetworkUserViewController(ctx context.Context, device *BringYourDevice) *NetworkUserViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &NetworkUserViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		networkUser: nil,
		isLoading:   false,

		isLoadingListener:   connect.NewCallbackList[IsNetworkUserLoadingListener](),
		networkUserListener: connect.NewCallbackList[NetworkUserListener](),
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
