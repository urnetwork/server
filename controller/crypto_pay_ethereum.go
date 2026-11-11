package controller

// ETH on the crypto payment rail (mmm/BTCPAY.md §17.3): STUB, the reader
// lands in its own commit.

import (
	"context"
	"errors"

	"github.com/urnetwork/server/model"
)

type ethereumWallet struct {
	*cryptoCoinbaseAddresses
	cryptoAddressWatch
}

func newEthereumWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *ethereumWallet {
	return &ethereumWallet{cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetEth, settings, cryptoEthereumAddressCanonical)}
}

var errCryptoEthReaderPending = errors.New("the ETH reader is not built yet")

func (self *ethereumWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	return nil, 0, errCryptoEthReaderPending
}

func (self *ethereumWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	return nil, false, errCryptoEthReaderPending
}

func (self *ethereumWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	return 0, 0, 0, errCryptoEthReaderPending
}
