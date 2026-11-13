package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/urnetwork/server/v2026"
)

type CoinbaseClient interface {
	FetchExchangeRates(ctx context.Context, currencyTicker string) (*CoinbaseExchangeRatesResults, error)
}

type CoreCoinbaseClient struct {
	readSettings PaymentReadSettings
	readHooks    paymentReadHooks
	readHost     func() string
}

func NewCoreCoinbaseClient(settings PaymentReadSettings) (*CoreCoinbaseClient, error) {
	settings, err := settings.normalized()
	if err != nil {
		return nil, err
	}
	return &CoreCoinbaseClient{readSettings: settings}, nil
}

var coinbaseClientInstance CoinbaseClient = &CoreCoinbaseClient{}

func NewCoinbaseClient() CoinbaseClient {
	return coinbaseClientInstance
}

func SetCoinbaseClient(client CoinbaseClient) {
	coinbaseClientInstance = client
}

type CoinbaseExchangeRatesResults struct {
	Currency string            `json:"currency"`
	Rates    map[string]string `json:"rates"`
}

var coinbaseApiHost = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("coinbase.yml").Parse()
	return c["api"].(map[string]any)["host"].(string)
})

type CoinbaseResponse[T any] struct {
	Data T `json:"data"`
}

func (self *CoreCoinbaseClient) FetchExchangeRates(
	ctx context.Context,
	currencyTicker string,
) (*CoinbaseExchangeRatesResults, error) {
	if ctx == nil {
		return nil, fmt.Errorf("exchange-rate GET requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if currencyTicker == "" || len(currencyTicker) > 32 {
		return nil, fmt.Errorf("invalid exchange-rate currency")
	}
	path := fmt.Sprintf("/v2/exchange-rates?currency=%s", url.QueryEscape(currencyTicker))
	host := self.readHost
	if host == nil {
		host = coinbaseApiHost
	}
	uri := fmt.Sprintf("https://%s%s", host(), path)

	exchangeRatesResult, err := paymentHttpGet(
		ctx,
		self.readSettings,
		self.readHooks,
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
			if !strings.EqualFold(result.Data.Currency, currencyTicker) {
				return nil, fmt.Errorf("exchange-rate observation currency mismatch")
			}

			return &result.Data, nil
		},
	)

	if err != nil {
		return nil, err
	}

	return exchangeRatesResult, nil

}
