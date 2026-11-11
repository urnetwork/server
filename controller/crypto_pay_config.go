package controller

// Configuration and kill switch of the crypto payment rail
// (mmm/ZCASH-CONTRACT.md §3.6).
//
// Policy is config/<env>/crypto_pay.yml: which coins are on, what each sells,
// confirmation depths, quote rules, pool size, scan cadence and the retention
// window. A missing or unparseable file is everything off, as with
// onboarding.yml: a partial deploy must degrade to "no crypto", never to a
// boot failure or to coins sold on default rules.
//
// Secrets are in the vault, one file per wallet daemon. The kill switch
// (`enabled`) gates selling only: a coin is listed and quoted while it is
// switched on AND its file names everything selling needs (for xmr the wallet
// file's password and monerod, for zec zebrad), and it is scanned, credited
// and reconciled whenever its file names the wallet daemon, switched on or
// not, so payments to open invoices are never stranded (mmm/ZCASH-CONTRACT.md
// §3.13.1, §3.13.9, §3.13.10). The files are read on each use, so a vault fix
// takes effect without a restart:
//
//	vault/<env>/monero.yml
//	  monero:
//	    wallet_rpc_url: http://127.0.0.1:18088/json_rpc   # monero-wallet-rpc, view-only (snow: mainnet 18088, stagenet 38088)
//	    wallet_rpc_user: ...
//	    wallet_rpc_password: ...                           # HTTP digest
//	    primary_address: 4...                              # never issued; a payment to it is unknown_address
//	    wallet_password: ...                               # the Monero wallet FILE's password, not the RPC password:
//	                                                       # set_subaddress_lookahead asks for it (on snow:
//	                                                       # /etc/crypto-pay/<unit>.wallet-password). Required to sell
//	    daemon_rpc_url: http://127.0.0.1:18081/json_rpc    # monerod, for the node and network heights. Required to sell
//	    daemon_rpc_user: ...                               # optional
//	    daemon_rpc_password: ...                           # optional
//
//	vault/<env>/zcash.yml
//	  zcash:
//	    wallet_rpc_url: http://127.0.0.1:8252/             # zecd, watch-only (UFVK); RPC port mainnet 8252, testnet 18252
//	    wallet_rpc_user: ...
//	    wallet_rpc_password: ...                           # HTTP basic
//	    node_rpc_url: http://127.0.0.1:8232/               # zebrad, for the node and network heights. Required to
//	                                                       # sell (except on regtest, where zecd's view of the tip stands in)
//	    node_rpc_user: ...                                 # optional
//	    node_rpc_password: ...                             # optional
//
// USDC on Solana needs neither: its payments arrive through the Helius webhook
// (helius.yml), as they always have.
//
// TAO, Bitcoin, ETH and native SOL are received into our Coinbase account,
// one deposit address per invoice (mmm/BTCPAY.md §17.2), and credited from
// the chain alone (§17.3). Each reads two vault files: coinbase.yml for the
// addresses (selling needs it) and its chain source (scanning needs it):
//
//	vault/<env>/coinbase.yml        # every one of the four coins
//	  api:
//	    host: api.coinbase.com
//	    key_name: organizations/.../apiKeys/...           # a CDP key with the view and receive scopes
//	    private_key: -----BEGIN EC PRIVATE KEY-----...   # ES256
//	    account_id: ...                                   # not used here: each coin's wallet is looked up by its code
//
//	vault/<env>/alchemy.yml         # btc and eth (Alchemy only, §17.3)
//	  alchemy:
//	    api_key: ...                                      # the endpoints are built from it when not given
//	    eth_endpoint: https://eth-mainnet.g.alchemy.com/v2/...      # optional
//	    btc_endpoint: https://bitcoin-mainnet.g.alchemy.com/v2/...  # optional; the JSON-RPC alone, which the free tier serves (§16.7)
//	    notify_auth_token: ...                            # optional: registers each new ETH address with the webhook
//	    eth_webhook_id: wh_...                            # optional: the Address Activity webhook, a fast path only
//	    eth_webhook_signing_key: whsec_...
//
// Bitcoin needs no Alchemy add-on and has no webhook: its scanner reads every
// block through the JSON-RPC (crypto_pay_bitcoin.go).
//
//	vault/<env>/helius.yml          # sol: helius.api_key, the RPC the scanner reads
//	vault/<env>/st.yml              # tao: snow's Finney node (st.GetConnection, URNETWORK_ST_PROFILE)
//
// Polling the chain is the source of truth for all four, so neither coin's
// availability depends on a webhook.

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/st"
)

// The items a coin can sell. Monthly is card only (decision 6): no coin sells
// it, whatever the config lists.
var cryptoPayKnownPlans = []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding}
var cryptoPayKnownItems = []string{StripeItemData1Tib, StripeItemData10Tib}

// The coins, in the order every UI shows them: the main selector's USDC,
// Zcash and Monero, then "More coins": TAO, Bitcoin, ETH, SOL (mmm/BTCPAY.md
// §17.6).
var cryptoPayAssets = []string{
	model.CryptoAssetUsdcSolana,
	model.CryptoAssetZec,
	model.CryptoAssetXmr,
	model.CryptoAssetTao,
	model.CryptoAssetBtc,
	model.CryptoAssetEth,
	model.CryptoAssetSol,
}

// The coins the scanner watches: USDC arrives by webhook.
var cryptoPayScannedAssets = []string{
	model.CryptoAssetZec,
	model.CryptoAssetXmr,
	model.CryptoAssetTao,
	model.CryptoAssetBtc,
	model.CryptoAssetEth,
	model.CryptoAssetSol,
}

// The coins received into our Coinbase account, each invoice at a deposit
// address of its own that Coinbase creates (mmm/BTCPAY.md §17.2). Their
// scanners read the chain per invoice address, never an address's balance:
// Coinbase moves funds out of its deposit addresses at any time.
var cryptoPayCoinbaseAssets = []string{
	model.CryptoAssetTao,
	model.CryptoAssetBtc,
	model.CryptoAssetEth,
	model.CryptoAssetSol,
}

// cryptoAssetCoinbase reports a coin received into our Coinbase account.
func cryptoAssetCoinbase(asset string) bool {
	return slices.Contains(cryptoPayCoinbaseAssets, asset)
}

// The coins whose payments are screened against OFAC's addresses before
// credit (mmm/BTCPAY.md §17.3): their senders are public on the chain.
// Monero and Zcash are shielded, and USDC keeps its own path.
var cryptoPayScreenedAssets = []string{
	model.CryptoAssetTao,
	model.CryptoAssetBtc,
	model.CryptoAssetEth,
	model.CryptoAssetSol,
}

// Networks a wallet daemon may serve, per coin. regtest is zec's private test
// chain (local/crypto-regtest); Monero's regtest uses mainnet's addresses.
const (
	cryptoNetworkMainnet  = "mainnet"
	cryptoNetworkStagenet = "stagenet"
	cryptoNetworkTestnet  = "testnet"
	cryptoNetworkRegtest  = "regtest"
)

// Rate sources by config name.
const (
	cryptoRateSourceKraken   = "kraken"
	cryptoRateSourceCoinbase = "coinbase"
	// Monero's second source until the owner names its replacement: CoinGecko
	// is dropped for every other coin (mmm/BTCPAY.md §17.4, its keyless terms
	// exclude production use), and CoinPaprika's keyless tier, the planned
	// replacement, is licensed for personal use only (coinpaprika.com/api/pricing,
	// read 2026-11-10: "Personal use only. Commercial use starts with
	// Starter", $99 a month; showing the data to customers needs Enterprise).
	// Without a second source Monero could not be quoted at all.
	cryptoRateSourceCoingecko = "coingecko"
)

// cryptoAssetRateSources are the sources a coin may be quoted from, in the
// order its default list names them: Kraken and Coinbase for every coin they
// list (mmm/BTCPAY.md §17.4). Coinbase lists no Monero, and Monero keeps
// CoinGecko (cryptoRateSourceCoingecko).
func cryptoAssetRateSources(asset string) []string {
	switch asset {
	case model.CryptoAssetXmr:
		return []string{cryptoRateSourceKraken, cryptoRateSourceCoingecko}
	case model.CryptoAssetZec, model.CryptoAssetTao, model.CryptoAssetBtc, model.CryptoAssetEth, model.CryptoAssetSol:
		return []string{cryptoRateSourceKraken, cryptoRateSourceCoinbase}
	}
	return []string{}
}

// CryptoPayAssetConfig is one coin's policy.
type CryptoPayAssetConfig struct {
	Enabled bool     `yaml:"enabled"`
	Network string   `yaml:"network"`
	Plans   []string `yaml:"plans"`
	Items   []string `yaml:"items"`
	// credited at this depth (decision 5: xmr 1, zec 10)
	CreditConfirmations int64 `yaml:"credit_confirmations"`
	// re-checked at this depth
	FinalConfirmations int64 `yaml:"final_confirmations"`
	// each scan reaches back this many blocks behind its cursor
	ReorgWindowBlocks int64 `yaml:"reorg_window_blocks"`
	// zec: from this height the final depth and the reorg window are the
	// after-NU7 values (25 s blocks). 0 = not yet set.
	Nu7ActivationHeight        int64 `yaml:"nu7_activation_height"`
	FinalConfirmationsAfterNu7 int64 `yaml:"final_confirmations_after_nu7"`
	ReorgWindowBlocksAfterNu7  int64 `yaml:"reorg_window_blocks_after_nu7"`
	// zec: the wallet lag the quote path allows from the NU7 activation height
	// on. Blocks get three times faster, so max_lag_blocks.zec (20 blocks, 25
	// minutes) would be 8 minutes after it; this keeps about 25 minutes (60).
	MaxLagBlocksAfterNu7 int64    `yaml:"max_lag_blocks_after_nu7"`
	RateSources          []string `yaml:"rate_sources"`
	// whether a data pack can be bought as a balance code, without a network
	// (/pay/data/crypto-intent with no network name). Default true;
	// config/main sets it false for usdc_solana until every server runs a
	// build with migration 853 (mmm/ZCASH-CONTRACT.md §3.13.14)
	CodeDelivery *bool `yaml:"code_delivery"`
	// the quote's buffer for this coin, percent; 0 = the quote's
	// buffer_percent. TAO's default is 1% for its volatility (mmm/BTCPAY.md
	// §17.4)
	BufferPercent float64 `yaml:"buffer_percent"`
	// how often this coin is scanned; 0 = scan_interval. Bitcoin's 10-minute
	// blocks (a scan reads the tip, and a block only when one is new) and
	// ETH's 15-minute finality need no 20 s polling
	ScanInterval time.Duration `yaml:"scan_interval"`
	// the free addresses this coin's pool keeps; 0 = address_pool_target. A
	// Coinbase coin's addresses are made one request each, so its pool is
	// smaller (40)
	AddressPoolTarget int `yaml:"address_pool_target"`
	// a per-address coin's scanner still reads an invoice's address this long
	// after the invoice expired, was credited or was voided, for a late or an
	// extra payment (24 h), less often than an open invoice's. Bitcoin's block
	// scanner matches such an invoice's address in every block mined within
	// this long of its close
	WatchAfter time.Duration `yaml:"watch_after"`
}

// The pool of a Coinbase coin: each address is one request to Coinbase.
const cryptoCoinbasePoolTarget = 40

// How long a per-address coin's closed invoices are still read.
const cryptoDefaultWatchAfter = 24 * time.Hour

// CodeDeliveryEnabled reports whether the coin sells balance codes.
func (self *CryptoPayAssetConfig) CodeDeliveryEnabled() bool {
	return self != nil && (self.CodeDelivery == nil || *self.CodeDelivery)
}

// CryptoPayQuoteConfig is how quotes are made and payments accepted.
type CryptoPayQuoteConfig struct {
	Expiry               time.Duration `yaml:"expiry"`
	BufferPercent        float64       `yaml:"buffer_percent"`
	AcceptPercent        float64       `yaml:"accept_percent"`
	MaxInvoiceUsd        float64       `yaml:"max_invoice_usd"`
	RateCache            time.Duration `yaml:"rate_cache"`
	RateStaleAfter       time.Duration `yaml:"rate_stale_after"`
	RateMaxSpreadPercent float64       `yaml:"rate_max_spread_percent"`
}

// CryptoPayConfig is the parsed crypto_pay.yml.
type CryptoPayConfig struct {
	Assets                 map[string]*CryptoPayAssetConfig `yaml:"assets"`
	Quote                  CryptoPayQuoteConfig             `yaml:"quote"`
	AddressPoolTarget      int                              `yaml:"address_pool_target"`
	MaxOpenIntentsPerAsset int                              `yaml:"max_open_intents_per_asset"`
	// the most open code-delivery invoices (no sign-in, no network) per coin,
	// so they cannot drain the address pool for everyone else
	MaxOpenCodeIntentsPerAsset int              `yaml:"max_open_code_intents_per_asset"`
	ScanInterval               time.Duration    `yaml:"scan_interval"`
	MaxLagBlocks               map[string]int64 `yaml:"max_lag_blocks"`
	UnlinkAfter                time.Duration    `yaml:"unlink_after"`
}

type cryptoPayYaml struct {
	CryptoPay CryptoPayConfig `yaml:"crypto_pay"`
}

// The defaults for a key the file leaves out (the contract's values). A coin
// still sells nothing until the file switches it on.
func (self *CryptoPayConfig) normalize() {
	if self.Assets == nil {
		self.Assets = map[string]*CryptoPayAssetConfig{}
	}
	// a wallet-daemon coin (Zcash, Monero) the file leaves out is off, with
	// the default policy: its scanner still runs when its daemon is in the
	// vault. A Coinbase coin (TAO, Bitcoin, ETH, SOL) the file leaves out has
	// no policy at all: it is not sold, scanned or reconciled, and no
	// deposit address is asked of Coinbase. The release that switches one on
	// adds its entry (cryptoPayCoinbaseAssets, docs/CRYPTO-PAY-API.md).
	for _, asset := range cryptoPayScannedAssets {
		if self.Assets[asset] == nil && !cryptoAssetCoinbase(asset) {
			self.Assets[asset] = &CryptoPayAssetConfig{}
		}
	}
	for asset, assetConfig := range self.Assets {
		if assetConfig == nil || !slices.Contains(cryptoPayAssets, asset) {
			if assetConfig != nil {
				glog.Errorf("[cryptopay]crypto_pay.yml names an unknown asset %q; it is ignored\n", asset)
			}
			delete(self.Assets, asset)
			continue
		}
		// monthly is card only, and an unknown name is never sold
		assetConfig.Plans = slices.DeleteFunc(slices.Clone(assetConfig.Plans), func(plan string) bool {
			return !slices.Contains(cryptoPayKnownPlans, plan)
		})
		assetConfig.Items = slices.DeleteFunc(slices.Clone(assetConfig.Items), func(item string) bool {
			return !slices.Contains(cryptoPayKnownItems, item)
		})
		// a source the coin may not be quoted from is dropped (CoinGecko for
		// every coin but Monero, mmm/BTCPAY.md §17.4), and a list left with
		// fewer than the two a quote needs takes the coin's own
		assetConfig.RateSources = slices.DeleteFunc(slices.Clone(assetConfig.RateSources), func(source string) bool {
			if slices.Contains(cryptoAssetRateSources(asset), source) {
				return false
			}
			glog.Errorf("[cryptopay]crypto_pay.yml names rate source %q for %s, which it is not quoted from; it is ignored\n", source, asset)
			return true
		})
		if len(assetConfig.RateSources) < 2 {
			assetConfig.RateSources = cryptoAssetRateSources(asset)
		}
		switch asset {
		case model.CryptoAssetXmr:
			if assetConfig.CreditConfirmations <= 0 {
				assetConfig.CreditConfirmations = 1
			}
			if assetConfig.FinalConfirmations <= 0 {
				assetConfig.FinalConfirmations = 10
			}
			if assetConfig.ReorgWindowBlocks <= 0 {
				assetConfig.ReorgWindowBlocks = 10
			}
		case model.CryptoAssetTao:
			// snow's finalized blocks only: credited and final at finality
			// (one "confirmation" is a finalized block)
			assetConfig.CreditConfirmations = 1
			assetConfig.FinalConfirmations = 1
			if assetConfig.ReorgWindowBlocks <= 0 {
				assetConfig.ReorgWindowBlocks = 1
			}
			if assetConfig.BufferPercent <= 0 {
				assetConfig.BufferPercent = 1
			}
		case model.CryptoAssetBtc:
			if assetConfig.CreditConfirmations <= 0 {
				assetConfig.CreditConfirmations = 1
			}
			if assetConfig.FinalConfirmations <= 0 {
				assetConfig.FinalConfirmations = 6
			}
			if assetConfig.ReorgWindowBlocks <= 0 {
				assetConfig.ReorgWindowBlocks = 6
			}
			if assetConfig.ScanInterval <= 0 {
				assetConfig.ScanInterval = time.Minute
			}
		case model.CryptoAssetEth:
			// credited at finality: one "confirmation" is a block the
			// finalized tag covers; the window reaches past it (about 64
			// blocks), so a payment in a block that is reorged away before
			// finality is noticed
			assetConfig.CreditConfirmations = 1
			assetConfig.FinalConfirmations = 1
			if assetConfig.ReorgWindowBlocks <= 0 {
				assetConfig.ReorgWindowBlocks = 128
			}
			if assetConfig.ScanInterval <= 0 {
				assetConfig.ScanInterval = 30 * time.Second
			}
		case model.CryptoAssetSol:
			// the finalized commitment: one "confirmation" is a finalized slot
			assetConfig.CreditConfirmations = 1
			assetConfig.FinalConfirmations = 1
			if assetConfig.ReorgWindowBlocks <= 0 {
				assetConfig.ReorgWindowBlocks = 150
			}
		case model.CryptoAssetZec:
			if assetConfig.CreditConfirmations <= 0 {
				assetConfig.CreditConfirmations = 10
			}
			if assetConfig.FinalConfirmations <= 0 {
				assetConfig.FinalConfirmations = 24
			}
			if assetConfig.ReorgWindowBlocks <= 0 {
				assetConfig.ReorgWindowBlocks = 24
			}
			if assetConfig.FinalConfirmationsAfterNu7 <= 0 {
				assetConfig.FinalConfirmationsAfterNu7 = 72
			}
			if assetConfig.ReorgWindowBlocksAfterNu7 <= 0 {
				assetConfig.ReorgWindowBlocksAfterNu7 = 72
			}
			if assetConfig.MaxLagBlocksAfterNu7 <= 0 {
				assetConfig.MaxLagBlocksAfterNu7 = 60
			}
		case model.CryptoAssetUsdcSolana:
			// Helius reports confirmed transfers only
			assetConfig.CreditConfirmations = 1
			assetConfig.FinalConfirmations = 1
		}
		if assetConfig.Network == "" {
			assetConfig.Network = cryptoNetworkMainnet
		}
		if asset == model.CryptoAssetUsdcSolana || cryptoAssetCoinbase(asset) {
			// the Helius webhook watches mainnet only, and Coinbase gives no
			// testnet deposit addresses (mmm/BTCPAY.md §17.8)
			assetConfig.Network = cryptoNetworkMainnet
		}
		if cryptoAssetCoinbase(asset) {
			if assetConfig.AddressPoolTarget <= 0 {
				assetConfig.AddressPoolTarget = cryptoCoinbasePoolTarget
			}
			if assetConfig.WatchAfter <= 0 {
				assetConfig.WatchAfter = cryptoDefaultWatchAfter
			}
		}
	}
	if self.Quote.Expiry <= 0 {
		self.Quote.Expiry = 30 * time.Minute
	}
	// a buffer of 0 (or a missing key) is the contract's 0.5%: a quote with no
	// buffer goes short on any rate move during its expiry
	if self.Quote.BufferPercent <= 0 {
		self.Quote.BufferPercent = 0.5
	}
	if self.Quote.AcceptPercent <= 0 || 100 < self.Quote.AcceptPercent {
		self.Quote.AcceptPercent = 99.5
	}
	if self.Quote.MaxInvoiceUsd <= 0 {
		self.Quote.MaxInvoiceUsd = 100
	}
	if self.Quote.RateCache <= 0 {
		self.Quote.RateCache = 60 * time.Second
	}
	if self.Quote.RateStaleAfter <= 0 {
		self.Quote.RateStaleAfter = 2 * time.Minute
	}
	if self.Quote.RateMaxSpreadPercent <= 0 {
		self.Quote.RateMaxSpreadPercent = 3
	}
	if self.AddressPoolTarget <= 0 {
		self.AddressPoolTarget = 200
	}
	if self.MaxOpenIntentsPerAsset <= 0 {
		self.MaxOpenIntentsPerAsset = 5000
	}
	if self.MaxOpenCodeIntentsPerAsset <= 0 {
		self.MaxOpenCodeIntentsPerAsset = 100
	}
	if self.ScanInterval <= 0 {
		self.ScanInterval = 20 * time.Second
	}
	if self.MaxLagBlocks == nil {
		self.MaxLagBlocks = map[string]int64{}
	}
	if self.MaxLagBlocks[model.CryptoAssetXmr] <= 0 {
		self.MaxLagBlocks[model.CryptoAssetXmr] = 5
	}
	if self.MaxLagBlocks[model.CryptoAssetZec] <= 0 {
		self.MaxLagBlocks[model.CryptoAssetZec] = 20
	}
	// TAO: snow's finalized head behind its best head (2-3 blocks normally)
	// plus the scanner's own lag, about 6 minutes of 12 s blocks
	if self.MaxLagBlocks[model.CryptoAssetTao] <= 0 {
		self.MaxLagBlocks[model.CryptoAssetTao] = 30
	}
	// Bitcoin and ETH: the scanner reads Alchemy's head itself, so its height
	// is the network's; a little slack for a block that lands mid-scan
	if self.MaxLagBlocks[model.CryptoAssetBtc] <= 0 {
		self.MaxLagBlocks[model.CryptoAssetBtc] = 2
	}
	if self.MaxLagBlocks[model.CryptoAssetEth] <= 0 {
		self.MaxLagBlocks[model.CryptoAssetEth] = 16
	}
	// SOL: the finalized slot behind the confirmed one, about 2 minutes of
	// 400 ms slots
	if self.MaxLagBlocks[model.CryptoAssetSol] <= 0 {
		self.MaxLagBlocks[model.CryptoAssetSol] = 300
	}
	if self.UnlinkAfter <= 0 {
		self.UnlinkAfter = 720 * time.Hour
	}
}

// Asset is a coin's policy, or nil when the file does not name it.
func (self *CryptoPayConfig) Asset(asset string) *CryptoPayAssetConfig {
	if self == nil || self.Assets == nil {
		return nil
	}
	return self.Assets[asset]
}

// FinalConfirmationsAt is the depth a receipt mined at height is re-checked
// at: the after-NU7 depth from the configured activation height on (zec).
func (self *CryptoPayAssetConfig) FinalConfirmationsAt(height int64) int64 {
	if 0 < self.Nu7ActivationHeight && self.Nu7ActivationHeight <= height && 0 < self.FinalConfirmationsAfterNu7 {
		return self.FinalConfirmationsAfterNu7
	}
	return self.FinalConfirmations
}

// MaxLagBlocksAt is how far a coin's wallet may lag the network at height
// before the coin is unavailable: max_lag_blocks for the coin, or for zec from
// the NU7 activation height on, max_lag_blocks_after_nu7.
func (self *CryptoPayConfig) MaxLagBlocksAt(asset string, height int64) int64 {
	if assetConfig := self.Asset(asset); assetConfig != nil &&
		0 < assetConfig.Nu7ActivationHeight && assetConfig.Nu7ActivationHeight <= height && 0 < assetConfig.MaxLagBlocksAfterNu7 {
		return assetConfig.MaxLagBlocksAfterNu7
	}
	return self.MaxLagBlocks[asset]
}

// ReorgWindowBlocksAt is how far a scan whose cursor is at height reaches
// back.
func (self *CryptoPayAssetConfig) ReorgWindowBlocksAt(height int64) int64 {
	if 0 < self.Nu7ActivationHeight && self.Nu7ActivationHeight <= height && 0 < self.ReorgWindowBlocksAfterNu7 {
		return self.ReorgWindowBlocksAfterNu7
	}
	return self.ReorgWindowBlocks
}

// SellsPlan reports whether the coin sells the plan. Monthly never.
func (self *CryptoPayAssetConfig) SellsPlan(plan string) bool {
	return self != nil && slices.Contains(self.Plans, plan) && slices.Contains(cryptoPayKnownPlans, plan)
}

// SellsItem reports whether the coin sells the data item.
func (self *CryptoPayAssetConfig) SellsItem(item string) bool {
	return self != nil && slices.Contains(self.Items, item) && slices.Contains(cryptoPayKnownItems, item)
}

var cryptoPayConfigOnce = sync.OnceValue(func() *CryptoPayConfig {
	// OPTIONAL: a missing file is every coin off, not a boot failure
	resource, err := server.Config.SimpleResource("crypto_pay.yml")
	if err != nil {
		glog.Infof("[cryptopay]crypto_pay.yml is not present; crypto payments are off. err = %s\n", err)
		config := &CryptoPayConfig{}
		config.normalize()
		return config
	}
	return parseCryptoPayConfig(resource)
})

// parseCryptoPayConfig reads one crypto_pay.yml. A malformed file runs with
// every coin off and says so.
func parseCryptoPayConfig(resource *server.SimpleResource) *CryptoPayConfig {
	var y cryptoPayYaml
	if err := resource.UnmarshalYamlE(&y); err != nil {
		glog.Errorf("[cryptopay]crypto_pay.yml could not be parsed; crypto payments are off. err = %s\n", err)
		config := &CryptoPayConfig{}
		config.normalize()
		return config
	}
	config := &y.CryptoPay
	config.normalize()
	return config
}

// BufferPercentFor is the quote buffer of a coin, percent.
func (self *CryptoPayConfig) BufferPercentFor(asset string) float64 {
	if assetConfig := self.Asset(asset); assetConfig != nil && 0 < assetConfig.BufferPercent {
		return assetConfig.BufferPercent
	}
	return self.Quote.BufferPercent
}

// ScanIntervalFor is how often a coin is scanned.
func (self *CryptoPayConfig) ScanIntervalFor(asset string) time.Duration {
	if assetConfig := self.Asset(asset); assetConfig != nil && 0 < assetConfig.ScanInterval {
		return assetConfig.ScanInterval
	}
	return self.ScanInterval
}

// AddressPoolTargetFor is the free addresses a coin's pool keeps.
func (self *CryptoPayConfig) AddressPoolTargetFor(asset string) int {
	if assetConfig := self.Asset(asset); assetConfig != nil && 0 < assetConfig.AddressPoolTarget {
		return assetConfig.AddressPoolTarget
	}
	return self.AddressPoolTarget
}

// MaxOpenCodeIntentsFor is the most open code-delivery invoices of a coin. A
// Coinbase coin's smaller pool allows at most half of it, so anonymous
// invoices cannot drain it.
func (self *CryptoPayConfig) MaxOpenCodeIntentsFor(asset string) int {
	if cryptoAssetCoinbase(asset) {
		return max(1, min(self.MaxOpenCodeIntentsPerAsset, self.AddressPoolTargetFor(asset)/2))
	}
	return self.MaxOpenCodeIntentsPerAsset
}

// WatchAfterFor is how long a per-address coin's closed invoice is still read.
func (self *CryptoPayConfig) WatchAfterFor(asset string) time.Duration {
	if assetConfig := self.Asset(asset); assetConfig != nil && 0 < assetConfig.WatchAfter {
		return assetConfig.WatchAfter
	}
	return cryptoDefaultWatchAfter
}

// The parsed crypto_pay.yml (cached for the process). Replaceable only by
// tests, which run against a config of their own; production never mutates it.
var cryptoPayConfigFunc = func() *CryptoPayConfig {
	return cryptoPayConfigOnce()
}

// setCryptoPayConfigForTest swaps the config for a test and returns the
// restore.
func setCryptoPayConfigForTest(config *CryptoPayConfig) func() {
	previous := cryptoPayConfigFunc
	if config != nil {
		config.normalize()
	}
	cryptoPayConfigFunc = func() *CryptoPayConfig {
		return config
	}
	return func() {
		cryptoPayConfigFunc = previous
	}
}

// ----- the wallet daemons' vault settings -----

// cryptoWalletSettings is a coin's wallet daemon connection, from the vault,
// or for a Coinbase coin its chain source and the Coinbase key. Never logged:
// every field may be a credential or carry one.
type cryptoWalletSettings struct {
	Asset          string
	WalletRpcUrl   string
	WalletRpcUser  string
	WalletRpcPass  string
	PrimaryAddress string
	// the Monero wallet FILE's password (not the RPC password), which
	// set_subaddress_lookahead asks for; needed to sell xmr
	WalletPassword string
	// the full node, for the node and network heights; needed to sell
	NodeRpcUrl  string
	NodeRpcUser string
	NodeRpcPass string

	// a Coinbase coin's chain source, which scanning needs: the Alchemy
	// endpoint (btc, eth, with the key in its path), the Helius RPC (sol, with
	// the key in its query), snow's Finney WebSocket (tao)
	ChainRpcUrl string
	// tao: the st profile the Finney connection was resolved for (mainnet,
	// testnet)
	ChainProfile string
	// eth: the Alchemy webhook, an optional fast path, and the Notify API
	// token that registers new addresses with it
	AlchemyWebhookId         string
	AlchemyWebhookSigningKey string
	AlchemyNotifyToken       string
	// a Coinbase coin's deposit addresses: the CDP key of coinbase.yml.
	// Selling needs it; nil when coinbase.yml does not name a complete key
	Coinbase *cryptoCoinbaseSettings
}

// cryptoCoinbaseSettings is the Coinbase App API key that creates deposit
// addresses (the view and receive scopes, mmm/BTCPAY.md §17.2).
type cryptoCoinbaseSettings struct {
	Host       string
	KeyName    string
	PrivateKey string
}

// The vault resource and its top-level key, per scanned coin.
func cryptoWalletVaultResource(asset string) (resource string, root string, ok bool) {
	switch asset {
	case model.CryptoAssetXmr:
		return "monero.yml", "monero", true
	case model.CryptoAssetZec:
		return "zcash.yml", "zcash", true
	case model.CryptoAssetBtc, model.CryptoAssetEth:
		return "alchemy.yml", "alchemy", true
	case model.CryptoAssetSol:
		return "helius.yml", "helius", true
	case model.CryptoAssetTao:
		return st.VaultResourceName, "", true
	}
	return "", "", false
}

// The Alchemy endpoints built from the api key when alchemy.yml gives none.
var cryptoAlchemyDefaultEndpoints = map[string]string{
	model.CryptoAssetEth: "https://eth-mainnet.g.alchemy.com/v2/",
	model.CryptoAssetBtc: "https://bitcoin-mainnet.g.alchemy.com/v2/",
}

// The Helius RPC, with the api key in its query.
const cryptoHeliusRpcBaseUrl = "https://mainnet.helius-rpc.com/?api-key="

// The snow Finney connection, resolved from st.yml. Replaceable only by tests.
var cryptoTaoConnection = func() (wsUrl string, profile string, err error) {
	profile, err = st.ActiveProfile()
	if err != nil {
		return "", "", err
	}
	connection, err := st.GetConnection()
	if err != nil {
		return "", "", err
	}
	if connection.WsUrl == "" {
		return "", "", fmt.Errorf("st.yml names no subtensor WebSocket")
	}
	return connection.WsUrl, profile, nil
}

// cryptoCoinbaseSettingsFromVault reads the Coinbase key, or why it is not
// complete. The error names the path, never a value.
func cryptoCoinbaseSettingsFromVault() (*cryptoCoinbaseSettings, error) {
	resource, err := server.Vault.SimpleResource("coinbase.yml")
	if err != nil {
		return nil, err
	}
	required := func(path ...string) (string, error) {
		value, err := simpleResourceNonblankStringValue(resource, path...)
		if err != nil {
			return "", fmt.Errorf("coinbase.yml: %w", err)
		}
		return strings.TrimSpace(value), nil
	}
	settings := &cryptoCoinbaseSettings{}
	if settings.Host, err = required("api", "host"); err != nil {
		return nil, err
	}
	if settings.KeyName, err = required("api", "key_name"); err != nil {
		return nil, err
	}
	// a PEM keeps its line breaks: only the surrounding space is trimmed
	if settings.PrivateKey, err = required("api", "private_key"); err != nil {
		return nil, err
	}
	return settings, nil
}

// cryptoChainSettingsFromVault reads a Coinbase coin's chain source and,
// when complete, its Coinbase key. An error is a chain source that is
// missing: the coin is neither scanned nor sold. A missing Coinbase key
// leaves the coin scanned (its open invoices are still credited) but not
// sold.
func cryptoChainSettingsFromVault(asset string) (*cryptoWalletSettings, error) {
	settings := &cryptoWalletSettings{Asset: asset}
	switch asset {
	case model.CryptoAssetBtc, model.CryptoAssetEth:
		resource, err := server.Vault.SimpleResource("alchemy.yml")
		if err != nil {
			return nil, err
		}
		optional := func(path ...string) string {
			value, err := simpleResourceNonblankStringValue(resource, path...)
			if err != nil {
				return ""
			}
			return strings.TrimSpace(value)
		}
		endpointKey := "eth_endpoint"
		if asset == model.CryptoAssetBtc {
			endpointKey = "btc_endpoint"
		}
		settings.ChainRpcUrl = optional("alchemy", endpointKey)
		if settings.ChainRpcUrl == "" {
			apiKey := optional("alchemy", "api_key")
			if apiKey == "" {
				return nil, fmt.Errorf("alchemy.yml: alchemy.%s and alchemy.api_key are missing", endpointKey)
			}
			settings.ChainRpcUrl = cryptoAlchemyDefaultEndpoints[asset] + apiKey
		}
		if !strings.HasPrefix(settings.ChainRpcUrl, "https://") {
			return nil, fmt.Errorf("alchemy.yml: alchemy.%s is not an https url", endpointKey)
		}
		if asset == model.CryptoAssetEth {
			// the ETH webhook, a fast path only; Bitcoin has none
			settings.AlchemyWebhookId = optional("alchemy", "eth_webhook_id")
			settings.AlchemyWebhookSigningKey = optional("alchemy", "eth_webhook_signing_key")
			settings.AlchemyNotifyToken = optional("alchemy", "notify_auth_token")
		}
	case model.CryptoAssetSol:
		apiKey, err := vaultNonblankString("helius.yml", "helius", "api_key")
		if err != nil {
			return nil, err
		}
		settings.ChainRpcUrl = cryptoHeliusRpcBaseUrl + strings.TrimSpace(apiKey)
	case model.CryptoAssetTao:
		wsUrl, profile, err := cryptoTaoConnection()
		if err != nil {
			return nil, fmt.Errorf("st.yml: %w", err)
		}
		settings.ChainRpcUrl = wsUrl
		settings.ChainProfile = profile
	default:
		return nil, fmt.Errorf("%s has no chain source", asset)
	}
	if coinbase, err := cryptoCoinbaseSettingsFromVault(); err == nil {
		settings.Coinbase = coinbase
	}
	return settings, nil
}

// cryptoWalletSettingsFromVault reads a coin's daemon settings, or why they
// are unusable. The error names the resource and the path, never a value.
func cryptoWalletSettingsFromVault(asset string) (*cryptoWalletSettings, error) {
	if cryptoAssetCoinbase(asset) {
		return cryptoChainSettingsFromVault(asset)
	}
	resourceName, root, ok := cryptoWalletVaultResource(asset)
	if !ok {
		return nil, fmt.Errorf("%s has no wallet daemon", asset)
	}
	resource, err := server.Vault.SimpleResource(resourceName)
	if err != nil {
		return nil, err
	}
	required := func(path ...string) (string, error) {
		value, err := simpleResourceNonblankStringValue(resource, path...)
		if err != nil {
			return "", fmt.Errorf("%s: %w", resourceName, err)
		}
		return strings.TrimSpace(value), nil
	}
	optional := func(path ...string) string {
		value, err := simpleResourceNonblankStringValue(resource, path...)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(value)
	}
	settings := &cryptoWalletSettings{Asset: asset}
	if settings.WalletRpcUrl, err = required(root, "wallet_rpc_url"); err != nil {
		return nil, err
	}
	if settings.WalletRpcUser, err = required(root, "wallet_rpc_user"); err != nil {
		return nil, err
	}
	if settings.WalletRpcPass, err = required(root, "wallet_rpc_password"); err != nil {
		return nil, err
	}
	switch asset {
	case model.CryptoAssetXmr:
		if settings.PrimaryAddress, err = required(root, "primary_address"); err != nil {
			return nil, err
		}
		settings.WalletPassword = optional(root, "wallet_password")
		settings.NodeRpcUrl = optional(root, "daemon_rpc_url")
		settings.NodeRpcUser = optional(root, "daemon_rpc_user")
		settings.NodeRpcPass = optional(root, "daemon_rpc_password")
	case model.CryptoAssetZec:
		settings.NodeRpcUrl = optional(root, "node_rpc_url")
		settings.NodeRpcUser = optional(root, "node_rpc_user")
		settings.NodeRpcPass = optional(root, "node_rpc_password")
	}
	return settings, nil
}

// The daemon settings seam. Replaceable only by tests; production reads the
// vault.
var cryptoWalletSettingsFunc = cryptoWalletSettingsFromVault

// cryptoWalletSettingsSellable is what selling a coin needs from its vault
// file beyond scanning: for xmr the wallet file's password (the lookahead)
// and monerod, for zec zebrad (the network height the lag is judged
// against), except on regtest, whose zebrad extrapolates a meaningless
// height and where zecd's view of the tip stands in.
func cryptoWalletSettingsSellable(asset string, settings *cryptoWalletSettings, network string) error {
	switch asset {
	case model.CryptoAssetXmr:
		if settings.WalletPassword == "" {
			return fmt.Errorf("monero.yml: monero.wallet_password is missing (set_subaddress_lookahead needs it)")
		}
		if settings.NodeRpcUrl == "" {
			return fmt.Errorf("monero.yml: monero.daemon_rpc_url is missing (the lag check needs monerod)")
		}
	case model.CryptoAssetZec:
		if settings.NodeRpcUrl == "" && network != cryptoNetworkRegtest {
			return fmt.Errorf("zcash.yml: zcash.node_rpc_url is missing (the lag check needs zebrad)")
		}
	case model.CryptoAssetTao, model.CryptoAssetBtc, model.CryptoAssetEth, model.CryptoAssetSol:
		if settings.ChainRpcUrl == "" {
			return fmt.Errorf("%s has no chain source", asset)
		}
		if settings.Coinbase == nil {
			return fmt.Errorf("coinbase.yml: api.host, api.key_name and api.private_key are not all set (the deposit addresses need them)")
		}
		if asset == model.CryptoAssetTao && settings.ChainProfile != network {
			return fmt.Errorf("st.yml: the subtensor connection serves %s, the coin's network is %s", settings.ChainProfile, network)
		}
	}
	return nil
}

// ----- the kill switch -----

// cryptoAssetEnabled reports whether a coin is sold: switched on in the
// config and, for the scanned coins, with everything selling needs in the
// vault. This is the kill switch, and it gates selling only: a coin that is
// off is not listed, quoted or re-quoted, but its scanner, credits and
// reconcile legs go on (cryptoAssetScannable).
func cryptoAssetEnabled(config *CryptoPayConfig, asset string) bool {
	return cryptoAssetSwitchedOn(config, asset) && cryptoAssetConfigured(config, asset) == nil
}

// cryptoAssetSwitchedOn reports a coin the config switches on, whatever the
// vault holds.
func cryptoAssetSwitchedOn(config *CryptoPayConfig, asset string) bool {
	assetConfig := config.Asset(asset)
	return assetConfig != nil && assetConfig.Enabled
}

// cryptoAssetConfigured is nil when the vault names everything selling the
// coin needs, else why not (the resource and path, never a value). USDC needs
// nothing beyond its webhook. A coin switched on but not configured is listed
// as unavailable (not_configured) and not sold; the reason is logged once a
// minute.
func cryptoAssetConfigured(config *CryptoPayConfig, asset string) error {
	if !slices.Contains(cryptoPayScannedAssets, asset) {
		return nil
	}
	assetConfig := config.Asset(asset)
	if assetConfig == nil {
		return fmt.Errorf("%s has no policy", asset)
	}
	settings, err := cryptoWalletSettingsFunc(asset)
	if err == nil {
		err = cryptoWalletSettingsSellable(asset, settings, assetConfig.Network)
	}
	if err != nil {
		if assetConfig.Enabled {
			cryptoPayVaultReports.report(asset, func() {
				glog.Warningf("[cryptopay]%s is switched on but its vault file is not complete, so it is not sold: %s\n", asset, err)
			})
		}
		return err
	}
	return nil
}

// cryptoAssetScannable reports whether a scanned coin's wallet daemon is
// configured, so its scanner runs, its payments are credited and its
// reconcile legs check them, whether or not the coin is sold.
func cryptoAssetScannable(config *CryptoPayConfig, asset string) error {
	if !slices.Contains(cryptoPayScannedAssets, asset) {
		return fmt.Errorf("%s is not scanned", asset)
	}
	if config.Asset(asset) == nil {
		return fmt.Errorf("%s has no policy", asset)
	}
	_, err := cryptoWalletSettingsFunc(asset)
	return err
}

// One default-level report a minute per coin of a coin switched on without
// its daemon.
var cryptoPayVaultReports = newCryptoPayKeyedReport(time.Minute)

// cryptoPayKeyedReport bounds a default-level report to one per interval per
// key, so a misconfigured coin is said once a minute, not on every request.
type cryptoPayKeyedReport struct {
	interval  time.Duration
	stateLock sync.Mutex
	last      map[string]time.Time
}

func newCryptoPayKeyedReport(interval time.Duration) *cryptoPayKeyedReport {
	return &cryptoPayKeyedReport{interval: interval, last: map[string]time.Time{}}
}

func (self *cryptoPayKeyedReport) report(key string, report func()) {
	self.stateLock.Lock()
	now := time.Now()
	due := self.last[key].IsZero() || self.interval <= now.Sub(self.last[key])
	if due {
		self.last[key] = now
	}
	self.stateLock.Unlock()
	if due {
		report()
	}
}

// cryptoEnabledAssets lists the coins that are on, in the UI order.
func cryptoEnabledAssets(config *CryptoPayConfig) []string {
	assets := []string{}
	for _, asset := range cryptoPayAssets {
		if cryptoAssetEnabled(config, asset) {
			assets = append(assets, asset)
		}
	}
	return assets
}

// cryptoAssetMarket is the subscription market a coin's plan credits under.
func cryptoAssetMarket(asset string) string {
	switch asset {
	case model.CryptoAssetXmr:
		return model.SubscriptionMarketMonero
	case model.CryptoAssetZec:
		return model.SubscriptionMarketZcash
	case model.CryptoAssetUsdcSolana, model.CryptoAssetSol:
		return model.SubscriptionMarketSolana
	case model.CryptoAssetTao:
		return model.SubscriptionMarketBittensor
	case model.CryptoAssetBtc:
		return model.SubscriptionMarketBitcoin
	case model.CryptoAssetEth:
		return model.SubscriptionMarketEthereum
	}
	return ""
}

// cryptoAssetOnboardingStore is the store a welcome offer bought with the coin
// is redeemed on.
func cryptoAssetOnboardingStore(asset string) string {
	switch asset {
	case model.CryptoAssetXmr:
		return model.OnboardingStoreMonero
	case model.CryptoAssetZec:
		return model.OnboardingStoreZcash
	case model.CryptoAssetUsdcSolana, model.CryptoAssetSol:
		return model.OnboardingStoreSolana
	case model.CryptoAssetTao:
		return model.OnboardingStoreBittensor
	case model.CryptoAssetBtc:
		return model.OnboardingStoreBitcoin
	case model.CryptoAssetEth:
		return model.OnboardingStoreEthereum
	}
	return ""
}

// The decimals of a coin's atomic unit: piconero, zatoshi, micro-USDC, rao,
// satoshi, wei, lamport (mmm/BTCPAY.md §17.7).
func cryptoAssetDecimals(asset string) int {
	switch asset {
	case model.CryptoAssetXmr:
		return 12
	case model.CryptoAssetZec:
		return 8
	case model.CryptoAssetUsdcSolana:
		return 6
	case model.CryptoAssetTao:
		return 9
	case model.CryptoAssetBtc:
		return 8
	case model.CryptoAssetEth:
		return 18
	case model.CryptoAssetSol:
		return 9
	}
	return 0
}

// cryptoAssetNetwork is the chain a coin is paid on.
func cryptoAssetNetwork(asset string) string {
	switch asset {
	case model.CryptoAssetUsdcSolana, model.CryptoAssetSol:
		return "solana"
	case model.CryptoAssetZec:
		return "zcash"
	case model.CryptoAssetXmr:
		return "monero"
	case model.CryptoAssetTao:
		return "bittensor"
	case model.CryptoAssetBtc:
		return "bitcoin"
	case model.CryptoAssetEth:
		return "ethereum"
	}
	return ""
}

// cryptoAssetPaymentLink reports a coin whose invoices carry a payment uri:
// every coin but TAO, which has no payment link standard.
func cryptoAssetPaymentLink(asset string) bool {
	return asset != model.CryptoAssetTao && cryptoAssetNetwork(asset) != ""
}

// cryptoAssetNetworkWarning is what to send and on which network, in English,
// for a person or an agent paying an invoice by hand.
func cryptoAssetNetworkWarning(asset string) string {
	switch asset {
	case model.CryptoAssetUsdcSolana:
		return "Send USDC on Solana (the USDC token, mint " + solanaUsdcMint + ") to this address, exactly the amount. USDC on another network, or SOL, is not credited. A transfer sent by hand carries the memo."
	case model.CryptoAssetZec:
		return "Send ZEC to this shielded address. ZEC sent from a transparent address is credited; ZEC sent to a transparent address is not. A payment needs 10 confirmations."
	case model.CryptoAssetXmr:
		return "Send XMR to this subaddress, exactly the amount."
	case model.CryptoAssetTao:
		return "Send TAO on the Bittensor network (Finney) to this address, exactly the amount. TAO has no memo: the address alone identifies this invoice. TAO sent on another network or as a wrapped token is not credited."
	case model.CryptoAssetBtc:
		return "Send BTC on the Bitcoin network to this address, exactly the amount. BTC on another network (Lightning, Base, Ethereum, Solana) is not credited. A payment needs 1 confirmation."
	case model.CryptoAssetEth:
		return "Send ETH on Ethereum mainnet to this address, exactly the amount. ETH on another network (Base, Arbitrum, Optimism) or a token is not credited. A payment is credited once its block is finalized, about 15 minutes."
	case model.CryptoAssetSol:
		return "Send native SOL on Solana to this address, exactly the amount. A token (SPL) transfer is not credited. The address alone identifies this invoice: no memo is needed."
	}
	return ""
}

// cryptoAssetUriNote is why an invoice of the coin has no payment uri, or "".
func cryptoAssetUriNote(asset string) string {
	if asset == model.CryptoAssetTao {
		return "TAO has no payment link standard: copy the address and send exactly the amount."
	}
	return ""
}

// CryptoAssetDecimals is cryptoAssetDecimals for the operator tools.
func CryptoAssetDecimals(asset string) int {
	return cryptoAssetDecimals(asset)
}

// The decimals a coin's quoted amount is rounded up to: 6 for every coin but
// Bitcoin, which is quoted to the satoshi (mmm/BTCPAY.md §17.7). Exchanges
// send TAO at 8 decimals at most, so 6 is short enough to send as quoted.
func cryptoAssetQuoteDecimals(asset string) int {
	if asset == model.CryptoAssetBtc {
		return 8
	}
	return cryptoQuoteDecimals
}

// cryptoAssetQuoteTtl is how long a coin's quote holds: the config's expiry
// for the scanned coins, the plan intent's hour for USDC (the amount is in
// dollars and its unique suffix stays reserved long after).
func cryptoAssetQuoteTtl(config *CryptoPayConfig, asset string) time.Duration {
	if asset == model.CryptoAssetUsdcSolana {
		return solanaPlanIntentDuration
	}
	return config.Quote.Expiry
}
