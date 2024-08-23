package client

import (
	"context"

	"crypto/md5"
	"fmt"

	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
)

var cvcLog = logFn("connect_view_controller")

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
	ConnectionStatusChanged(status ConnectionStatus)
}

type FilteredLocationsListener interface {
	FilteredLocationsChanged()
}

type ConnectedProviderCountListener interface {
	ConnectedProviderCountChanged(count int32)
}
type ConnectViewController struct {
	glViewController

	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	selectedLocation             *ConnectLocation
	locations                    *ConnectLocationList
	connectionStatus             ConnectionStatus
	nextFilterSequenceNumber     int64
	previousFilterSequenceNumber int64
	connectedProviderCount       int32

	selectedLocationListeners       *connect.CallbackList[SelectedLocationListener]
	connectionStatusListeners       *connect.CallbackList[ConnectionStatusListener]
	filteredLocationListeners       *connect.CallbackList[FilteredLocationsListener]
	connectedProviderCountListeners *connect.CallbackList[ConnectedProviderCountListener]
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
		locations:                    NewConnectLocationList(),
		connectionStatus:             Disconnected,
		selectedLocation:             nil,
		connectedProviderCount:       0,

		selectedLocationListeners:       connect.NewCallbackList[SelectedLocationListener](),
		connectionStatusListeners:       connect.NewCallbackList[ConnectionStatusListener](),
		filteredLocationListeners:       connect.NewCallbackList[FilteredLocationsListener](),
		connectedProviderCountListeners: connect.NewCallbackList[ConnectedProviderCountListener](),
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

	self.FilterLocations("")
}

func (self *ConnectViewController) Stop() {
	// FIXME
}

func (self *ConnectViewController) GetLocations() *ConnectLocationList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.locations
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

func (self *ConnectViewController) AddFilteredLocationsListener(listener FilteredLocationsListener) Sub {
	callbackId := self.filteredLocationListeners.Add(listener)
	return newSub(func() {
		self.filteredLocationListeners.Remove(callbackId)
	})
}

// `FilteredLocationsListener`
func (self *ConnectViewController) filteredLocationsChanged() {
	for _, listener := range self.filteredLocationListeners.Get() {
		connect.HandleError(func() {
			listener.FilteredLocationsChanged()
		})
	}
}

func (self *ConnectViewController) connectionStatusChanged(status ConnectionStatus) {
	for _, listener := range self.connectionStatusListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectionStatusChanged(status)
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

func (self *ConnectViewController) AddConnectedProviderCountListener(listener ConnectedProviderCountListener) Sub {
	callbackId := self.connectedProviderCountListeners.Add(listener)
	return newSub(func() {
		self.connectedProviderCountListeners.Remove(callbackId)
	})
}

// `FilteredLocationsListener`
func (self *ConnectViewController) connectedProviderCountChanged(count int32) {
	for _, listener := range self.connectedProviderCountListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectedProviderCountChanged(count)
		})
	}
}

func (self *ConnectViewController) setConnectedProviderCount(count int32) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectedProviderCount = count
	self.connectedProviderCountChanged(count)
}

func (self *ConnectViewController) GetConnectedProviderCount() int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectedProviderCount
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

	// self.setSelectedLocation(location)
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
			self.setConnectedProviderCount(location.ProviderCount)
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

				if err == nil && result.ProviderStats != nil {

					clientIds := []Id{}
					for _, provider := range result.ProviderStats.exportedList.values {
						clientId := provider.ClientId
						clientIds = append(clientIds, *clientId)
					}
					self.setConnectedProviderCount(int32(len(clientIds)))
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
	self.locations = exportedFilteredLocations
	self.filteredLocationsChanged()
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

func cmpConnectLocationLayout(a *ConnectLocation, b *ConnectLocation) int {
	// sort locations
	// - provider count descending
	// - name

	if a == b {
		return 0
	}

	// provider count descending
	if a.ProviderCount != b.ProviderCount {
		if a.ProviderCount < b.ProviderCount {
			return 1
		} else {
			return -1
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

/**
 * Code is usually the country code which maps to a color hex.
 * If the location is not a country, we just need a unique string that represents the location
 * ie locationId.toString()
 */
func (self *ConnectViewController) GetColorHex(code string) string {

	if color, exists := countryCodeColorHexes[code]; exists {
		return color
	}

	/**
	 * Fallback if color hex isn't found, generate a new one by mixing two colors together
	 */
	keys := make([]string, 0, len(countryCodeColorHexes))
	for k := range countryCodeColorHexes {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	index1 := getHashIndex(code, len(keys))
	index2 := getHashIndex(code+"salt", len(keys))

	color1 := countryCodeColorHexes[keys[index1]]
	color2 := countryCodeColorHexes[keys[index2]]

	return mixColors(color1, color2)
}

// to get a consistent index from the id
func getHashIndex(id string, mod int) int {
	hash := md5.Sum([]byte(id))
	return int(hash[0]) % mod
}

func mixColors(color1, color2 string) string {
	r1, g1, b1 := hexToRGB(color1)
	r2, g2, b2 := hexToRGB(color2)

	// Mix the colors by averaging their RGB components
	r := (r1 + r2) / 2
	g := (g1 + g2) / 2
	b := (b1 + b2) / 2

	return rgbToHex(r, g, b)
}

func hexToRGB(hex string) (int, int, int) {
	var r, g, b int
	fmt.Sscanf(hex, "%02x%02x%02x", &r, &g, &b)
	return r, g, b
}

func rgbToHex(r, g, b int) string {
	return fmt.Sprintf("%02x%02x%02x", r, g, b)
}

var countryCodeColorHexes = map[string]string{
	"is": "639A88",
	"ee": "78C0E0",
	"ca": "449DD1",
	"de": "663F46",
	"au": "F29E4C",
	"us": "BAC5B3",
	"gb": "F1789B",
	"jp": "CC3363",
	"ie": "7EE081",
	"fi": "F56E48",
	"nl": "F56E48",
	"se": "A4C4F4",
	"fr": "A864DC",
	"it": "F9F871",
	"dk": "D6E6F4",
	"no": "BCE5DC",
	"be": "9B4A91",
	"at": "FFCB68",
	"ch": "FFABA0",
	"nz": "008A64",
	"pt": "248C89",
	"es": "B41F43",
	"lv": "EEE8A9",
	"lt": "8179E0",
	"cz": "99E8CE",
	"si": "FF6C58",
	"sk": "87FB67",
	"pl": "D38B5D",
	"hu": "FF8484",
	"hr": "99B2DD",
	"ro": "C6362F",
	"bg": "A1CDF4",
	"gr": "C874D9",
	"cy": "E1BBC9",
	"mt": "FFC43D",
	"il": "A9E4EF",
	"za": "F2B79F",
	"ar": "8E8DBE",
	"br": "DCD6F7",
	"cl": "FA824C",
	"cr": "E07A5F",
	"uy": "7FDEFF",
	"jm": "7B886F",
	"tt": "0072BB",
	"gh": "1098F7",
	"ke": "F2EDEB",
	"ng": "64113F",
	"tn": "6B4D57",
	"sn": "596869",
	"na": "813405",
	"bw": "D45113",
	"mu": "60E1E0",
	"mg": "F25D72",
	"in": "F2E2D2",
	"kr": "C320D9",
	"tw": "E6EA23",
	"my": "3A1772",
	"ph": "B4CEB3",
	"id": "586189",
	"mn": "A6A57A",
	"ge": "679436",
	"am": "F2B5D4",
	"ua": "00F28D",
	"md": "7F675B",
	"me": "E5FFDE",
	"rs": "FF495C",
	"al": "E4B363",
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
