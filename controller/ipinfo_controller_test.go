package controller

import (
	"context"
	"encoding/json"
	"math"
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

	myInfo, err := NewMyInfo("1.2.3.4", location, &model.ConnectionLocationScores{})
	connect.AssertEqual(t, err, nil)

	connect.AssertEqual(t, myInfo.IP, "1.2.3.4")
	connect.AssertEqual(t, myInfo.Location.City, "Kamakura")
	connect.AssertEqual(t, myInfo.Location.Region, "Kanagawa")
	connect.AssertEqual(t, myInfo.Location.Country.Code, "jp")
	connect.AssertEqual(t, myInfo.Location.Country.Name, "Japan")
	connect.AssertEqual(t, myInfo.Location.Continent.Code, "as")
	connect.AssertEqual(t, myInfo.Location.Continent.Name, "Asia")
	connect.AssertEqual(t, myInfo.Location.Timezone, "Asia/Tokyo")
	connect.AssertEqual(t, myInfo.Location.Coordinates.Latitude, 35.3192)
	connect.AssertEqual(t, myInfo.Location.Coordinates.Longitude, 139.5467)
	connect.AssertEqual(t, myInfo.Privacy.VPN, false)
	connect.AssertEqual(t, myInfo.Privacy.Hosting, false)
}

// the wire format is fixed by the api spec (MyIPInfoResult in
// connect/api/bringyour.yml): renaming the Go types must not change the json
func TestMyIpInfoResultJson(t *testing.T) {
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

	myInfo, err := NewMyInfo("1.2.3.4", location, &model.ConnectionLocationScores{
		NetTypePrivacy: 1,
		NetTypeHosting: 1,
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, myInfo.Privacy.VPN, true)
	connect.AssertEqual(t, myInfo.Privacy.Hosting, true)

	result := &MyIpInfoResult{
		Info:               myInfo,
		ConnectedToNetwork: true,
	}
	resultJson, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)

	expectedJson := `{"info":{"ip":"1.2.3.4","location":{"coordinates":{"lat":35.3192,"lon":139.5467},"city":"Kamakura","region":"Kanagawa","country":{"code":"jp","name":"Japan"},"continent":{"code":"as","name":"Asia"},"timezone":"Asia/Tokyo"},"privacy":{"vpn":true,"proxy":false,"tor":false,"relay":false,"hosting":true,"service":""}},"connected_to_network":true}`
	connect.AssertEqual(t, string(resultJson), expectedJson)
}

func TestGetMyIpInfo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a stable hosting ip in the bundled mmdb (see server/ip_test.go)
		clientIp := "65.19.157.62"
		clientSession := session.NewLocalClientSession(ctx, clientIp+":12345", nil)

		result, err := GetMyIpInfo(clientSession)
		connect.AssertEqual(t, err, nil)

		connect.AssertEqual(t, result.Info.IP, clientIp)
		connect.AssertEqual(t, result.Info.Location.Country.Code, "us")
		connect.AssertEqual(t, result.Info.Location.Country.Name, "United States")
		connect.AssertEqual(t, result.Info.Location.Region, "California")
		connect.AssertNotEqual(t, result.Info.Location.Coordinates.Latitude, float64(0.0))
		connect.AssertNotEqual(t, result.Info.Location.Coordinates.Longitude, float64(0.0))
		connect.AssertNotEqual(t, result.Info.Privacy, nil)
		connect.AssertEqual(t, result.ConnectedToNetwork, false)

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
