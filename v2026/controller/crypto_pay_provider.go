package controller

// What the Coinbase coins' chain readers share (crypto_pay_bitcoin.go,
// crypto_pay_ethereum.go, crypto_pay_solana_native.go,
// crypto_pay_bittensor.go): the invoice addresses the scanner names each
// scan of a per-address coin, and the providers' http client and limits.
// Errors name the request and the status, never the url, which carries the
// provider's key.

import (
	"math/big"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A provider's answer is read up to this many bytes.
const cryptoProviderMaxResponseBytes = 32 * 1024 * 1024

// A provider request is abandoned after this long.
const cryptoProviderTimeout = 30 * time.Second

// The clock the readers judge a stalled source by. Replaceable only by tests.
var cryptoChainNow = time.Now

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

// cryptoProviderClient is the http client of the providers' reads.
func cryptoProviderClient() *http.Client {
	return server.NewHttpClient(cryptoProviderTimeout)
}

// cryptoIndexOf is a pointer to an address index, for a receipt.
func cryptoIndexOf(index int64) *uint64 {
	value := uint64(index)
	return &value
}

// cryptoClampUint64 is an amount as a uint64, saturated (no payment is near).
func cryptoClampUint64(value *big.Int) uint64 {
	if value.IsUint64() {
		return value.Uint64()
	}
	return 1 << 62
}
