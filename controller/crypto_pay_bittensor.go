package controller

// TAO on the crypto payment rail (mmm/BTCPAY.md §17.3): STUB, the reader
// lands in its own commit.

import (
	"context"
	"errors"

	"github.com/urnetwork/server/model"
)

type bittensorWallet struct {
	*cryptoCoinbaseAddresses
	cryptoAddressWatch
}

func newBittensorWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *bittensorWallet {
	return &bittensorWallet{cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetTao, settings, cryptoTaoAddressCanonical)}
}

var errCryptoTaoReaderPending = errors.New("the TAO reader is not built yet")

// cryptoTaoRefundAddressCanonical is a refund destination a customer gave, as
// the SS58 address (prefix 42) a refund is sent to: STUB, SS58 only.
func cryptoTaoRefundAddressCanonical(address string) (string, error) {
	return cryptoTaoAddressCanonical(address)
}

func (self *bittensorWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	return nil, 0, errCryptoTaoReaderPending
}

func (self *bittensorWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	return nil, false, errCryptoTaoReaderPending
}

func (self *bittensorWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	return 0, 0, 0, errCryptoTaoReaderPending
}
