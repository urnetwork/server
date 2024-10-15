package client

import (
	"context"
	"crypto/md5"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"bringyour.com/connect"
)

var locationsVcLog = logFn("locations_view_controller")

type FilterLocationsState = string

const (
	LocationsLoading FilterLocationsState = "LOCATIONS_LOADING"
	LocationsLoaded  FilterLocationsState = "LOCATIONS_LOADED"
	LocationsError   FilterLocationsState = "LOCATIONS_ERROR"
)

type FilteredLocations struct {
	BestMatches *ConnectLocationList
	Promoted    *ConnectLocationList
	Countries   *ConnectLocationList
	Cities      *ConnectLocationList
	Regions     *ConnectLocationList
	Devices     *ConnectLocationList
}

// type FilteredLocationsStateListener interface {
// 	Update(state FilterLocationsState)
// }

type FilteredLocationsListener interface {
	FilteredLocationsChanged(locations *FilteredLocations, state FilterLocationsState)
}

type LocationsViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	nextFilterSequenceNumber     int64
	previousFilterSequenceNumber int64

	filteredLocations     *FilteredLocations
	filteredLocationState FilterLocationsState

	filteredLocationListeners *connect.CallbackList[FilteredLocationsListener]
	// filteredLocationsStateListeners *connect.CallbackList[FilteredLocationsStateListener]
}

func newLocationsViewController(ctx context.Context, device *BringYourDevice) *LocationsViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &LocationsViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		nextFilterSequenceNumber:     0,
		previousFilterSequenceNumber: 0,
		filteredLocations:            nil,
		filteredLocationState:        LocationsError,

		filteredLocationListeners: connect.NewCallbackList[FilteredLocationsListener](),
		// filteredLocationsStateListeners: connect.NewCallbackList[FilteredLocationsStateListener](),
	}
	return vc
}

func (self *LocationsViewController) Start() {
	go self.FilterLocations("")
}

func (self *LocationsViewController) Stop() {}

func (self *LocationsViewController) Close() {
	locationsVcLog("close")

	self.cancel()
}

// func (self *LocationsViewController) GetLocations() *ConnectLocationList {
// 	return self.locations
// }

func (self *LocationsViewController) GetFilteredLocations() *FilteredLocations {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.filteredLocations
}

func (self *LocationsViewController) GetFilteredLocationState() FilterLocationsState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.filteredLocationState
}

func (self *LocationsViewController) filteredLocationsChanged(locations *FilteredLocations, state FilterLocationsState) {
	for _, listener := range self.filteredLocationListeners.Get() {
		connect.HandleError(func() {
			listener.FilteredLocationsChanged(locations, state)
		})
	}
}

func (self *LocationsViewController) AddFilteredLocationsListener(listener FilteredLocationsListener) Sub {
	callbackId := self.filteredLocationListeners.Add(listener)
	return newSub(func() {
		self.filteredLocationListeners.Remove(callbackId)
	})
}

// func (self *LocationsViewController) filterLocationsStateChanged(state FilterLocationsState) {
// 	for _, listener := range self.filteredLocationsStateListeners.Get() {
// 		connect.HandleError(func() {
// 			listener.Update(state)
// 		})
// 	}
// }

// func (self *LocationsViewController) AddFilteredLocationsStateListener(listener FilteredLocationsStateListener) Sub {
// 	callbackId := self.filteredLocationsStateListeners.Add(listener)
// 	return newSub(func() {
// 		self.filteredLocationsStateListeners.Remove(callbackId)
// 	})
// }

func (self *LocationsViewController) FilterLocations(filter string) {
	// api call, call callback
	filter = strings.TrimSpace(filter)

	locationsVcLog("FILTER LOCATIONS %s", filter)
	// self.filterLocationsStateChanged(LocationsLoading)

	var filterSequenceNumber int64
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.nextFilterSequenceNumber += 1
		filterSequenceNumber = self.nextFilterSequenceNumber

		self.filteredLocationState = LocationsLoading
	}()

	self.filteredLocationsChanged(self.GetFilteredLocations(), self.GetFilteredLocationState())

	locationsVcLog("POST FILTER LOCATIONS %s", filter)

	callback := FindLocationsCallback(connect.NewApiCallback[*FindLocationsResult](
		func(result *FindLocationsResult, err error) {
			locationsVcLog("FIND LOCATIONS RESULT %s %s", result, err)

			update := false
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.previousFilterSequenceNumber < filterSequenceNumber {
					self.previousFilterSequenceNumber = filterSequenceNumber
					update = true
					if err == nil {
						self.setFilteredLocationsFromResult(result, filter)
					} else {
						self.filteredLocationState = LocationsError
						self.filteredLocations = nil
					}
				}
			}()
			if update {
				self.filteredLocationsChanged(self.GetFilteredLocations(), self.GetFilteredLocationState())
			}
		},
	))

	if filter == "" {
		self.device.GetApi().GetProviderLocations(callback)
	} else {
		findLocations := &FindLocationsArgs{
			Query: filter,
		}
		self.device.GetApi().FindProviderLocations(findLocations, callback)
	}
}

// must be called with the state lock
// func (self *LocationsViewController) setFilteredLocationState(state FilterLocationsState) {
// 	self.filteredLocationState = state
// }

// must be called with the state lock
func (self *LocationsViewController) setFilteredLocationsFromResult(result *FindLocationsResult, filter string) {
	locationsVcLog("SET FILTERED LOCATIONS FROM RESULT %s", result)

	var bestMatch []*ConnectLocation
	var promoted []*ConnectLocation
	var countries []*ConnectLocation
	var cities []*ConnectLocation
	var regions []*ConnectLocation
	var devices []*ConnectLocation

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

		if groupResult.MatchDistance == 0 && filter != "" {
			bestMatch = append(bestMatch, location)
		} else if groupResult.Promoted {
			promoted = append(promoted, location)
		}
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

		if location.MatchDistance == 0 && filter != "" {
			bestMatch = append(bestMatch, location)
		} else {

			if location.LocationType == LocationTypeCountry {
				countries = append(countries, location)
			}

			// only show cities when searching
			if location.LocationType == LocationTypeCity && filter != "" {
				cities = append(cities, location)
			}

			// only show regions when searching
			if location.LocationType == LocationTypeRegion && filter != "" {
				regions = append(regions, location)
			}

		}

	}

	for i := 0; i < result.Devices.Len(); i += 1 {
		locationDeviceResult := result.Devices.Get(i)

		location := &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{
				ClientId: locationDeviceResult.ClientId,
			},
			Name: locationDeviceResult.DeviceName,
		}
		devices = append(devices, location)
	}

	slices.SortStableFunc(bestMatch, cmpConnectLocations)
	slices.SortStableFunc(promoted, cmpConnectLocations)
	slices.SortStableFunc(countries, cmpConnectLocations)
	slices.SortStableFunc(cities, cmpConnectLocations)
	slices.SortStableFunc(regions, cmpConnectLocations)

	exportedBestMatches := NewConnectLocationList()
	exportedBestMatches.addAll(bestMatch...)

	exportedPromoted := NewConnectLocationList()
	exportedPromoted.addAll(promoted...)

	exportedCountries := NewConnectLocationList()
	exportedCountries.addAll(countries...)

	exportedCities := NewConnectLocationList()
	exportedCities.addAll(cities...)

	exportedRegions := NewConnectLocationList()
	exportedRegions.addAll(regions...)

	exportedDevices := NewConnectLocationList()
	exportedDevices.addAll(devices...)

	filteredLocations := &FilteredLocations{
		BestMatches: exportedBestMatches,
		Promoted:    exportedPromoted,
		Countries:   exportedCountries,
		Cities:      exportedCities,
		Regions:     exportedRegions,
		Devices:     exportedDevices,
	}

	// self.filteredLocationsChanged(filteredLocations)
	// self.filterLocationsStateChanged(LocationsLoaded)

	self.filteredLocations = filteredLocations
	self.filteredLocationState = LocationsLoaded

}

func cmpConnectLocations(a *ConnectLocation, b *ConnectLocation) int {
	// sort locations
	// - provider count descending
	// - name

	if a == b {
		return 0
	}

	if (a.MatchDistance <= 1) != (b.MatchDistance <= 1) {
		if a.MatchDistance <= 1 {
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

	if a.Name != b.Name {
		if a.Name < b.Name {
			return -1
		} else {
			return 1
		}
	}

	return a.ConnectLocationId.Cmp(b.ConnectLocationId)

}

/**
 * Code is usually the country code which maps to a color hex.
 * If the location is not a country, we just need a unique string that represents the location
 * ie locationId.toString()
 */
func (vc *LocationsViewController) GetColorHex(code string) string {

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
