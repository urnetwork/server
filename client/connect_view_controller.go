package client

import (
	"context"
	// "fmt"
	"slices"
	"strings"
	"sync"

	// "golang.org/x/mobile/gl"

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
	// glViewController

	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	activeLocation               *ConnectLocation
	nextFilterSequenceNumber     int64
	previousFilterSequenceNumber int64

	connectionListeners       *connect.CallbackList[ConnectionListener]
	filteredLocationListeners *connect.CallbackList[FilteredLocationsListener]
}

func newConnectViewController(ctx context.Context, device *BringYourDevice) *ConnectViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ConnectViewController{
		// glViewController: *newGLViewController(),
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		nextFilterSequenceNumber:     0,
		previousFilterSequenceNumber: 0,

		connectionListeners:       connect.NewCallbackList[ConnectionListener](),
		filteredLocationListeners: connect.NewCallbackList[FilteredLocationsListener](),
	}
	// vc.drawController = vc
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

// FIXME ConnectWithSpecs(SpecList)

func (self *ConnectViewController) Connect(location *ConnectLocation) {
	// api call to get client ids, device.SETLOCATION
	// call callback

	// TODO store the connected locationId
	// TODO reset clientIds

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.activeLocation = location
	}()

	if location.IsDevice() {
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
		self.connectionChanged(location)
	} else {
		specs := NewProviderSpecList()
		specs.Add(&ProviderSpec{
			LocationId:      location.ConnectLocationId.LocationId,
			LocationGroupId: location.ConnectLocationId.LocationGroupId,
		})

		self.device.SetDestination(specs, ProvideModePublic)
		self.connectionChanged(location)
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

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.activeLocation = nil
	}()

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
		self.device.Api().GetProviderLocations(FindLocationsCallback(connect.NewApiCallback[*FindLocationsResult](
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
		self.device.Api().FindProviderLocations(findLocations, FindLocationsCallback(connect.NewApiCallback[*FindLocationsResult](
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
			Name:          groupResult.Name,
			ProviderCount: int32(groupResult.ProviderCount),
			Promoted:      groupResult.Promoted,
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
			LocationType:      locationResult.LocationType,
			Name:              locationResult.Name,
			City:              locationResult.City,
			Region:            locationResult.Region,
			Country:           locationResult.Country,
			CountryCode:       locationResult.CountryCode,
			CityLocationId:    locationResult.CityLocationId,
			RegionLocationId:  locationResult.RegionLocationId,
			CountryLocationId: locationResult.CountryLocationId,
			ProviderCount:     int32(locationResult.ProviderCount),
			MatchDistance:     int32(locationResult.MatchDistance),
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

// func (self *ConnectViewController) draw(g gl.Context) {
// 	// cvcLog("draw")

// 	g.ClearColor(self.bgRed, self.bgGreen, self.bgBlue, 1.0)
// 	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
// }

// func (self *ConnectViewController) drawLoopOpen() {
// 	self.frameRate = 24
// }

// func (self *ConnectViewController) drawLoopClose() {
// }

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
	ConnectLocationId *ConnectLocationId `json:"connect_location_id,omitempty"`
	Name              string             `json:"name,omitempty"`

	ProviderCount int32 `json:"provider_count,omitempty"`
	Promoted      bool  `json:"promoted,omitempty"`
	MatchDistance int32 `json:"match_distance,omitempty"`

	LocationType LocationType `json:"location_type,omitempty"`

	City        string `json:"city,omitempty"`
	Region      string `json:"region,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`

	CityLocationId    *Id `json:"city_location_id,omitempty"`
	RegionLocationId  *Id `json:"region_location_id,omitempty"`
	CountryLocationId *Id `json:"country_location_id,omitempty"`
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
	ClientId        *Id `json:"client_id,omitempty"`
	LocationId      *Id `json:"location_id,omitempty"`
	LocationGroupId *Id `json:"location_group_id,omitempty"`
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
