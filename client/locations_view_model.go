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

var lvmLog = logFn("locations_view_model")

type FilteredLocationsListener interface {
	FilteredLocationsChanged()
}

type LocationsViewModel struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	locations                    *ConnectLocationList
	nextFilterSequenceNumber     int64
	previousFilterSequenceNumber int64

	filteredLocationListeners *connect.CallbackList[FilteredLocationsListener]
}

func newLocationsViewModel(ctx context.Context, device *BringYourDevice) *LocationsViewModel {
	cancelCtx, cancel := context.WithCancel(ctx)

	vm := &LocationsViewModel{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		nextFilterSequenceNumber:     0,
		previousFilterSequenceNumber: 0,
		locations:                    NewConnectLocationList(),

		filteredLocationListeners: connect.NewCallbackList[FilteredLocationsListener](),
	}
	return vm
}

func (vm *LocationsViewModel) Start() {
	vm.FilterLocations("")
}

func (vm *LocationsViewModel) Stop() {}

func (vm *LocationsViewModel) Close() {
	lvmLog("close")

	vm.cancel()
}

func (vm *LocationsViewModel) GetLocations() *ConnectLocationList {
	vm.stateLock.Lock()
	defer vm.stateLock.Unlock()
	return vm.locations
}

func (vm *LocationsViewModel) filteredLocationsChanged() {
	for _, listener := range vm.filteredLocationListeners.Get() {
		connect.HandleError(func() {
			listener.FilteredLocationsChanged()
		})
	}
}

func (vm *LocationsViewModel) AddFilteredLocationsListener(listener FilteredLocationsListener) Sub {
	callbackId := vm.filteredLocationListeners.Add(listener)
	return newSub(func() {
		vm.filteredLocationListeners.Remove(callbackId)
	})
}

func (vm *LocationsViewModel) FilterLocations(filter string) {
	// api call, call callback
	filter = strings.TrimSpace(filter)

	lvmLog("FILTER LOCATIONS %s", filter)

	var filterSequenceNumber int64
	vm.stateLock.Lock()
	vm.nextFilterSequenceNumber += 1
	filterSequenceNumber = vm.nextFilterSequenceNumber
	vm.stateLock.Unlock()

	lvmLog("POST FILTER LOCATIONS %s", filter)

	if filter == "" {
		vm.device.Api().GetProviderLocations(FindLocationsCallback(newApiCallback[*FindLocationsResult](
			func(result *FindLocationsResult, err error) {
				lvmLog("FIND LOCATIONS RESULT %s %s", result, err)
				if err == nil {
					var update bool
					vm.stateLock.Lock()
					if vm.previousFilterSequenceNumber < filterSequenceNumber {
						vm.previousFilterSequenceNumber = filterSequenceNumber
						update = true
					}
					vm.stateLock.Unlock()

					if update {
						vm.setFilteredLocationsFromResult(result)
					}
				}
			},
		)))
	} else {
		findLocations := &FindLocationsArgs{
			Query: filter,
		}
		vm.device.Api().FindProviderLocations(findLocations, FindLocationsCallback(newApiCallback[*FindLocationsResult](
			func(result *FindLocationsResult, err error) {
				lvmLog("FIND LOCATIONS RESULT %s %s", result, err)
				if err == nil {
					var update bool
					vm.stateLock.Lock()
					if vm.previousFilterSequenceNumber < filterSequenceNumber {
						vm.previousFilterSequenceNumber = filterSequenceNumber
						update = true
					}
					vm.stateLock.Unlock()

					if update {
						vm.setFilteredLocationsFromResult(result)
					}
				}
			},
		)))
	}
}

func (vm *LocationsViewModel) setFilteredLocationsFromResult(result *FindLocationsResult) {
	lvmLog("SET FILTERED LOCATIONS FROM RESULT %s", result)

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

	slices.SortStableFunc(locations, cmpConnectLocations)

	exportedFilteredLocations := NewConnectLocationList()
	exportedFilteredLocations.addAll(locations...)
	vm.locations = exportedFilteredLocations
	vm.filteredLocationsChanged()
}

func cmpConnectLocations(a *ConnectLocation, b *ConnectLocation) int {
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

/**
 * Code is usually the country code which maps to a color hex.
 * If the location is not a country, we just need a unique string that represents the location
 * ie locationId.toString()
 */
func (vm *LocationsViewModel) GetColorHex(code string) string {

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
