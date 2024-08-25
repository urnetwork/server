package client

import (
	"context"

	"sync"

	"bringyour.com/connect"
)

var cvmLog = logFn("connect_view_model")

type ConnectionStatus = string

const (
	Disconnected ConnectionStatus = "DISCONNECTED"
	Connecting   ConnectionStatus = "CONNECTING"
	Connected    ConnectionStatus = "CONNECTED"
	Canceling    ConnectionStatus = "CANCELING"
)

type SelectedLocationListener interface {
	SelectedLocationChanged(location *ConnectLocation)
}

type ConnectionStatusListener interface {
	ConnectionStatusChanged()
}

// type FilteredLocationsListener interface {
// 	FilteredLocationsChanged()
// }

type ConnectedProviderCountListener interface {
	ConnectedProviderCountChanged(count int32)
}

type ConnectViewModel struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	selectedLocation       *ConnectLocation
	connectionStatus       ConnectionStatus
	connectedProviderCount int32

	selectedLocationListeners       *connect.CallbackList[SelectedLocationListener]
	connectionStatusListeners       *connect.CallbackList[ConnectionStatusListener]
	connectedProviderCountListeners *connect.CallbackList[ConnectedProviderCountListener]
}

func newConnectViewModel(ctx context.Context, device *BringYourDevice) *ConnectViewModel {
	cancelCtx, cancel := context.WithCancel(ctx)

	vm := &ConnectViewModel{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		connectionStatus:       Disconnected,
		selectedLocation:       nil,
		connectedProviderCount: 0,

		selectedLocationListeners:       connect.NewCallbackList[SelectedLocationListener](),
		connectionStatusListeners:       connect.NewCallbackList[ConnectionStatusListener](),
		connectedProviderCountListeners: connect.NewCallbackList[ConnectedProviderCountListener](),
	}
	return vm
}

func (vm *ConnectViewModel) Start() {
	// var activeLocation *ConnectLocation
	// self.stateLock.Lock()
	// activeLocation = self.activeLocation
	// self.stateLock.Unlock()

	// self.connectionChanged(activeLocation, activeLocation != nil)

	// TODO filtered results

	// self.FilterLocations("")
}

func (vm *ConnectViewModel) Stop() {
	// FIXME
}

func (vm *ConnectViewModel) Close() {
	cvmLog("close")

	vm.cancel()
}

func (vm *ConnectViewModel) GetConnectionStatus() ConnectionStatus {
	vm.stateLock.Lock()
	defer vm.stateLock.Unlock()
	return vm.connectionStatus
}

func (vm *ConnectViewModel) setConnectionStatus(status ConnectionStatus) {
	func() {
		vm.stateLock.Lock()
		defer vm.stateLock.Unlock()
		vm.connectionStatus = status
	}()
	vm.connectionStatusChanged()
}

func (vm *ConnectViewModel) connectionStatusChanged() {
	for _, listener := range vm.connectionStatusListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectionStatusChanged()
		})
	}
}

func (vm *ConnectViewModel) AddConnectionStatusListener(listener ConnectionStatusListener) Sub {
	callbackId := vm.connectionStatusListeners.Add(listener)
	return newSub(func() {
		vm.connectionStatusListeners.Remove(callbackId)
	})
}

func (vm *ConnectViewModel) AddSelectedLocationListener(listener SelectedLocationListener) Sub {
	callbackId := vm.selectedLocationListeners.Add(listener)
	return newSub(func() {
		vm.selectedLocationListeners.Remove(callbackId)
	})
}

func (vm *ConnectViewModel) setSelectedLocation(location *ConnectLocation) {
	func() {
		vm.stateLock.Lock()
		defer vm.stateLock.Unlock()
		vm.selectedLocation = location
	}()
	vm.selectedLocationChanged(location)
}

func (vm *ConnectViewModel) GetSelectedLocation() *ConnectLocation {
	vm.stateLock.Lock()
	defer vm.stateLock.Unlock()
	return vm.selectedLocation
}

func (vm *ConnectViewModel) selectedLocationChanged(location *ConnectLocation) {
	for _, listener := range vm.selectedLocationListeners.Get() {
		connect.HandleError(func() {
			listener.SelectedLocationChanged(location)
		})
	}
}

func (vm *ConnectViewModel) AddConnectedProviderCountListener(listener ConnectedProviderCountListener) Sub {
	callbackId := vm.connectedProviderCountListeners.Add(listener)
	return newSub(func() {
		vm.connectedProviderCountListeners.Remove(callbackId)
	})
}

// `FilteredLocationsListener`
func (vm *ConnectViewModel) connectedProviderCountChanged(count int32) {
	for _, listener := range vm.connectedProviderCountListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectedProviderCountChanged(count)
		})
	}
}

func (vm *ConnectViewModel) setConnectedProviderCount(count int32) {
	func() {
		vm.stateLock.Lock()
		defer vm.stateLock.Unlock()
		vm.connectedProviderCount = count
	}()
	vm.connectedProviderCountChanged(count)
}

func (vm *ConnectViewModel) GetConnectedProviderCount() int32 {
	vm.stateLock.Lock()
	defer vm.stateLock.Unlock()
	return vm.connectedProviderCount
}

// FIXME ConnectWithSpecs(SpecList)

func (vm *ConnectViewModel) isCanceling() bool {
	vm.stateLock.Lock()
	defer vm.stateLock.Unlock()
	isCanceling := false
	if vm.connectionStatus == Canceling {
		isCanceling = true
	}
	return isCanceling
}

func (vm *ConnectViewModel) Connect(location *ConnectLocation) {
	// api call to get client ids, device.SETLOCATION
	// call callback

	// TODO store the connected locationId
	// TODO reset clientIds

	func() {
		vm.stateLock.Lock()
		defer vm.stateLock.Unlock()
		vm.selectedLocation = location
	}()

	// self.setSelectedLocation(location)
	vm.setConnectionStatus(Connecting)

	if location.IsDevice() {

		isCanceling := vm.isCanceling()

		if isCanceling {
			vm.setConnectionStatus(Disconnected)
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
			vm.device.SetDestination(specs, ProvideModePublic)
			vm.setSelectedLocation(location)
			vm.setConnectionStatus(Connected)
		}

	} else {
		specs := NewProviderSpecList()
		specs.Add(&ProviderSpec{
			LocationId:      location.ConnectLocationId.LocationId,
			LocationGroupId: location.ConnectLocationId.LocationGroupId,
		})

		isCanceling := vm.isCanceling()

		if isCanceling {
			vm.setConnectionStatus(Disconnected)
		} else {
			vm.device.SetDestination(specs, ProvideModePublic)
			vm.setSelectedLocation(location)
			vm.setConnectedProviderCount(location.ProviderCount)
			vm.setConnectionStatus(Connected)
		}
	}
}

func (vm *ConnectViewModel) ConnectBestAvailable() {

	vm.setConnectionStatus(Connecting)

	specs := &ProviderSpecList{}
	specs.Add(&ProviderSpec{
		BestAvailable: true,
	})

	args := &FindProviders2Args{
		Specs: specs,
		Count: 1024,
	}

	vm.device.Api().FindProviders2(args, FindProviders2Callback(newApiCallback[*FindProviders2Result](
		func(result *FindProviders2Result, err error) {

			isCanceling := vm.isCanceling()
			if isCanceling {
				vm.setConnectionStatus(Disconnected)
			} else {

				if err != nil && result.ProviderStats != nil {

					clientIds := []Id{}
					for _, provider := range result.ProviderStats.exportedList.values {
						clientId := provider.ClientId
						clientIds = append(clientIds, *clientId)
					}
					vm.setConnectedProviderCount(int32(len(clientIds)))
					vm.setConnectionStatus(Connected)
				} else {
					vm.setConnectionStatus(Disconnected)
				}
			}
		},
	)))
}

func (vm *ConnectViewModel) CancelConnection() {
	vm.stateLock.Lock()
	defer vm.stateLock.Unlock()
	status := Canceling
	vm.connectionStatus = status
	vm.connectionStatusChanged()
}

// func (self *ConnectViewController) Shuffle() {
// 	self.device.Shuffle()
// }

// func (self *ConnectViewController) Broaden() {
// 	// FIXME
// 	var upLocation *ConnectLocation
// 	func() {
// 		self.stateLock.Lock()
// 		defer self.stateLock.Unlock()

// 		if self.selectedLocation != nil {
// 			if !self.selectedLocation.IsGroup() {
// 				// a location
// 				switch self.selectedLocation.LocationType {
// 				case LocationTypeCity:
// 					upLocation = self.selectedLocation.ToRegion()
// 				case LocationTypeRegion:
// 					upLocation = self.selectedLocation.ToCountry()
// 					// TODO Mark each country with an upgroup
// 					// case Country:
// 					// 	upLocation = self.activeLocation.UpGroup()
// 				}
// 			}
// 		}
// 	}()

// 	if upLocation != nil {
// 		self.Connect(upLocation)
// 	}
// }

// func (self *ConnectViewController) Reset() {
// 	// self.device.client().Reset()

// 	// todo how to reset?
// }

func (vm *ConnectViewModel) Disconnect() {
	vm.device.RemoveDestination()
	vm.connectionStatus = Disconnected
	vm.connectionStatusChanged()
}

// merged location and location group
type ConnectLocation struct {
	ConnectLocationId *ConnectLocationId
	Name              string

	ProviderCount int32
	Promoted      bool
	MatchDistance int32

	LocationType LocationType

	City        string
	Region      string
	Country     string
	CountryCode string

	CityLocationId    *Id
	RegionLocationId  *Id
	CountryLocationId *Id
}

func (self *ConnectLocation) IsGroup() bool {
	return self.ConnectLocationId.IsGroup()
}

func (self *ConnectLocation) IsDevice() bool {
	return self.ConnectLocationId.IsDevice()
}

func (self *ConnectLocation) ToRegion() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: self.ConnectLocationId,
		Name:              self.Region,

		ProviderCount: self.ProviderCount,
		Promoted:      false,
		MatchDistance: 0,

		LocationType: LocationTypeRegion,

		City:        "",
		Region:      self.Region,
		Country:     self.Country,
		CountryCode: self.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  self.RegionLocationId,
		CountryLocationId: self.CountryLocationId,
	}
}

func (self *ConnectLocation) ToCountry() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: self.ConnectLocationId,
		Name:              self.Country,

		ProviderCount: self.ProviderCount,
		Promoted:      false,
		MatchDistance: 0,

		LocationType: LocationTypeCountry,

		City:        "",
		Region:      "",
		Country:     self.Country,
		CountryCode: self.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  nil,
		CountryLocationId: self.CountryLocationId,
	}
}

// merged location and location group
type ConnectLocationId struct {
	// if set, the location is a direct connection to another device
	ClientId        *Id
	LocationId      *Id
	LocationGroupId *Id
}

func (self *ConnectLocationId) IsGroup() bool {
	return self.LocationGroupId != nil
}

func (self *ConnectLocationId) IsDevice() bool {
	return self.ClientId != nil
}

func (self *ConnectLocationId) Cmp(b *ConnectLocationId) int {
	// - direct
	// - group
	if self.ClientId != nil && b.ClientId != nil {
		if c := self.ClientId.Cmp(b.ClientId); c != 0 {
			return c
		}
	} else if self.ClientId != nil {
		return -1
	} else if b.ClientId != nil {
		return 1
	}

	if self.LocationGroupId != nil && b.LocationGroupId != nil {
		if c := self.LocationGroupId.Cmp(b.LocationGroupId); c != 0 {
			return c
		}
	} else if self.LocationGroupId != nil {
		return -1
	} else if b.LocationGroupId != nil {
		return 1
	}

	if self.LocationId != nil && b.LocationId != nil {
		if c := self.LocationId.Cmp(b.LocationId); c != 0 {
			return c
		}
	} else if self.LocationId != nil {
		return -1
	} else if b.LocationId != nil {
		return 1
	}

	return 0
}
