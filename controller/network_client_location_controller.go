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

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var ipInfoConfig = sync.OnceValue(func() map[string]any {
	c := server.Vault.RequireSimpleResource("ipinfo.yml").Parse()
	return c["ipinfo"].(map[string]any)
})

const LocationLookupResultExpiration = 30 * 24 * time.Hour

// FIXME use lite? curl https://api.ipinfo.io/lite/8.8.8.8?token=0c3390b39605e0

func GetLocationForIp(ctx context.Context, ipStr string) (*model.Location, *model.ConnectionLocationScores, error) {
	resultJson, err := GetIPInfo(ctx, ipStr)
	if err != nil {
		return nil, nil, err
	}

	// server.Logger().Printf("Got ipinfo result %s", string(resultJson))

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
	err = json.Unmarshal(resultJson, &ipInfoResult)
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

type IpInfoErrorResponse struct {
	Status int `json:"status"`
	Error  struct {
		Title   string `json:"title"`
		Message string `json:"message"`
	} `json:"error"`
}

func GetIPInfo(ctx context.Context, ipStr string) ([]byte, error) {
	earliestResultTime := server.NowUtc().Add(-LocationLookupResultExpiration)

	var errResp IpInfoErrorResponse

	var resultJson []byte
	if resultJsonStr := model.GetLatestIpLocationLookupResult(ctx, ipStr, earliestResultTime); resultJsonStr != "" {
		resultJson = []byte(resultJsonStr)

		/**
		 * Check if cached JSON is an invalid response
		 */
		if err := json.Unmarshal(resultJson, &errResp); err == nil && errResp.Status == 429 {
			return nil, fmt.Errorf("rate limit exceeded: %s", errResp.Error.Message)
		}

		return resultJson, nil
	}

	req, err := http.NewRequest(
		"GET",
		fmt.Sprintf("https://ipinfo.io/%s/json", ipStr),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}

	token, ok := ipInfoConfig()["access_token"].(string)

	if !ok {
		return nil, fmt.Errorf("could not cast access_token to string")
	}

	req.Header.Add(
		"Authorization",
		fmt.Sprintf("Bearer %s", token),
	)

	client := server.DefaultHttpClient()

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error sending request: %w", err)
	}
	defer res.Body.Close()

	resultJson, err = io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	/**
	 * check if the response indicates a rate limit error
	 */
	if err := json.Unmarshal(resultJson, &errResp); err == nil && errResp.Status == 429 {
		return nil, fmt.Errorf("rate limit exceeded: %s", errResp.Error.Message)
	}

	resultJson = server.AttemptCompactJson(resultJson)

	model.SetIpLocationLookupResult(ctx, ipStr, string(resultJson))

	return resultJson, nil
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
