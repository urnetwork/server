package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"bringyour.com/bringyour"
)

type CoinbaseClient interface {
	FetchExchangeRates(currencyTicker string) (*CoinbaseExchangeRatesResults, error)
}

type CoreCoinbaseClient struct {}

var coinbaseClientInstance CoinbaseClient = &CoreCoinbaseClient{}

func NewCoinbaseClient() CoinbaseClient {
	return coinbaseClientInstance
}

func SetCoinbaseClient(client CoinbaseClient) {
	coinbaseClientInstance = client
}

type CoinbaseExchangeRatesResults struct {
	Currency string `json:"currency"`
	Rates map[string]string `json:"rates"`
}

var coinbaseApiHost = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("coinbase.yml").Parse()
	return c["api"].(map[string]any)["host"].(string)
})


type CoinbaseResponse[T any] struct {
	Data T `json:"data"`
}

func (c *CoreCoinbaseClient) FetchExchangeRates(
	currencyTicker string,
) (*CoinbaseExchangeRatesResults, error) {
	path := fmt.Sprintf("/v2/exchange-rates?currency=%s", currencyTicker)
	uri := fmt.Sprintf("https://%s%s", coinbaseApiHost(), path)

	exchangeRatesResult, err := bringyour.HttpGetRequireStatusOk(
		uri,
		func(header http.Header) {
			header.Add("Accept", "application/json")
		},
		func(response *http.Response, responseBodyBytes []byte) (*CoinbaseExchangeRatesResults, error) {
			result := &CoinbaseResponse[CoinbaseExchangeRatesResults]{}

			err := json.Unmarshal(responseBodyBytes, result)
			if err != nil {
				return nil, err
			}

			return &result.Data, nil
		},
	)

	if err != nil {
		return nil, err
	}

	return exchangeRatesResult, nil

}
