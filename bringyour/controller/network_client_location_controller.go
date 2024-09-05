package controller

import (
	"context"
	"encoding/json"
	"net/http"
	// "encoding/base64"
	"fmt"
	"io"
	"sync"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
)

var ipInfoConfig = sync.OnceValue(func() map[string]any {
	c := bringyour.Vault.RequireSimpleResource("ipinfo.yml").Parse()
	return c["ipinfo"].(map[string]any)
})

const LocationLookupResultExpiration = 24 * time.Hour

func GetLocationForIp(ctx context.Context, ipStr string) (*model.Location, *model.ConnectionLocationScores, error) {
	earliestResultTime := bringyour.NowUtc().Add(-LocationLookupResultExpiration)

	var resultJson []byte
	if resultJsonStr := model.GetLatestIpLocationLookupResult(ctx, ipStr, earliestResultTime); resultJsonStr != "" {
		resultJson = []byte(resultJsonStr)
		resultJson = bringyour.AttemptCompactJson(resultJson)
	} else {
		req, err := http.NewRequest(
			"GET",
			fmt.Sprintf("https://ipinfo.io/%s/json", ipStr),
			nil,
		)
		if err != nil {
			return nil, nil, err
		}

		token := ipInfoConfig()["access_token"].(string)
		bringyour.Logger().Printf("Ipinfo token: %s", token)

		// tokenBase64 := base64.StdEncoding.EncodeToString([]byte(token))

		req.Header.Add(
			"Authorization",
			fmt.Sprintf("Bearer %s", token),
		)

		client := bringyour.DefaultHttpClient()

		res, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer res.Body.Close()

		resultJson, err = io.ReadAll(res.Body)
		if err != nil {
			return nil, nil, err
		}
		resultJson = bringyour.AttemptCompactJson(resultJson)

		bringyour.Logger().Printf("Got ipinfo result %s", string(resultJson))

		model.SetIpLocationLookupResult(ctx, ipStr, string(resultJson))
	}

	bringyour.Logger().Printf("Got ipinfo result %s", string(resultJson))

	/*
		example result:
		{
		  "ip": "64.124.162.234",
		  "hostname": "64.124.162.234.idia-242364-zyo.zip.zayo.com",
		  "city": "Palo Alto",
		  "region": "California",
		  "country": "US",
		  "loc": "37.4180,-122.1274",
		  "org": "AS6461 Zayo Bandwidth",
		  "postal": "94306",
		  "timezone": "America/Los_Angeles"
		}
	*/

	var ipInfoResult IpInfoResult
	err := json.Unmarshal(resultJson, &ipInfoResult)
	if err != nil {
		return nil, nil, err
	}

	location := &model.Location{
		City:        ipInfoResult.City,
		Region:      ipInfoResult.Region,
		CountryCode: ipInfoResult.CountryCode,
	}
	location.LocationType, err = location.GuessLocationType()
	if err != nil {
		return nil, nil, err
	}

	connectionLocationScores := &model.ConnectionLocationScores{}
	if ipInfoResult.Privacy != nil {
		if ipInfoResult.Privacy.Vpn {
			connectionLocationScores.NetTypeVpn = 1
		}
		if ipInfoResult.Privacy.Proxy {
			connectionLocationScores.NetTypeProxy = 1
		}
		if ipInfoResult.Privacy.Tor {
			connectionLocationScores.NetTypeTor = 1
		}
		if ipInfoResult.Privacy.Relay {
			connectionLocationScores.NetTypeRelay = 1
		}
		if ipInfoResult.Privacy.Hosting {
			connectionLocationScores.NetTypeHosting = 1
		}
	}

	return location, connectionLocationScores, nil
}

type IpInfoResult struct {
	City        string         `json:"city,omitempty"`
	Region      string         `json:"region,omitempty"`
	CountryCode string         `json:"country,omitempty"`
	Privacy     *IpInfoPrivacy `json:"privacy,omitempty"`
}

type IpInfoPrivacy struct {
	Vpn     bool `json:"vpn,omitempty"`
	Proxy   bool `json:"proxy,omitempty"`
	Tor     bool `json:"tor,omitempty"`
	Relay   bool `json:"relay,omitempty"`
	Hosting bool `json:"hosting,omitempty"`
}
