package controller

import (
	"context"
	"net/http"
	"encoding/json"
	// "encoding/base64"
	"time"
	"sync"
	"fmt"
	"io"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
)


var ipInfoConfig = sync.OnceValue(func() map[string]any {
	c := bringyour.Vault.RequireSimpleResource("ipinfo.yml").Parse()
	return c["ipinfo"].(map[string]any)
})


const LocationLookupResultExpiration = 1 * time.Hour


func GetLocationForIp(ctx context.Context, ipStr string) (*model.Location, error) {
	earliestResultTime := time.Now().Add(-LocationLookupResultExpiration)

	var resultJson []byte
	if resultJsonStr := model.GetLatestIpLocationLookupResult(ctx, ipStr, earliestResultTime); resultJsonStr != "" {
		resultJson = []byte(resultJsonStr)
	} else {
		req, err := http.NewRequest(
			"GET",
			fmt.Sprintf("https://ipinfo.io/%s/json", ipStr),
			nil,
		)
		if err != nil {
			return nil, err
		}

		token := ipInfoConfig()["access_token"].(string)
		bringyour.Logger().Printf("Ipinfo token: %s", token)

		// tokenBase64 := base64.StdEncoding.EncodeToString([]byte(token))


		req.Header.Add(
			"Authorization",
			fmt.Sprintf("Bearer %s", token),
		)

		client := bringyour.DefaultClient()

		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()

		resultJson, err = io.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}

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
	type IpInfoResult struct {
		City string `json:"city",omitempty`
		Region string `json:"region",omitempty`
		CountryCode string `json:"country",omitempty`
	}
	var ipInfoResult IpInfoResult
	err := json.Unmarshal(resultJson, &ipInfoResult)
	if err != nil {
		return nil, err
	}

	location := &model.Location{
		City: ipInfoResult.City,
		Region: ipInfoResult.Region,
		CountryCode: ipInfoResult.CountryCode,
	}
	location.LocationType, err = location.GuessLocationType()
	if err != nil {
		return nil, err
	}

	return location, nil
}

