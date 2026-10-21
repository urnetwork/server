package controller

// The in-memory wallet daemon and the shared fixtures of the crypto payment
// tests. The fake keeps receipts as monero-wallet-rpc and zecd report them
// after the clients' decoding: one per transaction and address, mempool
// receipts at height 0, confirmations derived from the wallet's height.

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type fakeCryptoWallet struct {
	stateLock sync.Mutex
	asset     string
	nextIndex uint64
	// receipts by txid/output key, in arrival order
	receipts []*cryptoWalletReceipt
	// txids the daemon no longer knows
	gone map[string]bool

	walletHeight  uint64
	nodeHeight    uint64
	networkHeight uint64

	// a failure every call answers with, when set
	err error
	// the network check's answer
	networkErr error
	// the identity check's answer (another wallet derives other addresses)
	addressErr error
	// the pool addresses the identity check and the lookahead check were
	// asked about, as "<index> <address>"
	addressChecks   []string
	lookaheadChecks []string

	newAddressCalls int
	transactionCall int
}

func newFakeCryptoWallet(asset string) *fakeCryptoWallet {
	return &fakeCryptoWallet{
		asset:         asset,
		nextIndex:     1,
		gone:          map[string]bool{},
		walletHeight:  1000,
		nodeHeight:    1000,
		networkHeight: 1000,
	}
}

// fakeCryptoAddress is a synthetic receive address for an index: never a real
// address, alphanumeric like one.
func fakeCryptoAddress(asset string, index uint64) string {
	switch asset {
	case model.CryptoAssetXmr:
		return fmt.Sprintf("8fakexmr%087d", index)
	case model.CryptoAssetZec:
		return fmt.Sprintf("utest1fakezec%040d", index)
	}
	return fmt.Sprintf("fake%d", index)
}

func (self *fakeCryptoWallet) NewAddress(ctx context.Context, label string) (string, uint64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return "", 0, self.err
	}
	self.newAddressCalls += 1
	index := self.nextIndex
	self.nextIndex += 1
	return fakeCryptoAddress(self.asset, index), index, nil
}

func (self *fakeCryptoWallet) copyReceipt(receipt *cryptoWalletReceipt) *cryptoWalletReceipt {
	copied := *receipt
	if receipt.AddressIndex != nil {
		index := *receipt.AddressIndex
		copied.AddressIndex = &index
	}
	if 0 < copied.BlockHeight && copied.BlockHeight <= self.walletHeight {
		copied.Confirmations = self.walletHeight - copied.BlockHeight + 1
	} else {
		copied.Confirmations = 0
	}
	return &copied
}

func (self *fakeCryptoWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return nil, 0, self.err
	}
	receipts := []*cryptoWalletReceipt{}
	for _, receipt := range self.receipts {
		if self.gone[receipt.TxId] {
			continue
		}
		if receipt.BlockHeight == 0 || minHeight <= receipt.BlockHeight {
			receipts = append(receipts, self.copyReceipt(receipt))
		}
	}
	return receipts, self.walletHeight, nil
}

func (self *fakeCryptoWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.transactionCall += 1
	if self.err != nil {
		return nil, false, self.err
	}
	if self.gone[txId] {
		return nil, false, nil
	}
	receipts := []*cryptoWalletReceipt{}
	for _, receipt := range self.receipts {
		if receipt.TxId == txId {
			receipts = append(receipts, self.copyReceipt(receipt))
		}
	}
	return receipts, 0 < len(receipts), nil
}

func (self *fakeCryptoWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return 0, 0, 0, self.err
	}
	return self.walletHeight, self.nodeHeight, self.networkHeight, nil
}

func (self *fakeCryptoWallet) CheckNetwork(ctx context.Context) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.networkErr
}

func (self *fakeCryptoWallet) CheckAddress(ctx context.Context, index uint64, address string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addressChecks = append(self.addressChecks, fmt.Sprintf("%d %s", index, address))
	return self.addressErr
}

func (self *fakeCryptoWallet) EnsureLookahead(ctx context.Context, highestIndex uint64, highestAddress string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lookaheadChecks = append(self.lookaheadChecks, fmt.Sprintf("%d %s", highestIndex, highestAddress))
	return nil
}

// pay adds a mempool receipt of amountAtomic to the address of index.
func (self *fakeCryptoWallet) pay(txId string, index uint64, amountAtomic uint64) *cryptoWalletReceipt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	addressIndex := index
	receipt := &cryptoWalletReceipt{
		TxId:         txId,
		OutputKey:    fakeCryptoOutputKey(self.asset, index),
		Address:      fakeCryptoAddress(self.asset, index),
		AddressIndex: &addressIndex,
		AmountAtomic: amountAtomic,
		Timestamp:    server.NowUtc(),
	}
	if self.asset == model.CryptoAssetZec {
		receipt.Pool = "orchard"
	}
	self.receipts = append(self.receipts, receipt)
	return receipt
}

func fakeCryptoOutputKey(asset string, index uint64) string {
	if asset == model.CryptoAssetXmr {
		return fmt.Sprintf("0.%d", index)
	}
	return fmt.Sprintf("%d", index)
}

// mine puts a transaction in the block at height.
func (self *fakeCryptoWallet) mine(txId string, height uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, receipt := range self.receipts {
		if receipt.TxId == txId {
			receipt.BlockHeight = height
			if self.asset == model.CryptoAssetZec {
				receipt.Pool = "ironwood"
			}
		}
	}
}

// setHeight moves the wallet, the node and the network to height.
func (self *fakeCryptoWallet) setHeight(height uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.walletHeight = height
	self.nodeHeight = height
	self.networkHeight = height
}

// drop makes the daemon forget a transaction (dropped or reorged away).
func (self *fakeCryptoWallet) drop(txId string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.gone[txId] = true
}

// payFlagged adds a mempool receipt the daemon flags as a double spend.
func (self *fakeCryptoWallet) payFlagged(txId string, index uint64, amountAtomic uint64) *cryptoWalletReceipt {
	receipt := self.pay(txId, index, amountAtomic)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	receipt.DoubleSpend = true
	return receipt
}

// mineAt puts a transaction in the block at height with the block's time; a
// mined transfer is never flagged.
func (self *fakeCryptoWallet) mineAt(txId string, height uint64, blockTime time.Time) {
	self.mine(txId, height)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, receipt := range self.receipts {
		if receipt.TxId == txId {
			receipt.Timestamp = blockTime
			receipt.DoubleSpend = false
		}
	}
}

// setHeights moves the wallet and the network apart (a lagging wallet).
func (self *fakeCryptoWallet) setHeights(walletHeight uint64, networkHeight uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.walletHeight = walletHeight
	self.nodeHeight = networkHeight
	self.networkHeight = networkHeight
}

var _ cryptoWallet = (*fakeCryptoWallet)(nil)
var _ cryptoWalletAddressCheck = (*fakeCryptoWallet)(nil)
var _ cryptoWalletLookahead = (*fakeCryptoWallet)(nil)

// ----- fixtures -----

// cryptoTestConfig is every coin on, with the contract's numbers.
func cryptoTestConfig() *CryptoPayConfig {
	config := &CryptoPayConfig{
		Assets: map[string]*CryptoPayAssetConfig{
			model.CryptoAssetUsdcSolana: {
				Enabled: true,
				Plans:   []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding},
				Items:   []string{StripeItemData1Tib, StripeItemData10Tib},
			},
			model.CryptoAssetXmr: {
				Enabled:             true,
				Network:             cryptoNetworkStagenet,
				Plans:               []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding, model.SolanaPlanMonthly},
				Items:               []string{StripeItemData1Tib, StripeItemData10Tib},
				CreditConfirmations: 1,
				FinalConfirmations:  10,
				ReorgWindowBlocks:   10,
				RateSources:         []string{cryptoRateSourceKraken, cryptoRateSourceCoingecko},
			},
			model.CryptoAssetZec: {
				Enabled:                    true,
				Network:                    cryptoNetworkTestnet,
				Plans:                      []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding},
				Items:                      []string{StripeItemData1Tib, StripeItemData10Tib},
				CreditConfirmations:        10,
				FinalConfirmations:         24,
				ReorgWindowBlocks:          24,
				FinalConfirmationsAfterNu7: 72,
				ReorgWindowBlocksAfterNu7:  72,
				RateSources:                []string{cryptoRateSourceKraken, cryptoRateSourceCoingecko, cryptoRateSourceCoinbase},
			},
		},
		Quote: CryptoPayQuoteConfig{
			Expiry:               30 * time.Minute,
			BufferPercent:        0.5,
			AcceptPercent:        99.5,
			MaxInvoiceUsd:        100,
			RateCache:            60 * time.Second,
			RateStaleAfter:       2 * time.Minute,
			RateMaxSpreadPercent: 3,
		},
		AddressPoolTarget:      5,
		MaxOpenIntentsPerAsset: 5000,
		ScanInterval:           20 * time.Second,
		UnlinkAfter:            720 * time.Hour,
	}
	return config
}

// cryptoTestRates answers every rate source at fixed rates without the
// network.
func cryptoTestRates(t testing.TB, rates map[string]map[string]float64) {
	previous := cryptoRateFetchers
	fetchers := map[string]cryptoRateFetch{}
	for source, byAsset := range rates {
		fetchers[source] = func(ctx context.Context, asset string) (float64, *time.Time, error) {
			rate, ok := byAsset[asset]
			if !ok {
				return 0, nil, fmt.Errorf("%s has no rate for %s", source, asset)
			}
			return rate, nil, nil
		}
	}
	cryptoRateFetchers = fetchers
	cryptoRates.reset()
	t.Cleanup(func() {
		cryptoRateFetchers = previous
		cryptoRates.reset()
	})
}

// cryptoTestStandardRates is XMR at $555.36 and ZEC at $1,304.60 on every
// source.
func cryptoTestStandardRates(t testing.TB) {
	cryptoTestRates(t, map[string]map[string]float64{
		cryptoRateSourceKraken:    {model.CryptoAssetXmr: 555.36, model.CryptoAssetZec: 1304.60},
		cryptoRateSourceCoingecko: {model.CryptoAssetXmr: 555.36, model.CryptoAssetZec: 1304.60},
		cryptoRateSourceCoinbase:  {model.CryptoAssetZec: 1304.60},
	})
}

// cryptoTestEnable turns the rail on for one test: the config, a vault that
// names every daemon, the given fake wallets, and nothing cached.
func cryptoTestEnable(t testing.TB, config *CryptoPayConfig, wallets map[string]*fakeCryptoWallet) {
	restoreConfig := setCryptoPayConfigForTest(config)
	previousSettings := cryptoWalletSettingsFunc
	previousWalletFor := cryptoWalletFor
	cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
		if !slices.Contains(cryptoPayScannedAssets, asset) {
			return nil, fmt.Errorf("%s has no wallet daemon", asset)
		}
		// everything selling needs: the wallet file's password and the node
		return &cryptoWalletSettings{
			Asset:          asset,
			WalletRpcUrl:   "http://127.0.0.1:1/json_rpc",
			WalletRpcUser:  "synthetic",
			WalletRpcPass:  "synthetic",
			PrimaryAddress: "5syntheticprimary",
			WalletPassword: "synthetic",
			NodeRpcUrl:     "http://127.0.0.1:2/json_rpc",
		}, nil
	}
	cryptoWalletFor = func(asset string, settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) (cryptoWallet, error) {
		wallet, ok := wallets[asset]
		if !ok {
			return nil, fmt.Errorf("no fake wallet for %s", asset)
		}
		return wallet, nil
	}
	resetCryptoAvailability()
	t.Cleanup(func() {
		restoreConfig()
		cryptoWalletSettingsFunc = previousSettings
		cryptoWalletFor = previousWalletFor
		resetCryptoAvailability()
	})
}

// cryptoTestIntent is an open xmr invoice for $40 at $555.36 with the address
// of index, without a database.
func cryptoTestIntent(asset string, index int64, now time.Time) *model.CryptoPaymentIntent {
	networkId := server.NewId()
	amountAtomic, _ := cryptoQuoteAmountAtomic(40, 555.36, 0.5, cryptoAssetDecimals(asset))
	return &model.CryptoPaymentIntent{
		IntentId:                 server.NewId(),
		Asset:                    asset,
		Delivery:                 model.CryptoDeliveryAccount,
		NetworkId:                &networkId,
		Item:                     model.SolanaPlanYearly,
		Tier:                     "standard",
		PriceUsdNanoCents:        model.UsdToNanoCents(40),
		RegularPriceUsdNanoCents: model.UsdToNanoCents(40),
		Address:                  fakeCryptoAddress(asset, uint64(index)),
		AddressIndex:             &index,
		AmountAtomic:             amountAtomic,
		RateUsd:                  555.36,
		QuotedAt:                 now,
		ExpiresAt:                now.Add(30 * time.Minute),
		CreateTime:               now,
	}
}

// cryptoTestReceipt is a receipt row of an invoice.
func cryptoTestReceipt(intent *model.CryptoPaymentIntent, txId string, amountAtomic int64, confirmations int, firstSeenAt time.Time, state string) *model.CryptoPaymentReceipt {
	intentId := intent.IntentId
	receipt := &model.CryptoPaymentReceipt{
		Asset:         intent.Asset,
		TxId:          txId,
		OutputKey:     fmt.Sprintf("0.%d", *intent.AddressIndex),
		IntentId:      &intentId,
		Address:       intent.Address,
		AddressIndex:  intent.AddressIndex,
		AmountAtomic:  amountAtomic,
		Confirmations: confirmations,
		FirstSeenAt:   firstSeenAt,
		State:         state,
	}
	if 0 < confirmations {
		height := int64(1000 - confirmations + 1)
		receipt.BlockHeight = &height
	}
	return receipt
}
