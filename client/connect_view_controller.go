// todo - deprecate in favor of connect_view_model

package client

import (
	"context"

	"sync"

	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
)

var cvcLog = logFn("connect_view_controller")

type ConnectViewController struct {
	glViewController

	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	selectedLocation             *ConnectLocation
	connectionStatus             ConnectionStatus
	nextFilterSequenceNumber     int64
	previousFilterSequenceNumber int64
	connectedProviderCount       int32

	selectedLocationListeners *connect.CallbackList[SelectedLocationListener]
	connectionStatusListeners *connect.CallbackList[ConnectionStatusListener]
}

func newConnectViewController(ctx context.Context, device *BringYourDevice) *ConnectViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ConnectViewController{
		glViewController: *newGLViewController(),
		ctx:              cancelCtx,
		cancel:           cancel,
		device:           device,

		nextFilterSequenceNumber:     0,
		previousFilterSequenceNumber: 0,
		connectionStatus:             Disconnected,
		selectedLocation:             nil,
		connectedProviderCount:       0,

		selectedLocationListeners: connect.NewCallbackList[SelectedLocationListener](),
		connectionStatusListeners: connect.NewCallbackList[ConnectionStatusListener](),
	}
	vc.drawController = vc
	return vc
}

func (self *ConnectViewController) Start() {
}

func (self *ConnectViewController) Stop() {
	// FIXME
}

func (self *ConnectViewController) GetConnectionStatus() ConnectionStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectionStatus
}

func (self *ConnectViewController) setConnectionStatus(status ConnectionStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectionStatus = status
	self.connectionStatusChanged(status)
}

func (self *ConnectViewController) connectionStatusChanged(_ ConnectionStatus) {
	for _, listener := range self.connectionStatusListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectionStatusChanged()
		})
	}
}

func (self *ConnectViewController) AddConnectionStatusListener(listener ConnectionStatusListener) Sub {
	callbackId := self.connectionStatusListeners.Add(listener)
	return newSub(func() {
		self.connectionStatusListeners.Remove(callbackId)
	})
}

func (self *ConnectViewController) AddSelectedLocationListener(listener SelectedLocationListener) Sub {
	callbackId := self.selectedLocationListeners.Add(listener)
	return newSub(func() {
		self.selectedLocationListeners.Remove(callbackId)
	})
}

func (self *ConnectViewController) setSelectedLocation(location *ConnectLocation) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.selectedLocation = location
	}()
	self.selectedLocationChanged(location)
}

func (self *ConnectViewController) GetSelectedLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.selectedLocation
}

func (self *ConnectViewController) selectedLocationChanged(location *ConnectLocation) {
	for _, listener := range self.selectedLocationListeners.Get() {
		connect.HandleError(func() {
			listener.SelectedLocationChanged(location)
		})
	}
}

// FIXME ConnectWithSpecs(SpecList)

func (self *ConnectViewController) isCanceling() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	isCanceling := false
	if self.connectionStatus == Canceling {
		isCanceling = true
	}
	return isCanceling
}

func (self *ConnectViewController) Connect(location *ConnectLocation) {
	// api call to get client ids, device.SETLOCATION
	// call callback

	// TODO store the connected locationId
	// TODO reset clientIds

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.selectedLocation = location
	}()

	self.setConnectionStatus(Connecting)

	if location.IsDevice() {

		isCanceling := self.isCanceling()

		if isCanceling {
			self.setConnectionStatus(Disconnected)
		} else {
			destinationIds := []Id{
				*location.ConnectLocationId.ClientId,
			}

			specs := NewProviderSpecList()
			for _, destinationId := range destinationIds {
				specs.Add(&ProviderSpec{
					ClientId: &destinationId,
				})
			}
			self.device.SetDestination(specs, ProvideModePublic)
			self.setSelectedLocation(location)
			self.setConnectionStatus(Connected)
		}

	} else {
		specs := NewProviderSpecList()
		specs.Add(&ProviderSpec{
			LocationId:      location.ConnectLocationId.LocationId,
			LocationGroupId: location.ConnectLocationId.LocationGroupId,
		})

		isCanceling := self.isCanceling()

		if isCanceling {
			self.setConnectionStatus(Disconnected)
		} else {
			self.device.SetDestination(specs, ProvideModePublic)
			self.setSelectedLocation(location)
			self.setConnectionStatus(Connected)
		}
	}
}

func (self *ConnectViewController) ConnectBestAvailable() {

	self.setConnectionStatus(Connecting)

	specs := &ProviderSpecList{}
	specs.Add(&ProviderSpec{
		BestAvailable: true,
	})

	args := &FindProviders2Args{
		Specs: specs,
		Count: 1024,
	}

	self.device.Api().FindProviders2(args, FindProviders2Callback(newApiCallback[*FindProviders2Result](
		func(result *FindProviders2Result, err error) {

			isCanceling := self.isCanceling()
			if isCanceling {
				self.setConnectionStatus(Disconnected)
			} else {

				if err != nil && result.ProviderStats != nil {

					clientIds := []Id{}
					for _, provider := range result.ProviderStats.exportedList.values {
						clientId := provider.ClientId
						clientIds = append(clientIds, *clientId)
					}
					self.setConnectionStatus(Connected)
				} else {
					self.setConnectionStatus(Disconnected)
				}
			}
		},
	)))
}

func (self *ConnectViewController) handleConnectResult(clientIdList *IdList, err error) {

}

func (self *ConnectViewController) CancelConnection() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	status := Canceling
	self.connectionStatus = status
	self.connectionStatusChanged(status)
}

func (self *ConnectViewController) Shuffle() {
	self.device.Shuffle()
}

func (self *ConnectViewController) Broaden() {
	// FIXME
	var upLocation *ConnectLocation
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.selectedLocation != nil {
			if !self.selectedLocation.IsGroup() {
				// a location
				switch self.selectedLocation.LocationType {
				case LocationTypeCity:
					upLocation = self.selectedLocation.ToRegion()
				case LocationTypeRegion:
					upLocation = self.selectedLocation.ToCountry()
					// TODO Mark each country with an upgroup
					// case Country:
					// 	upLocation = self.activeLocation.UpGroup()
				}
			}
		}
	}()

	if upLocation != nil {
		self.Connect(upLocation)
	}
}

func (self *ConnectViewController) Reset() {
	// self.device.client().Reset()

	// todo how to reset?
}

func (self *ConnectViewController) Disconnect() {
	self.device.RemoveDestination()
	self.connectionStatusChanged(Disconnected)
}

func (self *ConnectViewController) draw(g gl.Context) {
	// cvcLog("draw")

	g.ClearColor(self.bgRed, self.bgGreen, self.bgBlue, 1.0)
	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
}

func (self *ConnectViewController) drawLoopOpen() {
	self.frameRate = 24
}

func (self *ConnectViewController) drawLoopClose() {
}

func (self *ConnectViewController) Close() {
	cvcLog("close")

	self.cancel()
}
