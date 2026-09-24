package controller

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestNewMyInfo(t *testing.T) {
	location := &model.Location{
		LocationType:  model.LocationTypeCity,
		City:          "Kamakura",
		Region:        "Kanagawa",
		Country:       "Japan",
		CountryCode:   "jp",
		Continent:     "Asia",
		ContinentCode: "as",
		Latitude:      35.3192,
		Longitude:     139.5467,
		Timezone:      "Asia/Tokyo",
	}

	// the address is only a label here: NewMyInfo looks nothing up
	myInfo, err := NewMyInfo("192.0.2.1", location)
	connect.AssertEqual(t, err, nil)

	connect.AssertEqual(t, myInfo.IP, "192.0.2.1")
	connect.AssertEqual(t, myInfo.Location.City, "Kamakura")
	connect.AssertEqual(t, myInfo.Location.Region, "Kanagawa")
	connect.AssertEqual(t, myInfo.Location.Country.Code, "jp")
	connect.AssertEqual(t, myInfo.Location.Country.Name, "Japan")
	connect.AssertEqual(t, myInfo.Location.Continent.Code, "as")
	connect.AssertEqual(t, myInfo.Location.Continent.Name, "Asia")
	connect.AssertEqual(t, myInfo.Location.Timezone, "Asia/Tokyo")
	connect.AssertEqual(t, myInfo.Location.Coordinates.Latitude, 35.3192)
	connect.AssertEqual(t, myInfo.Location.Coordinates.Longitude, 139.5467)
}

// the wire format is fixed by the api spec (MyIPInfoResult in
// connect/api/bringyour.yml): renaming the Go types must not change the json.
// It carries only what GeoLite2 backs (connect/GEOMAP.md §3.3): no privacy
// verdicts, and no flag or landmarks.
func TestMyIpInfoResultJson(t *testing.T) {
	location := &model.Location{
		LocationType:     model.LocationTypeCity,
		City:             "Kamakura",
		Region:           "Kanagawa",
		Country:          "Japan",
		CountryCode:      "jp",
		Continent:        "Asia",
		ContinentCode:    "as",
		Latitude:         35.3192,
		Longitude:        139.5467,
		Timezone:         "Asia/Tokyo",
		CityGeonameId:    1860672,
		RegionGeonameId:  1860291,
		CountryGeonameId: 1861060,
	}

	myInfo, err := NewMyInfo("192.0.2.1", location)
	connect.AssertEqual(t, err, nil)

	result := &MyIpInfoResult{
		Info:               myInfo,
		ConnectedToNetwork: true,
	}
	resultJson, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)

	expectedJson := `{"info":{"ip":"192.0.2.1","location":{"coordinates":{"lat":35.3192,"lon":139.5467},"city":"Kamakura","region":"Kanagawa","country":{"code":"jp","name":"Japan"},"continent":{"code":"as","name":"Asia"},"timezone":"Asia/Tokyo"}},"connected_to_network":true}`
	connect.AssertEqual(t, string(resultJson), expectedJson)
	assertMyIpInfoJsonShape(t, resultJson)

	// a country-level answer (GeoLite2 has no city or subdivision for the
	// address) drops city and region, and keeps the coordinates, continent
	// and timezone the database still has
	myInfo, err = NewMyInfo("198.51.100.1", &model.Location{
		LocationType:     model.LocationTypeCountry,
		Country:          "United States",
		CountryCode:      "us",
		Continent:        "North America",
		ContinentCode:    "na",
		Latitude:         37.751,
		Longitude:        -97.822,
		Timezone:         "America/Chicago",
		CountryGeonameId: 6252001,
	})
	connect.AssertEqual(t, err, nil)
	resultJson, err = json.Marshal(&MyIpInfoResult{
		Info:               myInfo,
		ConnectedToNetwork: false,
	})
	connect.AssertEqual(t, err, nil)

	expectedJson = `{"info":{"ip":"198.51.100.1","location":{"coordinates":{"lat":37.751,"lon":-97.822},"country":{"code":"us","name":"United States"},"continent":{"code":"na","name":"North America"},"timezone":"America/Chicago"}},"connected_to_network":false}`
	connect.AssertEqual(t, string(resultJson), expectedJson)
	assertMyIpInfoJsonShape(t, resultJson)
}

// Checks the keys of a /my-ip-info body, whatever the values: exactly `info`
// and `connected_to_network`, no `privacy` or `landmarks` at any level, no
// country flag, and a location carrying coordinates, a country, a continent
// and a timezone.
func assertMyIpInfoJsonShape(t testing.TB, resultJson []byte) {
	t.Helper()
	var result map[string]any
	connect.AssertEqual(t, json.Unmarshal(resultJson, &result), nil)
	keys := func(v any) []string {
		obj, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("expected a json object, got %v", v)
		}
		return slices.Sorted(maps.Keys(obj))
	}
	connect.AssertEqual(t, keys(result), []string{"connected_to_network", "info"})

	info := result["info"].(map[string]any)
	connect.AssertEqual(t, keys(info), []string{"ip", "location"})

	location := info["location"].(map[string]any)
	for _, key := range []string{"coordinates", "country", "continent", "timezone"} {
		if _, ok := location[key]; !ok {
			t.Fatalf("location has no %s: %s", key, resultJson)
		}
	}
	connect.AssertEqual(t, keys(location["coordinates"]), []string{"lat", "lon"})
	connect.AssertEqual(t, keys(location["country"]), []string{"code", "name"})
	connect.AssertEqual(t, keys(location["continent"]), []string{"code", "name"})
	for _, removed := range []string{`"privacy"`, `"landmarks"`, `"flag_url"`} {
		if strings.Contains(string(resultJson), removed) {
			t.Fatalf("the result still has %s: %s", removed, resultJson)
		}
	}
}

func TestGetMyIpInfo(t *testing.T) {
	// a public address, so this reads the GeoLite2 database, which the
	// portable fixture does not have
	if _, err := server.Config.ResourcePath("mmdb/geolite2.mmdb"); err != nil {
		t.Skipf("requires the GeoLite2 City database: %s", err)
	}

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a long-lived hosting network in the US. Only the facts that do not
		// move between GeoLite2 builds are asserted (server/ip_geolite2_test.go
		// pins exact records against one build)
		clientIp := "65.19.157.62"
		clientSession := session.NewLocalClientSession(ctx, clientIp+":12345", nil)

		result, err := GetMyIpInfo(clientSession)
		connect.AssertEqual(t, err, nil)

		connect.AssertEqual(t, result.Info.IP, clientIp)
		connect.AssertEqual(t, result.Info.Location.Country.Code, "us")
		connect.AssertEqual(t, result.Info.Location.Country.Name, "United States")
		connect.AssertEqual(t, result.Info.Location.Continent.Code, "na")
		connect.AssertEqual(t, result.Info.Location.Continent.Name, "North America")
		connect.AssertNotEqual(t, result.Info.Location.Region, "")
		connect.AssertEqual(t, strings.HasPrefix(result.Info.Location.Timezone, "America/"), true)
		connect.AssertNotEqual(t, result.Info.Location.Coordinates.Latitude, float64(0.0))
		connect.AssertNotEqual(t, result.Info.Location.Coordinates.Longitude, float64(0.0))
		connect.AssertEqual(t, result.ConnectedToNetwork, false)

		resultJson, err := json.Marshal(result)
		connect.AssertEqual(t, err, nil)
		assertMyIpInfoJsonShape(t, resultJson)

		// connect a client from the same ip; the ip is now connected to the network
		handlerId := model.CreateNetworkClientHandler(ctx)
		_, _, _, _, err = model.ConnectNetworkClient(ctx, server.NewId(), clientIp+":5555", handlerId)
		connect.AssertEqual(t, err, nil)

		result, err = GetMyIpInfo(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.ConnectedToNetwork, true)

		// a client address that does not parse to an ip is an error
		badSession := session.NewLocalClientSession(ctx, "not-an-ip:80", nil)
		_, err = GetMyIpInfo(badSession)
		connect.AssertNotEqual(t, err, nil)
	})
}

func TestParseCoordinates(t *testing.T) {
	c, err := ParseCoordinates("45.8399,-119.7006")
	connect.AssertEqual(t, nil, err)

	if d := math.Abs(45.8399 - c.Latitude); 1e-8 < d {
		t.Fatalf("%f<>%f", 45.8399, c.Latitude)
	}
	if d := math.Abs(-119.7006 - c.Longitude); 1e-8 < d {
		t.Fatalf("%f<>%f", -119.7006, c.Longitude)
	}
}
