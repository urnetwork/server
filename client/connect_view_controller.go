package client


import (
	"context"
	"strings"

	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
)


var cvcLog = logFn("connect_view_controller")


type ConnectViewController struct {
	glViewController

	ctx context.Context
	cancel context.CancelFunc
	device *BringYourDevice

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
		connectionListeners: connect.NewCallbackList[ConnectionListener](),
		filteredLocationListeners: connect.NewCallbackList[FilteredLocationsListener](),
	}
	vc.drawController = vc
	return vc
}

func (self *ConnectViewController) AddConnectionListener(listener ConnectionListener) Sub {
	self.connectionListeners.Add(listener)
	return newSub(func() {
		self.connectionListeners.Remove(listener)
	})
}

// `ConnectionListener`
func (self *ConnectViewController) connectionChanged(locationId *ConnectLocationId, connected bool) {
	for _, listener := range self.connectionListeners.Get() {
		func() {
			defer recover()
			listener.ConnectionChanged(locationId, connected)
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

func (self *ConnectViewController) Connect(locationId *ConnectLocationId) {
	// api call to get client ids, device.SETLOCATION
	// call callback

	exportedExcludeClientIds := NewIdList()
	// exclude self
	exportedExcludeClientIds.Add(self.device.ClientId())

	findActiveProviders := &FindActiveProvidersArgs{
		LocationId: locationId.LocationId,
		LocationGroupId: locationId.LocationGroupId,
		Count: 1,
		ExcludeClientIds: exportedExcludeClientIds,
	}
	self.device.Api().FindProviders(findActiveProviders, FindActiveProvidersCallback(newApiCallback[*FindActiveProvidersResult](
		func(result *FindActiveProvidersResult, err error) {
			if err == nil {
				if result.ClientIds != nil && 1 <= result.ClientIds.Len() {
					clientId := result.ClientIds.Get(0)
					self.device.SetDestinationPublicClientId(clientId)
					self.connectionChanged(locationId, true)
				}
			}
		},
	)))
}

func (self *ConnectViewController) Shuffle() {
	// FIXME
}

func (self *ConnectViewController) Broaden() {
	// FIXME
}

func (self *ConnectViewController) Disconnect() {
	self.device.RemoveDestination()
	self.connectionChanged(nil, false)
}

func (self *ConnectViewController) FilterLocations(filter string) {
	// api call, call callback
	filter = strings.TrimSpace(filter)

	cvcLog("FILTER LOCATIONS %s", filter)

	if filter == "" {
		self.device.Api().GetProviderLocations(FindLocationsCallback(newApiCallback[*FindLocationsResult](
			func(result *FindLocationsResult, err error) {
				cvcLog("FIND LOCATIONS RESULT %s %s", result, err)
				if err == nil {
					self.setFilteredLocationsFromResult(result)
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
					self.setFilteredLocationsFromResult(result)
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
			ProviderCount: groupResult.ProviderCount,
		    Promoted: groupResult.Promoted,
		    MatchDistance: groupResult.MatchDistance,
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
		    ProviderCount: locationResult.ProviderCount,
		    MatchDistance: locationResult.MatchDistance,
		}
		locations = append(locations, location)
	}

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


type ConnectionListener interface {
	ConnectionChanged(locationId *ConnectLocationId, connected bool)
}


type FilteredLocationsListener interface {
	FilteredLocationsChanged(filteredLocations *ConnectLocationList)
}


// merged location and location group
type ConnectLocation struct {
	ConnectLocationId *ConnectLocationId
	Name string

	ProviderCount int
    Promoted bool
    MatchDistance int

    LocationType LocationType

	City string
	Region string
	Country string
	CountryCode string

	CityLocationId *Id
    RegionLocationId *Id
    CountryLocationId *Id
}


// merged location and location group
type ConnectLocationId struct {
	LocationId *Id 
	LocationGroupId *Id 
}

func (self *ConnectLocationId) IsGroup() bool {
	return self.LocationGroupId != nil
}
