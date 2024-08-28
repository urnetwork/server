package client

import (
	"context"
	"slices"

	"bringyour.com/connect"
)

var dvcLog = logFn("device_view_controller")

type NetworkClientsListener interface {
	NetworkClientsChanged(networkClients *NetworkClientInfoList)
}

type DevicesViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device *BringYourDevice

	networkClientsListeners *connect.CallbackList[NetworkClientsListener]
}

func newDevicesViewController(ctx context.Context, device *BringYourDevice) *DevicesViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &DevicesViewController{
		ctx:                     cancelCtx,
		cancel:                  cancel,
		device:                  device,
		networkClientsListeners: connect.NewCallbackList[NetworkClientsListener](),
	}
	return vc
}

func (self *DevicesViewController) ClientId() *Id {
	return self.device.ClientId()
}

func (self *DevicesViewController) Start() {
	// FIXME

	// request clients
	self.device.Api().GetNetworkClients(GetNetworkClientsCallback(connect.NewApiCallback[*NetworkClientsResult](
		func(result *NetworkClientsResult, err error) {
			if err == nil {
				// FIXME sort

				networkClients := []*NetworkClientInfo{}

				for i := 0; i < result.Clients.Len(); i += 1 {
					networkClient := result.Clients.Get(i)
					networkClients = append(networkClients, networkClient)
				}

				slices.SortStableFunc(networkClients, self.cmpNetworkClientLayout)

				exportedNetworkClients := NewNetworkClientInfoList()
				exportedNetworkClients.addAll(networkClients...)
				self.networkClientsChanged(exportedNetworkClients)
			}
		},
	)))
}

func (self *DevicesViewController) Stop() {
	// FIXME
}

func (self *DevicesViewController) AddNetworkClientsListener(listener NetworkClientsListener) Sub {
	callbackId := self.networkClientsListeners.Add(listener)
	return newSub(func() {
		self.networkClientsListeners.Remove(callbackId)
	})
}

// `NetworkClientsListener`
func (self *DevicesViewController) networkClientsChanged(networkClients *NetworkClientInfoList) {
	for _, listener := range self.networkClientsListeners.Get() {
		connect.HandleError(func() {
			listener.NetworkClientsChanged(networkClients)
		})
	}
}

func (self *DevicesViewController) Close() {
	dvcLog("close")

	self.cancel()
}

func (self *DevicesViewController) cmpNetworkClientLayout(a *NetworkClientInfo, b *NetworkClientInfo) int {
	if a == b {
		return 0
	}

	clientId := *self.ClientId()
	if (clientId == *a.ClientId) != (clientId == *b.ClientId) {
		if clientId == *a.ClientId {
			return -1
		} else {
			return 1
		}
	}

	if (a.Connections != nil && 0 < a.Connections.Len()) != (b.Connections != nil && 0 < b.Connections.Len()) {
		if a.Connections != nil && 0 < a.Connections.Len() {
			return -1
		} else {
			return 1
		}
	}

	return a.ClientId.Cmp(b.ClientId)
}
