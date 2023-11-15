package client


import (
	"context"
	"strings"
	"sync"
	"slices"

	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
)


var cvcLog = logFn("connect_view_controller")


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
	var activeLocation *ConnectLocation
	self.stateLock.Lock()
	activeLocation = self.activeLocation
	self.stateLock.Unlock()

	self.connectionChanged(activeLocation, activeLocation != nil)

	// TODO filtered results
}

func (self *ConnectViewController) Stop() {
	// FIXME	
}

func (self *ConnectViewController) AddConnectionListener(listener ConnectionListener) Sub {
	self.connectionListeners.Add(listener)
	return newSub(func() {
		self.connectionListeners.Remove(listener)
	})
}

// `ConnectionListener`
func (self *ConnectViewController) connectionChanged(location *ConnectLocation, connected bool) {
	for _, listener := range self.connectionListeners.Get() {
		func() {
			defer recover()
			listener.ConnectionChanged(location, connected)
		}()
	}
}

func (self *ConnectViewController) AddFilteredLocationsListener(listener FilteredLocationsListener) Sub {
	self.filteredLocationListeners.Add(listener)
	return newSub(func() {
		self.filteredLocationListeners.Remove(listener)
	})
}

// `FilteredLocationsListener`
func (self *ConnectViewController) filteredLocationsChanged(filteredLocations *ConnectLocationList) {
	for _, listener := range self.filteredLocationListeners.Get() {
		func() {
			defer recover()
			listener.FilteredLocationsChanged(filteredLocations)
		}()
	}
}

func (self *ConnectViewController) setDestination(location *ConnectLocation, destinationId Id) {
	self.stateLock.Lock()
	self.usedDestinationIds[destinationId] = true
	clear(self.activeDestinationIds)
	self.activeDestinationIds[destinationId] = true
	self.stateLock.Unlock()

	self.device.SetDestinationPublicClientId(&destinationId)
	self.connectionChanged(location, true)
}

func (self *ConnectViewController) updateDestination(destinationId Id) {
	self.stateLock.Lock()
	self.usedDestinationIds[destinationId] = true
	clear(self.activeDestinationIds)
	self.activeDestinationIds[destinationId] = true
	self.stateLock.Unlock()

	self.device.SetDestinationPublicClientId(&destinationId)
}

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

	exportedExcludeClientIds := NewIdList()
	// exclude self
	exportedExcludeClientIds.Add(self.device.ClientId())

	findActiveProviders := &FindActiveProvidersArgs{
		LocationId: location.ConnectLocationId.LocationId,
		LocationGroupId: location.ConnectLocationId.LocationGroupId,
		Count: 1,
		ExcludeClientIds: exportedExcludeClientIds,
	}
	self.device.Api().FindProviders(findActiveProviders, FindActiveProvidersCallback(newApiCallback[*FindActiveProvidersResult](
		func(result *FindActiveProvidersResult, err error) {
			if err == nil {
				if result.ClientIds != nil && 1 <= result.ClientIds.Len() {
					clientId := result.ClientIds.Get(0)
					self.setDestination(location, *clientId)
				}
			}
		},
	)))
}

func (self *ConnectViewController) Shuffle() {
	var connectLocationId *ConnectLocationId
	
	exportedExcludeClientIds := NewIdList()
	// exclude self
	exportedExcludeClientIds.Add(self.device.ClientId())
	self.stateLock.Lock()
	// eclude all tried so far
	for clientId, _ := range self.usedDestinationIds {
		exportedExcludeClientIds.Add(&clientId)
	}

	connectLocationId = self.activeLocation.ConnectLocationId
	self.stateLock.Unlock()

	findActiveProviders := &FindActiveProvidersArgs{
		LocationId: connectLocationId.LocationId,
		LocationGroupId: connectLocationId.LocationGroupId,
		Count: 1,
		ExcludeClientIds: exportedExcludeClientIds,
	}
	self.device.Api().FindProviders(findActiveProviders, FindActiveProvidersCallback(newApiCallback[*FindActiveProvidersResult](
		func(result *FindActiveProvidersResult, err error) {
			if err == nil {
				if result.ClientIds != nil && 1 <= result.ClientIds.Len() {
					clientId := result.ClientIds.Get(0)
					self.updateDestination(*clientId)
				} else {
					// no more destinations to try. Reset `usedDestinationIds` and try again once.

					var update bool
					self.stateLock.Lock()
					if len(self.usedDestinationIds) == 0 {
						// noting to reset
						update = false
					} else {
						clear(self.usedDestinationIds)						
						update = true
					}
					self.stateLock.Unlock()

					if update {
						exportedExcludeClientIds := NewIdList()
						// exclude self
						exportedExcludeClientIds.Add(self.device.ClientId())

						findActiveProviders := &FindActiveProvidersArgs{
							LocationId: connectLocationId.LocationId,
							LocationGroupId: connectLocationId.LocationGroupId,
							Count: 1,
							ExcludeClientIds: exportedExcludeClientIds,
						}

						self.device.Api().FindProviders(findActiveProviders, FindActiveProvidersCallback(newApiCallback[*FindActiveProvidersResult](
							func(result *FindActiveProvidersResult, err error) {
								if err == nil {
									if result.ClientIds != nil && 1 <= result.ClientIds.Len() {
										clientId := result.ClientIds.Get(0)
										self.updateDestination(*clientId)
									}
								}
							},
						)))
					}
				}
			}
		},
	)))
}

func (self *ConnectViewController) Broaden() {
	// FIXME
	var upLocation *ConnectLocation
	self.stateLock.Lock()
	if self.activeLocation == nil {
		return
	}
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
	self.stateLock.Unlock()

	if upLocation != nil {
		self.Connect(upLocation)
	}
}

func (self *ConnectViewController) Reset() {
	self.device.client().Reset()
}

func (self *ConnectViewController) Disconnect() {
	self.device.RemoveDestination()
	self.connectionChanged(nil, false)
}

func (self *ConnectViewController) FilterLocations(filter string) {
	// api call, call callback
	filter = strings.TrimSpace(filter)

	var filterSequenceNumber int64
	self.stateLock.Lock()
	self.nextFilterSequenceNumber += 1
	filterSequenceNumber = self.nextFilterSequenceNumber
	self.stateLock.Unlock()

	cvcLog("FILTER LOCATIONS %s", filter)

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
	// - groups first
	// - promoted
	// - provider count descending
	// - in order: country, region, location

	if a == b {
		return 0
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

		return a.ConnectLocationId.LocationGroupId.cmp(*b.ConnectLocationId.LocationGroupId)
	} else {
		// provider count descending
		if a.ProviderCount != b.ProviderCount {
			if a.ProviderCount < b.ProviderCount {
				return 1
			} else {
				return -1
			}
		}

		if (a.LocationType == LocationTypeCountry) != (b.LocationType == LocationTypeCountry) {
			if a.LocationType == LocationTypeCountry {
				return -1
			} else {
				return 1
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

		return a.ConnectLocationId.LocationId.cmp(*b.ConnectLocationId.LocationId)
	}
}


type ConnectionListener interface {
	ConnectionChanged(location *ConnectLocation, connected bool)
}


type FilteredLocationsListener interface {
	FilteredLocationsChanged(filteredLocations *ConnectLocationList)
}


type LocationListener interface {
	LocationChanged(location *ConnectLocation)
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
	return self.ConnectLocationId.LocationGroupId != nil
}

func (self *ConnectLocation) ToRegion() *ConnectLocation {
	// FIXME
	return self
}

func (self *ConnectLocation) ToCountry() *ConnectLocation {
	// FIXME
	return self
}


// merged location and location group
type ConnectLocationId struct {
	LocationId *Id 
	LocationGroupId *Id 
}

func (self *ConnectLocationId) IsGroup() bool {
	return self.LocationGroupId != nil
}



