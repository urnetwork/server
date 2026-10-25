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

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The items a coin can sell. Monthly is card only (decision 6): no coin sells
// it, whatever the config lists.
var cryptoPayKnownPlans = []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding}
var cryptoPayKnownItems = []string{StripeItemData1Tib, StripeItemData10Tib}

// The coins, in the order every UI shows them.
var cryptoPayAssets = []string{model.CryptoAssetUsdcSolana, model.CryptoAssetZec, model.CryptoAssetXmr}

// The coins the scanner watches: USDC arrives by webhook.
var cryptoPayScannedAssets = []string{model.CryptoAssetZec, model.CryptoAssetXmr}

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
	cryptoRateSourceKraken    = "kraken"
	cryptoRateSourceCoingecko = "coingecko"
	cryptoRateSourceCoinbase  = "coinbase"
)

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
}

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
	// a scanned coin the file leaves out is off, with the default policy: its
	// scanner still runs when its daemon is in the vault
	for _, asset := range cryptoPayScannedAssets {
		if self.Assets[asset] == nil {
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
			if len(assetConfig.RateSources) == 0 {
				assetConfig.RateSources = []string{cryptoRateSourceKraken, cryptoRateSourceCoingecko}
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
			if len(assetConfig.RateSources) == 0 {
				assetConfig.RateSources = []string{cryptoRateSourceKraken, cryptoRateSourceCoingecko, cryptoRateSourceCoinbase}
			}
		case model.CryptoAssetUsdcSolana:
			// Helius reports confirmed transfers only
			assetConfig.CreditConfirmations = 1
			assetConfig.FinalConfirmations = 1
		}
		if assetConfig.Network == "" {
			assetConfig.Network = cryptoNetworkMainnet
		}
		if asset == model.CryptoAssetUsdcSolana {
			// the Helius webhook watches mainnet only
			assetConfig.Network = cryptoNetworkMainnet
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

// cryptoWalletSettings is a coin's wallet daemon connection, from the vault.
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
}

// The vault resource and its top-level key, per scanned coin.
func cryptoWalletVaultResource(asset string) (resource string, root string, ok bool) {
	switch asset {
	case model.CryptoAssetXmr:
		return "monero.yml", "monero", true
	case model.CryptoAssetZec:
		return "zcash.yml", "zcash", true
	}
	return "", "", false
}

// cryptoWalletSettingsFromVault reads a coin's daemon settings, or why they
// are unusable. The error names the resource and the path, never a value.
func cryptoWalletSettingsFromVault(asset string) (*cryptoWalletSettings, error) {
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
	assetConfig := config.Asset(asset)
	if assetConfig == nil || !assetConfig.Enabled {
		return false
	}
	if !slices.Contains(cryptoPayScannedAssets, asset) {
		return true
	}
	settings, err := cryptoWalletSettingsFunc(asset)
	if err == nil {
		err = cryptoWalletSettingsSellable(asset, settings, assetConfig.Network)
	}
	if err != nil {
		cryptoPayVaultReports.report(asset, func() {
			glog.Warningf("[cryptopay]%s is switched on but its vault file is not complete, so it is not sold: %s\n", asset, err)
		})
		return false
	}
	return true
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
	case model.CryptoAssetUsdcSolana:
		return model.SubscriptionMarketSolana
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
	case model.CryptoAssetUsdcSolana:
		return model.OnboardingStoreSolana
	}
	return ""
}

// The decimals of a coin's atomic unit: piconero, zatoshi, micro-USDC.
func cryptoAssetDecimals(asset string) int {
	switch asset {
	case model.CryptoAssetXmr:
		return 12
	case model.CryptoAssetZec:
		return 8
	case model.CryptoAssetUsdcSolana:
		return 6
	}
	return 0
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
