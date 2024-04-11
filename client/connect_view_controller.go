package client


import (
	"context"
	"strings"
	"sync"
	"slices"
	"fmt"

	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
)


var cvcLog = logFn("connect_view_controller")


type ConnectionListener interface {
	ConnectionChanged(location *ConnectLocation)
}


type FilteredLocationsListener interface {
	FilteredLocationsChanged(filteredLocations *ConnectLocationList)
}


type LocationListener interface {
	LocationChanged(location *ConnectLocation)
}


type ConnectViewController struct {
	glViewController

	ctx context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	activeLocation *ConnectLocation
	usedDestinationIds map[Id]bool
	activeDestinationIds map[Id]bool
	nextFilterSequenceNumber int64
	previousFilterSequenceNumber int64

	connectionListeners *connect.CallbackList[ConnectionListener]
	filteredLocationListeners *connect.CallbackList[FilteredLocationsListener]
}


func newConnectViewController(ctx context.Context, device *BringYourDevice) *ConnectViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ConnectViewController{
		glViewController: *newGLViewController(),
		ctx: cancelCtx,
		cancel: cancel,
		device: device,

		usedDestinationIds: map[Id]bool{},
		activeDestinationIds: map[Id]bool{},
		nextFilterSequenceNumber: 0,
		previousFilterSequenceNumber: 0,

		connectionListeners: connect.NewCallbackList[ConnectionListener](),
		filteredLocationListeners: connect.NewCallbackList[FilteredLocationsListener](),
	}
	vc.drawController = vc
	return vc
}

func (self *ConnectViewController) Start() {
	// var activeLocation *ConnectLocation
	// self.stateLock.Lock()
	// activeLocation = self.activeLocation
	// self.stateLock.Unlock()

	// self.connectionChanged(activeLocation, activeLocation != nil)

	// TODO filtered results
}

func (self *ConnectViewController) Stop() {
	// FIXME	
}

func (self *ConnectViewController) GetActiveLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.activeLocation
}

func (self *ConnectViewController) AddConnectionListener(listener ConnectionListener) Sub {
	callbackId := self.connectionListeners.Add(listener)
	return newSub(func() {
		self.connectionListeners.Remove(callbackId)
	})
}

// `ConnectionListener`
func (self *ConnectViewController) connectionChanged(location *ConnectLocation) {
	for _, listener := range self.connectionListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectionChanged(location)
		})
	}
}

func (self *ConnectViewController) AddFilteredLocationsListener(listener FilteredLocationsListener) Sub {
	callbackId := self.filteredLocationListeners.Add(listener)
	return newSub(func() {
		self.filteredLocationListeners.Remove(callbackId)
	})
}

// `FilteredLocationsListener`
func (self *ConnectViewController) filteredLocationsChanged(filteredLocations *ConnectLocationList) {
	for _, listener := range self.filteredLocationListeners.Get() {
		connect.HandleError(func() {
			listener.FilteredLocationsChanged(filteredLocations)
		})
	}
}

func (self *ConnectViewController) setDestinations(destinationIds []Id) {

	destinationIdStrs := []string{}
	for _, destinationId := range destinationIds {
		destinationIdStrs = append(destinationIdStrs, destinationId.String())
	}
	fmt.Printf("Found client ids:\n%s", strings.Join(destinationIdStrs, "\n"))


	self.stateLock.Lock()
	for _, destinationId := range destinationIds {
		self.usedDestinationIds[destinationId] = true
	}
	clear(self.activeDestinationIds)
	for _, destinationId := range destinationIds {
		self.activeDestinationIds[destinationId] = true
	}
	self.stateLock.Unlock()

	clientIds := NewIdList()
	for _, destinationId := range destinationIds {
		clientIds.Add(&destinationId)
	}

	self.device.SetDestinationPublicClientIds(clientIds)
}

/*
func (self *ConnectViewController) updateDestination(destinationId Id) {
	self.stateLock.Lock()
	self.usedDestinationIds[destinationId] = true
	clear(self.activeDestinationIds)
	self.activeDestinationIds[destinationId] = true
	self.stateLock.Unlock()

	self.device.SetDestinationPublicClientId(&destinationId)
}
*/


// FIXME ConnectWithSpecs(SpecList)


func (self *ConnectViewController) Connect(location *ConnectLocation) {
	// api call to get client ids, device.SETLOCATION
	// call callback

	// TODO store the connected locationId
	// TODO reset clientIds

	self.stateLock.Lock()
	self.activeLocation = location
	clear(self.usedDestinationIds)
	clear(self.activeDestinationIds)
	self.stateLock.Unlock()

	if location.IsDevice() {
		clientIds := []Id{
			*location.ConnectLocationId.ClientId,
		}
		self.setDestinations(clientIds)
		self.connectionChanged(location)
	} else {
		exportedExcludeClientIds := NewIdList()
		// exclude self
		exportedExcludeClientIds.Add(self.device.ClientId())

		findProviders := &FindProvidersArgs{
			LocationId: location.ConnectLocationId.LocationId,
			LocationGroupId: location.ConnectLocationId.LocationGroupId,
			// FIXME 
			Count: 1024,
			ExcludeClientIds: exportedExcludeClientIds,
		}
		self.device.Api().FindProviders(findProviders, FindProvidersCallback(newApiCallback[*FindProvidersResult](
			func(result *FindProvidersResult, err error) {
				if err == nil && result.ClientIds != nil {
					clientIds := []Id{}
					for i := 0; i < result.ClientIds.Len(); i += 1 {
						clientId := result.ClientIds.Get(i)
						clientIds = append(clientIds, *clientId)
					}
					self.setDestinations(clientIds)
					self.connectionChanged(location)
				}
			},
		)))
	}
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

		if self.activeLocation != nil {
			if !self.activeLocation.IsGroup() {
				// a location
				switch self.activeLocation.LocationType {
				case LocationTypeCity:
					upLocation = self.activeLocation.ToRegion()
				case LocationTypeRegion:
					upLocation = self.activeLocation.ToCountry()
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

	self.stateLock.Lock()
	self.activeLocation = nil
	clear(self.usedDestinationIds)
	clear(self.activeDestinationIds)
	self.stateLock.Unlock()

	self.connectionChanged(nil)
}

func (self *ConnectViewController) FilterLocations(filter string) {
	// api call, call callback
	filter = strings.TrimSpace(filter)

	cvcLog("FILTER LOCATIONS %s", filter)

	var filterSequenceNumber int64
	self.stateLock.Lock()
	self.nextFilterSequenceNumber += 1
	filterSequenceNumber = self.nextFilterSequenceNumber
	self.stateLock.Unlock()

	cvcLog("POST FILTER LOCATIONS %s", filter)

	if filter == "" {
		self.device.Api().GetProviderLocations(FindLocationsCallback(newApiCallback[*FindLocationsResult](
			func(result *FindLocationsResult, err error) {
				cvcLog("FIND LOCATIONS RESULT %s %s", result, err)
				if err == nil {
					var update bool
					self.stateLock.Lock()
					if self.previousFilterSequenceNumber < filterSequenceNumber {
						self.previousFilterSequenceNumber = filterSequenceNumber
						update = true
					}
					self.stateLock.Unlock()

					if update {
						self.setFilteredLocationsFromResult(result)
					}
				}
			},
		)))
	} else {
		findLocations := &FindLocationsArgs{
			Query: filter,
		}
		self.device.Api().FindProviderLocations(findLocations, FindLocationsCallback(newApiCallback[*FindLocationsResult](
			func(result *FindLocationsResult, err error) {
				cvcLog("FIND LOCATIONS RESULT %s %s", result, err)
				if err == nil {
					var update bool
					self.stateLock.Lock()
					if self.previousFilterSequenceNumber < filterSequenceNumber {
						self.previousFilterSequenceNumber = filterSequenceNumber
						update = true
					}
					self.stateLock.Unlock()

					if update {
						self.setFilteredLocationsFromResult(result)
					}
				}
			},
		)))
	}
}

func (self *ConnectViewController) setFilteredLocationsFromResult(result *FindLocationsResult) {
	cvcLog("SET FILTERED LOCATIONS FROM RESULT %s", result)

	locations := []*ConnectLocation{}

	for i := 0; i < result.Groups.Len(); i += 1 {
		groupResult := result.Groups.Get(i)

		location := &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{
				LocationGroupId: groupResult.LocationGroupId,
			},
			Name: groupResult.Name,
			ProviderCount: int32(groupResult.ProviderCount),
		    Promoted: groupResult.Promoted,
		    MatchDistance: int32(groupResult.MatchDistance),
		}
		locations = append(locations, location)
	}

	for i := 0; i < result.Locations.Len(); i += 1 {
		locationResult := result.Locations.Get(i)

		location := &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{
				LocationId: locationResult.LocationId,
			},
			LocationType: locationResult.LocationType,
		    Name: locationResult.Name,
		    City: locationResult.City,
		    Region: locationResult.Region,
		    Country: locationResult.Country,
		    CountryCode: locationResult.CountryCode,
		    CityLocationId: locationResult.CityLocationId,
		    RegionLocationId: locationResult.RegionLocationId,
		    CountryLocationId: locationResult.CountryLocationId,
		    ProviderCount: int32(locationResult.ProviderCount),
		    MatchDistance: int32(locationResult.MatchDistance),
		}
		locations = append(locations, location)
	}

	for i := 0; i < result.Devices.Len(); i += 1 {
		locationDeviceResult := result.Devices.Get(i)

		location := &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{
				ClientId: locationDeviceResult.ClientId,
			},
		    Name: locationDeviceResult.DeviceName,
		}
		locations = append(locations, location)
	}

	slices.SortStableFunc(locations, cmpConnectLocationLayout)

	exportedFilteredLocations := NewConnectLocationList()
	exportedFilteredLocations.addAll(locations...)
	self.filteredLocationsChanged(exportedFilteredLocations)
}


// GL

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


func cmpConnectLocationLayout(a *ConnectLocation, b *ConnectLocation) int {
	// sort locations
	// - devices
	// - groups
	// - promoted
	// - provider count descending
	// - country
	// - region, location
	// - name

	if a == b {
		return 0
	}

	if a.IsDevice() != b.IsDevice() {
		if a.IsDevice() {
			return -1
		} else {
			return 1
		}
	}

	if a.IsGroup() != b.IsGroup() {
		if a.IsGroup() {
			return -1
		} else {
			return 1
		}
	}

	if a.IsGroup() {
		if a.Promoted != b.Promoted {
			if a.Promoted {
				return -1
			} else {
				return 1
			}
		}

		// provider count descending
		if a.ProviderCount != b.ProviderCount {
			if a.ProviderCount < b.ProviderCount {
				return 1
			} else {
				return -1
			}
		}

		return a.ConnectLocationId.LocationGroupId.Cmp(b.ConnectLocationId.LocationGroupId)
	} else {
		if (a.LocationType == LocationTypeCountry) != (b.LocationType == LocationTypeCountry) {
			if a.LocationType == LocationTypeCountry {
				return -1
			} else {
				return 1
			}
		}

		// provider count descending
		if a.ProviderCount != b.ProviderCount {
			if a.ProviderCount < b.ProviderCount {
				return 1
			} else {
				return -1
			}
		}

		if (a.LocationType == LocationTypeRegion) != (b.LocationType == LocationTypeRegion) {
			if a.LocationType == LocationTypeRegion {
				return -1
			} else {
				return 1
			}
		}

		if (a.LocationType == LocationTypeCity) != (b.LocationType == LocationTypeCity) {
			if a.LocationType == LocationTypeCity {
				return -1
			} else {
				return 1
			}
		}

		if a.Name != b.Name {
			if a.Name < b.Name {
				return -1
			} else {
				return 1
			}
		}

		return a.ConnectLocationId.Cmp(b.ConnectLocationId)
	}
}


// merged location and location group
type ConnectLocation struct {
	ConnectLocationId *ConnectLocationId
	Name string

	ProviderCount int32
    Promoted bool
    MatchDistance int32

    LocationType LocationType

	City string
	Region string
	Country string
	CountryCode string

	CityLocationId *Id
    RegionLocationId *Id
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
		Name: self.Region,

		ProviderCount: self.ProviderCount,
	    Promoted: false,
	    MatchDistance: 0,

	    LocationType: LocationTypeRegion,

		City: "",
		Region: self.Region,
		Country: self.Country,
		CountryCode: self.CountryCode,

		CityLocationId: nil,
	    RegionLocationId: self.RegionLocationId,
	    CountryLocationId: self.CountryLocationId,
	}
}

func (self *ConnectLocation) ToCountry() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: self.ConnectLocationId,
		Name: self.Country,

		ProviderCount: self.ProviderCount,
	    Promoted: false,
	    MatchDistance: 0,

	    LocationType: LocationTypeCountry,

		City: "",
		Region: "",
		Country: self.Country,
		CountryCode: self.CountryCode,

		CityLocationId: nil,
	    RegionLocationId: nil,
	    CountryLocationId: self.CountryLocationId,
	}
}


// merged location and location group
type ConnectLocationId struct {
	// if set, the location is a direct connection to another device
	ClientId *Id
	LocationId *Id 
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



