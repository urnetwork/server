package controller

// Native SOL on the crypto payment rail (mmm/BTCPAY.md §17.3): STUB, the
// reader lands in its own commit.

import (
	"context"
	"errors"

	"github.com/urnetwork/server/model"
)

type solanaNativeWallet struct {
	*cryptoCoinbaseAddresses
	cryptoAddressWatch
}

func newSolanaNativeWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *solanaNativeWallet {
	return &solanaNativeWallet{cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetSol, settings, cryptoSolanaAddressCanonical)}
}

var errCryptoSolanaReaderPending = errors.New("the native SOL reader is not built yet")

func (self *solanaNativeWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	return nil, 0, errCryptoSolanaReaderPending
}

func (self *solanaNativeWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	return nil, false, errCryptoSolanaReaderPending
}

func (self *solanaNativeWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	return 0, 0, 0, errCryptoSolanaReaderPending
}

// reconcileSolanaChainReferences is the USDC chain leg of the Solana
// reconcile: STUB.
func reconcileSolanaChainReferences(run *paymentReconcileRun) (bool, error) {
	return true, nil
}
