package controller

// Coinbase deposit addresses for the coins received into our Coinbase account
// (mmm/BTCPAY.md §17.2): TAO, Bitcoin, ETH and native SOL. The address refill
// (crypto_pay_scan.go) asks for one new address per future invoice, named
// with the invoice id it will serve, with the Coinbase App API:
//
//	GET  /v2/accounts/<CURRENCY>               the coin's wallet (Coinbase v2 keeps one account per currency)
//	POST /v2/accounts/<account_id>/addresses   {"name": <invoice id>, "network": <network>}
//
// with the CDP key of vault coinbase.yml (the view and receive scopes). Each
// request carries a fresh CDP JWT, signed by the CDP SDK's GenerateJWT as the
// x402 facilitator's are (x402_facilitator.go): the App API takes its `uris`
// claim (checked read-only on 2026-11-10). coinbase.yml's api.account_id is
// none of the four coins' wallets, so each coin's wallet is looked up by its
// currency code.
//
// No key of ours is involved, and nothing here reads a balance: Coinbase
// moves funds out of its deposit addresses at any time, so payments are read
// from the chain as transfers (crypto_pay_bitcoin.go, crypto_pay_ethereum.go,
// crypto_pay_solana_native.go, crypto_pay_bittensor.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coinbase/cdp-sdk/go/auth"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Each coin's Coinbase currency code.
var cryptoCoinbaseCurrencies = map[string]string{
	model.CryptoAssetTao: "TAO",
	model.CryptoAssetBtc: "BTC",
	model.CryptoAssetEth: "ETH",
	model.CryptoAssetSol: "SOL",
}

// Each coin's Coinbase network, named explicitly so a coin Coinbase also
// takes on other networks (Bitcoin and ETH on Base, SOL on Base) is always
// received on its own chain. TAO's is "bittensor": the id Coinbase
// Exchange's public currency data gives its one network
// (api.exchange.coinbase.com/currencies/TAO, read 2026-11-10). The App API's
// first live address request confirms it: a refused network fails the
// refill, and TAO stays unavailable, with the error in the log.
var cryptoCoinbaseNetworks = map[string]string{
	model.CryptoAssetTao: "bittensor",
	model.CryptoAssetBtc: "bitcoin",
	model.CryptoAssetEth: "ethereum",
	model.CryptoAssetSol: "solana",
}

// A Coinbase request is abandoned after this long.
const cryptoCoinbaseTimeout = 20 * time.Second

// A Coinbase answer is read up to this many bytes.
const cryptoCoinbaseMaxResponseBytes = 1 << 20

// A coin's wallet id is looked up again after this long.
const cryptoCoinbaseAccountTtl = time.Hour

// Coinbase has no wallet for the coin in our account: it is opened in the
// Coinbase app (Receive <coin>), after which the refill fills the pool.
var errCryptoCoinbaseNoAccount = errors.New("our Coinbase account has no wallet for this coin")

// The Coinbase App API's base url. Replaceable only by tests.
var cryptoCoinbaseBaseUrl = func(host string) string {
	return "https://" + host
}

// cryptoCoinbaseReceiver makes deposit addresses with one Coinbase key.
type cryptoCoinbaseReceiver struct {
	settings   *cryptoCoinbaseSettings
	httpClient *http.Client
}

func newCryptoCoinbaseReceiver(settings *cryptoCoinbaseSettings) *cryptoCoinbaseReceiver {
	return &cryptoCoinbaseReceiver{
		settings:   settings,
		httpClient: server.NewHttpClient(cryptoCoinbaseTimeout),
	}
}

// The coins' wallet ids, per process.
type cryptoCoinbaseAccount struct {
	accountId string
	fetchedAt time.Time
}

var cryptoCoinbaseAccountsLock sync.Mutex
var cryptoCoinbaseAccounts = map[string]*cryptoCoinbaseAccount{}

// resetCryptoCoinbaseAccounts forgets the cached wallet ids (tests).
func resetCryptoCoinbaseAccounts() {
	cryptoCoinbaseAccountsLock.Lock()
	defer cryptoCoinbaseAccountsLock.Unlock()
	cryptoCoinbaseAccounts = map[string]*cryptoCoinbaseAccount{}
}

// cryptoCoinbaseError is what Coinbase answered, when it refused: its status
// and its errors' ids and messages, never a credential or the request.
type cryptoCoinbaseError struct {
	Status int
	Errors []cryptoCoinbaseErrorEntry
}

type cryptoCoinbaseErrorEntry struct {
	Id      string `json:"id"`
	Message string `json:"message"`
}

func (self *cryptoCoinbaseError) Error() string {
	parts := []string{}
	for _, entry := range self.Errors {
		parts = append(parts, strings.TrimSpace(entry.Id+": "+entry.Message))
	}
	return fmt.Sprintf("coinbase http %d %s", self.Status, strings.Join(parts, "; "))
}

// do makes one App API request, signed with a fresh CDP JWT for exactly its
// method, host and path, and decodes the answer's data into data.
func (self *cryptoCoinbaseReceiver) do(ctx context.Context, method string, path string, body any, data any) error {
	token, err := auth.GenerateJWT(auth.JwtOptions{
		KeyID:         self.settings.KeyName,
		KeySecret:     self.settings.PrivateKey,
		RequestMethod: method,
		RequestHost:   self.settings.Host,
		RequestPath:   path,
	})
	if err != nil {
		// the SDK's errors name what is wrong with a key, never the key
		return fmt.Errorf("coinbase.yml: the key could not sign a request: %w", err)
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	requestCtx, cancel := context.WithTimeout(ctx, cryptoCoinbaseTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, cryptoCoinbaseBaseUrl(self.settings.Host)+path, reader)
	if err != nil {
		return errors.New("coinbase: invalid request url")
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := self.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("coinbase %s %s: transport failed", method, cryptoCoinbasePathKind(path))
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, cryptoCoinbaseMaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("coinbase %s %s: read failed", method, cryptoCoinbasePathKind(path))
	}
	if cryptoCoinbaseMaxResponseBytes < len(responseBody) {
		return errors.New("coinbase: answer too large")
	}
	var envelope struct {
		Data   json.RawMessage            `json:"data"`
		Errors []cryptoCoinbaseErrorEntry `json:"errors"`
	}
	decodeErr := json.Unmarshal(responseBody, &envelope)
	if response.StatusCode < 200 || 300 <= response.StatusCode || 0 < len(envelope.Errors) {
		return &cryptoCoinbaseError{Status: response.StatusCode, Errors: envelope.Errors}
	}
	if decodeErr != nil || len(envelope.Data) == 0 {
		return fmt.Errorf("coinbase %s %s: undecodable answer", method, cryptoCoinbasePathKind(path))
	}
	if err := json.Unmarshal(envelope.Data, data); err != nil {
		return fmt.Errorf("coinbase %s %s: undecodable data: %w", method, cryptoCoinbasePathKind(path), err)
	}
	return nil
}

// cryptoCoinbasePathKind names a request in an error without its ids.
func cryptoCoinbasePathKind(path string) string {
	if strings.HasSuffix(path, "/addresses") {
		return "addresses"
	}
	return "account"
}

// cryptoCoinbaseCurrencyCode reads an account's currency, an object
// ({"code": "BTC", ...}) in the current API and a bare code in older ones.
func cryptoCoinbaseCurrencyCode(raw json.RawMessage) string {
	var object struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.Code != "" {
		return object.Code
	}
	var code string
	if err := json.Unmarshal(raw, &code); err == nil {
		return code
	}
	return ""
}

// accountId is the id of our Coinbase wallet for a currency, cached for an
// hour. errCryptoCoinbaseNoAccount when the account has none.
func (self *cryptoCoinbaseReceiver) accountId(ctx context.Context, currency string) (string, error) {
	now := time.Now()
	cryptoCoinbaseAccountsLock.Lock()
	cached, ok := cryptoCoinbaseAccounts[currency]
	cryptoCoinbaseAccountsLock.Unlock()
	if ok && now.Sub(cached.fetchedAt) < cryptoCoinbaseAccountTtl {
		return cached.accountId, nil
	}
	var account struct {
		Id       string          `json:"id"`
		Type     string          `json:"type"`
		Currency json.RawMessage `json:"currency"`
	}
	err := self.do(ctx, http.MethodGet, "/v2/accounts/"+currency, nil, &account)
	var coinbaseErr *cryptoCoinbaseError
	if errors.As(err, &coinbaseErr) && coinbaseErr.Status == http.StatusNotFound {
		return "", fmt.Errorf("%w (%s): open it in the Coinbase app (Receive %s)", errCryptoCoinbaseNoAccount, currency, currency)
	}
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(cryptoCoinbaseCurrencyCode(account.Currency), currency) || account.Id == "" {
		return "", fmt.Errorf("coinbase answered another account than the %s wallet", currency)
	}
	cryptoCoinbaseAccountsLock.Lock()
	cryptoCoinbaseAccounts[currency] = &cryptoCoinbaseAccount{accountId: account.Id, fetchedAt: now}
	cryptoCoinbaseAccountsLock.Unlock()
	return account.Id, nil
}

// createAddress makes a new deposit address in the wallet on the network,
// named name, and checks Coinbase made it for that network.
func (self *cryptoCoinbaseReceiver) createAddress(ctx context.Context, accountId string, network string, name string) (string, error) {
	var address struct {
		Id      string `json:"id"`
		Address string `json:"address"`
		Name    string `json:"name"`
		Network string `json:"network"`
		// a memo some networks need; none of ours does, so one is refused
		DestinationTag string `json:"destination_tag"`
	}
	err := self.do(ctx, http.MethodPost, "/v2/accounts/"+accountId+"/addresses", map[string]string{
		"name":    name,
		"network": network,
	}, &address)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(address.Network, network) {
		return "", fmt.Errorf("coinbase made an address on network %q, not %q", address.Network, network)
	}
	if address.DestinationTag != "" {
		return "", errors.New("coinbase made an address that needs a destination tag")
	}
	if strings.TrimSpace(address.Address) == "" {
		return "", errors.New("coinbase answered no address")
	}
	return strings.TrimSpace(address.Address), nil
}

// cryptoCoinbaseAddresses is the address side of a Coinbase coin's wallet:
// each new address is a Coinbase deposit address named with the id of the
// invoice that will draw it, at the next pool index.
type cryptoCoinbaseAddresses struct {
	asset string
	// nil when coinbase.yml names no complete key: no address can be made
	receiver *cryptoCoinbaseReceiver
	// checks Coinbase's address for the coin and gives its canonical form
	canonical func(address string) (string, error)

	stateLock sync.Mutex
	highWater uint64
}

func newCryptoCoinbaseAddresses(asset string, settings *cryptoWalletSettings, canonical func(string) (string, error)) *cryptoCoinbaseAddresses {
	addresses := &cryptoCoinbaseAddresses{asset: asset, canonical: canonical}
	if settings != nil && settings.Coinbase != nil {
		addresses.receiver = newCryptoCoinbaseReceiver(settings.Coinbase)
	}
	return addresses
}

// SetAddressHighWater is the pool's highest index: new addresses come after.
func (self *cryptoCoinbaseAddresses) SetAddressHighWater(index uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.highWater = max(self.highWater, index)
}

// NewAddress refuses: a Coinbase address is made for a named invoice.
func (self *cryptoCoinbaseAddresses) NewAddress(ctx context.Context, label string) (string, uint64, error) {
	return "", 0, fmt.Errorf("a %s address is made for a named invoice", self.asset)
}

// NewNamedAddress makes the deposit address of the invoice named name.
func (self *cryptoCoinbaseAddresses) NewNamedAddress(ctx context.Context, name string) (string, uint64, error) {
	address, err := self.newNamedAddress(ctx, name)
	if err != nil {
		cryptoPayCoinbaseAddresses.WithLabelValues(self.asset, "error").Inc()
		return "", 0, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.highWater += 1
	return address, self.highWater, nil
}

func (self *cryptoCoinbaseAddresses) newNamedAddress(ctx context.Context, name string) (string, error) {
	if self.receiver == nil {
		return "", errors.New("coinbase.yml: api.host, api.key_name and api.private_key are not all set")
	}
	currency, ok := cryptoCoinbaseCurrencies[self.asset]
	if !ok {
		return "", fmt.Errorf("%s is not received into Coinbase", self.asset)
	}
	accountId, err := self.receiver.accountId(ctx, currency)
	if err != nil {
		return "", err
	}
	address, err := self.receiver.createAddress(ctx, accountId, cryptoCoinbaseNetworks[self.asset], name)
	if err != nil {
		return "", err
	}
	canonical, err := self.canonical(address)
	if err != nil {
		return "", fmt.Errorf("coinbase made a %s address that is not one: %w", self.asset, err)
	}
	return canonical, nil
}
