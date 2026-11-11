package controller

// What the per-address coins' chain readers share (crypto_pay_bitcoin.go,
// crypto_pay_ethereum.go, crypto_pay_solana_native.go,
// crypto_pay_bittensor.go): the invoice addresses the scanner names each
// scan, and requests to a provider whose url carries its key. Errors name the
// request and the status, never the url.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A provider's answer is read up to this many bytes.
const cryptoProviderMaxResponseBytes = 32 * 1024 * 1024

// A provider request is abandoned after this long.
const cryptoProviderTimeout = 30 * time.Second

// cryptoAddressWatch holds the invoice addresses a scan reads.
type cryptoAddressWatch struct {
	watchLock sync.Mutex
	watched   []*model.CryptoPaymentWatchedAddress
}

// SetWatched names the addresses the next reads cover.
func (self *cryptoAddressWatch) SetWatched(addresses []*model.CryptoPaymentWatchedAddress) {
	self.watchLock.Lock()
	defer self.watchLock.Unlock()
	self.watched = slices.Clone(addresses)
}

func (self *cryptoAddressWatch) watchedAddresses() []*model.CryptoPaymentWatchedAddress {
	self.watchLock.Lock()
	defer self.watchLock.Unlock()
	return slices.Clone(self.watched)
}

// cryptoProviderStatusError is a provider's refusal: its status and, when it
// says, its error text (never the request's url).
type cryptoProviderStatusError struct {
	Kind    string
	Status  int
	Message string
}

func (self *cryptoProviderStatusError) Error() string {
	if self.Message != "" {
		return fmt.Sprintf("%s: http %d: %s", self.Kind, self.Status, self.Message)
	}
	return fmt.Sprintf("%s: http %d", self.Kind, self.Status)
}

// cryptoProviderGet reads a JSON answer from a url that carries a key. A
// non-2xx answer is a cryptoProviderStatusError with the provider's `error`
// text when it gives one.
func cryptoProviderGet(ctx context.Context, client *http.Client, requestUrl string, kind string, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, cryptoProviderTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestUrl, nil)
	if err != nil {
		return fmt.Errorf("%s: invalid url", kind)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("%s: transport: %w", kind, urlErr.Err)
		}
		return fmt.Errorf("%s: transport failed", kind)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, cryptoProviderMaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%s: read failed", kind)
	}
	if cryptoProviderMaxResponseBytes < len(body) {
		return fmt.Errorf("%s: answer too large", kind)
	}
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &refusal)
		message := refusal.Error
		if 200 < len(message) {
			message = message[:200]
		}
		return &cryptoProviderStatusError{Kind: kind, Status: response.StatusCode, Message: message}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: undecodable answer: %w", kind, err)
	}
	return nil
}

// cryptoProviderClient is the http client of the providers' reads.
func cryptoProviderClient() *http.Client {
	return server.NewHttpClient(cryptoProviderTimeout)
}

// cryptoIndexOf is a pointer to an address index, for a receipt.
func cryptoIndexOf(index int64) *uint64 {
	value := uint64(index)
	return &value
}
