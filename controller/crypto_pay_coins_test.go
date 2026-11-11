package controller

// The coins received into our Coinbase account (mmm/BTCPAY.md §17): their
// policy, quotes, payment links, rate sources, availability and address
// forms. Hermetic: no database, no network; the rate sources and the chain
// readers answer recorded fixtures (testdata/crypto_pay).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// cryptoTestFixture reads a recorded fixture of testdata/crypto_pay.
func cryptoTestFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "crypto_pay", name))
	if err != nil {
		t.Fatalf("fixture %s: %s", name, err)
	}
	return data
}

// A crypto_pay.yml as config/main writes it: the three coins of the rail's
// launch, with CoinGecko still named for both scanned coins, and none of the
// Coinbase coins.
const cryptoTestMainConfigYaml = `
crypto_pay:
  assets:
    usdc_solana:
      enabled: true
      plans: [yearly, yearly_onboarding]
      items: [data_1tib, data_10tib]
    xmr:
      enabled: true
      network: mainnet
      plans: [yearly, yearly_onboarding]
      items: [data_1tib, data_10tib]
      credit_confirmations: 1
      final_confirmations: 10
      reorg_window_blocks: 10
      rate_sources: [kraken, coingecko]
    zec:
      enabled: true
      network: mainnet
      plans: [yearly, yearly_onboarding]
      items: [data_1tib, data_10tib]
      credit_confirmations: 10
      final_confirmations: 24
      reorg_window_blocks: 24
      rate_sources: [kraken, coingecko, coinbase]
  quote:
    expiry: 30m
    buffer_percent: 0.5
  address_pool_target: 200
  scan_interval: 20s
`

// cryptoTestMainConfigWithCoinbaseCoins is the production file with the
// entries that switch TAO, Bitcoin, ETH and SOL on.
func cryptoTestMainConfigWithCoinbaseCoins() string {
	return strings.Replace(cryptoTestMainConfigYaml, "  assets:\n", "  assets:\n"+cryptoTestCoinbaseEntriesYaml, 1)
}

func cryptoTestParseConfig(t testing.TB, yaml string) *CryptoPayConfig {
	t.Helper()
	pop := server.Config.PushSimpleResource("crypto_pay.yml", []byte(yaml))
	t.Cleanup(pop)
	resource, err := server.Config.SimpleResource("crypto_pay.yml")
	if err != nil {
		t.Fatal(err)
	}
	return parseCryptoPayConfig(resource)
}

// The entries that switch the Coinbase coins on: what each sells, and nothing
// else (the coin's own defaults do the rest).
const cryptoTestCoinbaseEntriesYaml = `    tao:
      enabled: true
      plans: [yearly, yearly_onboarding]
      items: [data_1tib, data_10tib]
    btc:
      enabled: true
      plans: [yearly, yearly_onboarding]
      items: [data_1tib, data_10tib]
    eth:
      enabled: true
      plans: [yearly, yearly_onboarding]
      items: [data_1tib, data_10tib]
    sol:
      enabled: true
      plans: [yearly, yearly_onboarding]
      items: [data_1tib, data_10tib]
`

// A crypto_pay.yml that leaves the Coinbase coins out has no policy for them:
// they are not listed, sold, scanned or reconciled, and no deposit address is
// asked of Coinbase, until the release that switches a coin on names it. A
// file that names them with what they sell gets each coin's own decimals,
// buffers, depths and cadences (mmm/BTCPAY.md §17.4, §17.7). CoinGecko is
// dropped for every coin but Monero, which keeps it as its second source.
func TestCryptoCoinbaseCoinsWaitForTheirEntry(t *testing.T) {
	left := cryptoTestParseConfig(t, cryptoTestMainConfigYaml)
	for _, asset := range cryptoPayCoinbaseAssets {
		connect.AssertEqual(t, left.Asset(asset), (*CryptoPayAssetConfig)(nil))
		connect.AssertEqual(t, cryptoAssetSwitchedOn(left, asset), false)
		connect.AssertEqual(t, cryptoAssetScannable(left, asset) != nil, true)
	}
	// the wallet-daemon coins keep their policies as the file names them
	connect.AssertEqual(t, left.Asset(model.CryptoAssetXmr).Enabled, true)

	config := cryptoTestParseConfig(t, cryptoTestMainConfigWithCoinbaseCoins())
	for _, asset := range []string{model.CryptoAssetTao, model.CryptoAssetBtc, model.CryptoAssetEth, model.CryptoAssetSol} {
		assetConfig := config.Asset(asset)
		if assetConfig == nil {
			t.Fatalf("%s has no policy", asset)
		}
		connect.AssertEqual(t, assetConfig.Enabled, true)
		connect.AssertEqual(t, assetConfig.Plans, []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding})
		connect.AssertEqual(t, assetConfig.Items, []string{StripeItemData1Tib, StripeItemData10Tib})
		connect.AssertEqual(t, assetConfig.Network, cryptoNetworkMainnet)
		connect.AssertEqual(t, assetConfig.RateSources, []string{cryptoRateSourceKraken, cryptoRateSourceCoinbase})
		connect.AssertEqual(t, config.AddressPoolTargetFor(asset), cryptoCoinbasePoolTarget)
		connect.AssertEqual(t, config.WatchAfterFor(asset), 24*time.Hour)
		connect.AssertEqual(t, assetConfig.SellsPlan(model.SolanaPlanMonthly), false)
	}
	// decimals and quote decimals (§17.7)
	connect.AssertEqual(t, cryptoAssetDecimals(model.CryptoAssetTao), 9)
	connect.AssertEqual(t, cryptoAssetDecimals(model.CryptoAssetBtc), 8)
	connect.AssertEqual(t, cryptoAssetDecimals(model.CryptoAssetEth), 18)
	connect.AssertEqual(t, cryptoAssetDecimals(model.CryptoAssetSol), 9)
	connect.AssertEqual(t, cryptoAssetQuoteDecimals(model.CryptoAssetTao), 6)
	connect.AssertEqual(t, cryptoAssetQuoteDecimals(model.CryptoAssetBtc), 8)
	connect.AssertEqual(t, cryptoAssetQuoteDecimals(model.CryptoAssetEth), 6)
	connect.AssertEqual(t, cryptoAssetQuoteDecimals(model.CryptoAssetSol), 6)
	// buffers: 1% for TAO, the rail's 0.5% for the others
	connect.AssertEqual(t, config.BufferPercentFor(model.CryptoAssetTao), 1.0)
	connect.AssertEqual(t, config.BufferPercentFor(model.CryptoAssetBtc), 0.5)
	connect.AssertEqual(t, config.BufferPercentFor(model.CryptoAssetEth), 0.5)
	connect.AssertEqual(t, config.BufferPercentFor(model.CryptoAssetSol), 0.5)
	connect.AssertEqual(t, config.BufferPercentFor(model.CryptoAssetXmr), 0.5)
	// depths: TAO, ETH and SOL at finality, Bitcoin at 1 and final at 6
	connect.AssertEqual(t, config.Asset(model.CryptoAssetTao).CreditConfirmations, int64(1))
	connect.AssertEqual(t, config.Asset(model.CryptoAssetEth).CreditConfirmations, int64(1))
	connect.AssertEqual(t, config.Asset(model.CryptoAssetSol).CreditConfirmations, int64(1))
	connect.AssertEqual(t, config.Asset(model.CryptoAssetBtc).CreditConfirmations, int64(1))
	connect.AssertEqual(t, config.Asset(model.CryptoAssetBtc).FinalConfirmations, int64(6))
	// cadences: Bitcoin every minute, ETH every 30 s, the others every scan_interval
	connect.AssertEqual(t, config.ScanIntervalFor(model.CryptoAssetBtc), time.Minute)
	connect.AssertEqual(t, config.ScanIntervalFor(model.CryptoAssetEth), 30*time.Second)
	connect.AssertEqual(t, config.ScanIntervalFor(model.CryptoAssetTao), 20*time.Second)
	connect.AssertEqual(t, config.ScanIntervalFor(model.CryptoAssetSol), 20*time.Second)
	// rate sources: CoinGecko only for Monero
	connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).RateSources, []string{cryptoRateSourceKraken, cryptoRateSourceCoinbase})
	connect.AssertEqual(t, config.Asset(model.CryptoAssetXmr).RateSources, []string{cryptoRateSourceKraken, cryptoRateSourceCoingecko})
	// the launch coins keep their pool and code limits
	connect.AssertEqual(t, config.AddressPoolTargetFor(model.CryptoAssetXmr), 200)
	connect.AssertEqual(t, config.MaxOpenCodeIntentsFor(model.CryptoAssetXmr), 100)
	connect.AssertEqual(t, config.MaxOpenCodeIntentsFor(model.CryptoAssetBtc), cryptoCoinbasePoolTarget/2)

	// a file that names a coin off keeps its own switch: not sold, but its
	// open invoices are still scanned and credited; the coins it leaves out
	// stay dormant
	named := cryptoTestParseConfig(t, strings.Replace(cryptoTestMainConfigYaml, "  assets:\n", "  assets:\n    btc:\n      enabled: false\n", 1))
	connect.AssertEqual(t, named.Asset(model.CryptoAssetBtc).Enabled, false)
	connect.AssertEqual(t, named.Asset(model.CryptoAssetTao), (*CryptoPayAssetConfig)(nil))

	// no file at all is every coin off, and the Coinbase coins dormant
	empty := &CryptoPayConfig{}
	empty.normalize()
	connect.AssertEqual(t, empty.Asset(model.CryptoAssetXmr).Enabled, false)
	connect.AssertEqual(t, empty.Asset(model.CryptoAssetBtc), (*CryptoPayAssetConfig)(nil))
	connect.AssertEqual(t, empty.Asset(model.CryptoAssetTao), (*CryptoPayAssetConfig)(nil))
}

// The production file, as committed in the config repo, reads the same way:
// a Coinbase coin it names is on mainnet with the coin's own defaults, and
// one it leaves out is dormant until the release that switches it on.
func TestCryptoMainConfigReadsTheCoinbaseCoins(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "main", "crypto_pay.yml"))
	if err != nil {
		t.Skipf("config/main is not beside the server checkout: %s", err)
	}
	config := cryptoTestParseConfig(t, string(data))
	for _, asset := range cryptoPayCoinbaseAssets {
		if assetConfig := config.Asset(asset); assetConfig != nil {
			connect.AssertEqual(t, assetConfig.Network, cryptoNetworkMainnet)
			connect.AssertEqual(t, config.AddressPoolTargetFor(asset), cryptoCoinbasePoolTarget)
		} else {
			connect.AssertEqual(t, cryptoAssetScannable(config, asset) != nil, true)
		}
	}
	connect.AssertEqual(t, config.Asset(model.CryptoAssetXmr).RateSources, []string{cryptoRateSourceKraken, cryptoRateSourceCoingecko})
}

// Quotes at each coin's decimals: Bitcoin to the satoshi, the others to 6
// decimals, TAO with its 1% buffer.
func TestCryptoCoinbaseCoinsQuoteMath(t *testing.T) {
	config := cryptoTestParseConfig(t, cryptoTestMainConfigWithCoinbaseCoins())
	quote := func(asset string, priceUsd float64, rateUsd float64) int64 {
		t.Helper()
		amount, ok := cryptoAssetQuoteAmountAtomic(asset, priceUsd, rateUsd, config.BufferPercentFor(asset))
		connect.AssertEqual(t, ok, true)
		return amount
	}
	// $40 at $73,596.90 with 0.5%: 0.00054621... BTC, up to the satoshi
	connect.AssertEqual(t, quote(model.CryptoAssetBtc, 40, 73596.90), int64(54622))
	connect.AssertEqual(t, cryptoFormatAtomic(54622, 8), "0.00054622")
	// $40 at $218.6068 with 1%: 0.184806... TAO, up to 6 decimals, in rao
	connect.AssertEqual(t, quote(model.CryptoAssetTao, 40, 218.6068), int64(184807000))
	connect.AssertEqual(t, cryptoFormatAtomic(184807000, 9), "0.184807")
	// $3 at $2,044.90: 0.001474... ETH, in wei
	connect.AssertEqual(t, quote(model.CryptoAssetEth, 3, 2044.90), int64(1475000000000000))
	connect.AssertEqual(t, cryptoFormatAtomic(1475000000000000, 18), "0.001475")
	// $40 at $82.89: 0.484979... SOL, in lamports
	connect.AssertEqual(t, quote(model.CryptoAssetSol, 40, 82.89), int64(484981000))
	// an ETH amount past what a bigint holds is refused, never wrapped
	_, ok := cryptoAssetQuoteAmountAtomic(model.CryptoAssetEth, 100, 1, 0.5)
	connect.AssertEqual(t, ok, false)
}

// cryptoTestCoinIntent is an open invoice of a coin at an address of the
// coin's real shape.
func cryptoTestCoinIntent(asset string, address string, amountAtomic int64) *model.CryptoPaymentIntent {
	networkId := server.NewId()
	index := int64(7)
	now := server.NowUtc()
	return &model.CryptoPaymentIntent{
		IntentId:                 server.NewId(),
		Asset:                    asset,
		Delivery:                 model.CryptoDeliveryAccount,
		NetworkId:                &networkId,
		Item:                     model.SolanaPlanYearly,
		PriceUsdNanoCents:        model.UsdToNanoCents(40),
		RegularPriceUsdNanoCents: model.UsdToNanoCents(40),
		Address:                  address,
		AddressIndex:             &index,
		AmountAtomic:             amountAtomic,
		RateUsd:                  1,
		QuotedAt:                 now,
		ExpiresAt:                now.Add(30 * time.Minute),
		CreateTime:               now,
	}
}

// Every link the server builds passes the sdk's own check (sdk
// CheckCryptoPaymentUri), byte for byte against the invoice's address, and
// TAO has none.
func TestCryptoPaymentLinksPassTheSdkCheck(t *testing.T) {
	reference := cryptoNewSolanaReference()
	cases := []struct {
		asset   string
		address string
		amount  int64
		uri     string
	}{
		{model.CryptoAssetBtc, "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u", 54622,
			"bitcoin:bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u?amount=0.00054622&label=URnetwork&message=URnetwork%20Pro%20yearly"},
		{model.CryptoAssetEth, "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045", 1475000000000000,
			"ethereum:0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045@1?value=1475000000000000"},
		{model.CryptoAssetSol, "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM", 485001000, ""},
		{model.CryptoAssetXmr, "8" + strings.Repeat("A", 94), 72386000000, ""},
		{model.CryptoAssetZec, "u1" + strings.Repeat("q", 60), 3070000, ""},
		{model.CryptoAssetUsdcSolana, "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM", 40004317, ""},
	}
	for _, c := range cases {
		intent := cryptoTestCoinIntent(c.asset, c.address, c.amount)
		if c.asset == model.CryptoAssetUsdcSolana {
			intent.SolanaReference = &reference
		}
		uri := cryptoPaymentUri(intent)
		if c.uri != "" {
			connect.AssertEqual(t, uri, c.uri)
		}
		if !sdk.CheckCryptoPaymentUri(c.asset, uri, c.address) {
			t.Errorf("the sdk refuses the %s link %q", c.asset, uri)
		}
	}
	// SOL: no spl-token, the invoice's reference, which is its memo
	sol := cryptoTestCoinIntent(model.CryptoAssetSol, "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM", 485001000)
	solUri := cryptoPaymentUri(sol)
	connect.AssertEqual(t, strings.Contains(solUri, "spl-token"), false)
	connect.AssertEqual(t, strings.Contains(solUri, "&reference="+cryptoSolReference(sol.IntentId)+"&"), true)
	connect.AssertEqual(t, cryptoSolReference(sol.IntentId), cryptoSolReference(sol.IntentId))
	// TAO: no link, the address alone for a QR code, and the note says why
	tao := cryptoTestCoinIntent(model.CryptoAssetTao, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY", 184808000)
	connect.AssertEqual(t, cryptoPaymentUri(tao), "")
	connect.AssertEqual(t, sdk.CryptoPaymentQrText(model.CryptoAssetTao, "", tao.Address), tao.Address)
	connect.AssertEqual(t, cryptoAssetUriNote(model.CryptoAssetTao) != "", true)
	connect.AssertEqual(t, cryptoAssetPaymentLink(model.CryptoAssetTao), false)
	for _, asset := range cryptoPayAssets {
		connect.AssertEqual(t, cryptoAssetNetworkWarning(asset) != "", true)
		// the sdk's market per coin is the server's network and market
		connect.AssertEqual(t, sdk.CryptoAssetMarket(asset), cryptoAssetNetwork(asset))
		connect.AssertEqual(t, sdk.CryptoAssetMarket(asset), cryptoAssetMarket(asset))
		connect.AssertEqual(t, sdk.CryptoAssetDecimals(asset), cryptoAssetDecimals(asset))
		connect.AssertEqual(t, sdk.CryptoAssetQuoteDecimals(asset), cryptoAssetQuoteDecimals(asset))
	}
}

// Kraken's pairs and Coinbase's codes for every coin they list, read from
// recorded answers: XBTUSD answers under Kraken's own name XXBTZUSD.
func TestCryptoRateSourcesForTheNewCoins(t *testing.T) {
	krakenAnswers := map[string][]byte{
		"XBTUSD": cryptoTestFixture(t, "kraken_xbtusd.json"),
		"TAOUSD": cryptoTestFixture(t, "kraken_taousd.json"),
	}
	krakenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer, ok := krakenAnswers[r.URL.Query().Get("pair")]
		if !ok {
			w.Write([]byte(`{"error":["EQuery:Unknown asset pair"]}`))
			return
		}
		w.Write(answer)
	}))
	defer krakenServer.Close()
	previousKraken := cryptoKrakenBaseUrl
	cryptoKrakenBaseUrl = krakenServer.URL
	t.Cleanup(func() { cryptoKrakenBaseUrl = previousKraken })

	rate, _, err := krakenFetchRate(context.Background(), model.CryptoAssetBtc)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, rate, 73596.90)
	rate, _, err = krakenFetchRate(context.Background(), model.CryptoAssetTao)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, rate, 218.6068)
	_, _, err = krakenFetchRate(context.Background(), model.CryptoAssetEth)
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, cryptoKrakenPairs[model.CryptoAssetEth], "ETHUSD")
	connect.AssertEqual(t, cryptoKrakenPairs[model.CryptoAssetSol], "SOLUSD")

	previousCoinbase := NewCoinbaseClient()
	asked := []string{}
	SetCoinbaseClient(&cryptoTestCoinbaseRates{asked: &asked})
	t.Cleanup(func() { SetCoinbaseClient(previousCoinbase) })
	for asset, code := range map[string]string{model.CryptoAssetTao: "TAO", model.CryptoAssetBtc: "BTC", model.CryptoAssetEth: "ETH", model.CryptoAssetSol: "SOL", model.CryptoAssetZec: "ZEC"} {
		rate, _, err := coinbaseFetchRate(context.Background(), asset)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rate, 100.5)
		connect.AssertEqual(t, asked[len(asked)-1], code)
	}
	_, _, err = coinbaseFetchRate(context.Background(), model.CryptoAssetXmr)
	connect.AssertEqual(t, err != nil, true)
	// CoinGecko answers Monero only
	_, _, err = coingeckoFetchRate(context.Background(), model.CryptoAssetZec)
	connect.AssertEqual(t, err != nil, true)
}

type cryptoTestCoinbaseRates struct {
	asked *[]string
}

func (self *cryptoTestCoinbaseRates) FetchExchangeRates(ctx context.Context, currencyTicker string) (*CoinbaseExchangeRatesResults, error) {
	*self.asked = append(*self.asked, currencyTicker)
	return &CoinbaseExchangeRatesResults{Currency: currencyTicker, Rates: map[string]string{"USD": "100.5"}}, nil
}

// The coin list judges rates from the cache alone: a coin nothing quoted
// lately is not judged, one whose sources were just asked and left fewer than
// two fresh answers that agree is unavailable, and it never asks a source.
func TestCryptoListedRatesAreJudgedFromTheCacheAlone(t *testing.T) {
	config := cryptoTestParseConfig(t, cryptoTestMainConfigWithCoinbaseCoins())
	fetches := 0
	previousFetchers := cryptoRateFetchers
	cryptoRateFetchers = map[string]cryptoRateFetch{
		cryptoRateSourceKraken: func(ctx context.Context, asset string) (float64, *time.Time, error) {
			fetches += 1
			return 0, nil, errors.New("synthetic: down")
		},
		cryptoRateSourceCoinbase: func(ctx context.Context, asset string) (float64, *time.Time, error) {
			fetches += 1
			return 100, nil, nil
		},
	}
	cryptoRates.reset()
	t.Cleanup(func() {
		cryptoRateFetchers = previousFetchers
		cryptoRates.reset()
	})
	now := cryptoRateNow()
	// cold: not judged, and no source asked
	available, _ := cryptoCachedRateAvailable(config, model.CryptoAssetBtc, now)
	connect.AssertEqual(t, available, true)
	connect.AssertEqual(t, fetches, 0)
	// a quote asks: one source down leaves one fresh answer, too few
	quote, reason := cryptoQuoteRate(context.Background(), config, model.CryptoAssetBtc)
	connect.AssertEqual(t, quote == nil, true)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedTooFew)
	available, reason = cryptoCachedRateAvailable(config, model.CryptoAssetBtc, cryptoRateNow())
	connect.AssertEqual(t, available, false)
	connect.AssertEqual(t, reason, cryptoQuoteRefusedRates)
	// two fresh answers that agree: available
	cryptoRateFetchers[cryptoRateSourceKraken] = func(ctx context.Context, asset string) (float64, *time.Time, error) {
		return 100.4, nil, nil
	}
	cryptoRates.reset()
	quote, _ = cryptoQuoteRate(context.Background(), config, model.CryptoAssetBtc)
	connect.AssertEqual(t, quote != nil, true)
	available, _ = cryptoCachedRateAvailable(config, model.CryptoAssetBtc, cryptoRateNow())
	connect.AssertEqual(t, available, true)
}

// A Coinbase coin switched on but not configured is listed as unavailable
// (not_configured), in the balance too; a coin whose pool is empty is
// unavailable (address_pool_empty). Neither is ever offered.
func TestCryptoCoinbaseCoinsUnavailableUntilConfigured(t *testing.T) {
	config := cryptoTestConfig()
	for _, asset := range cryptoPayCoinbaseAssets {
		config.Assets[asset] = &CryptoPayAssetConfig{
			Enabled: true,
			Plans:   []string{model.SolanaPlanYearly},
			Items:   []string{StripeItemData1Tib},
		}
	}
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{})
	// the vault: TAO and SOL complete; Bitcoin and ETH without alchemy.yml
	cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
		switch asset {
		case model.CryptoAssetTao:
			return &cryptoWalletSettings{Asset: asset, ChainRpcUrl: "ws://127.0.0.1:1", ChainProfile: cryptoNetworkMainnet, Coinbase: &cryptoCoinbaseSettings{Host: "api.coinbase.com", KeyName: "k", PrivateKey: "p"}}, nil
		case model.CryptoAssetSol:
			return &cryptoWalletSettings{Asset: asset, ChainRpcUrl: "https://synthetic.invalid/?api-key=x", Coinbase: &cryptoCoinbaseSettings{Host: "api.coinbase.com", KeyName: "k", PrivateKey: "p"}}, nil
		case model.CryptoAssetBtc, model.CryptoAssetEth:
			return nil, fmt.Errorf("alchemy.yml: alchemy.api_key is missing")
		}
		return &cryptoWalletSettings{Asset: asset, WalletRpcUrl: "http://127.0.0.1:1/", WalletRpcUser: "u", WalletRpcPass: "p", PrimaryAddress: "5x", WalletPassword: "w", NodeRpcUrl: "http://127.0.0.1:2/"}, nil
	}
	now := server.NowUtc()
	fresh := now.Add(-5 * time.Second)
	height := func(value int64) *int64 { return &value }
	cursors := map[string]*model.CryptoScanCursor{}
	for _, asset := range cryptoPayScannedAssets {
		cursors[asset] = &model.CryptoScanCursor{Asset: asset, LastSuccessAt: &fresh, WalletHeight: height(100), NetworkHeight: height(100)}
	}
	cryptoTestScanCursors(t, cursors)
	free := map[string]int{model.CryptoAssetTao: 0, model.CryptoAssetSol: 3}
	cryptoCountFreeAddresses = func(ctx context.Context, asset string) int {
		if count, ok := free[asset]; ok {
			return count
		}
		return 1
	}
	result, err := CryptoPaymentAssets(&session.ClientSession{Ctx: context.Background()})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, cryptoTestAssetNames(result.Assets), []string{model.CryptoAssetUsdcSolana, model.CryptoAssetZec, model.CryptoAssetXmr, model.CryptoAssetSol})
	reasons := map[string]string{}
	for _, listed := range result.UnavailableAssets {
		reasons[listed.Asset] = listed.UnavailableReason
		connect.AssertEqual(t, listed.Available, false)
	}
	connect.AssertEqual(t, reasons, map[string]string{
		model.CryptoAssetTao: cryptoQuoteRefusedPoolEmpty,
		model.CryptoAssetBtc: cryptoQuoteRefusedNotConfigured,
		model.CryptoAssetEth: cryptoQuoteRefusedNotConfigured,
	})
	// every coin of the list says its network, decimals and link
	for _, listed := range append(result.Assets, result.UnavailableAssets...) {
		connect.AssertEqual(t, listed.Network, cryptoAssetNetwork(listed.Asset))
		connect.AssertEqual(t, listed.Decimals, cryptoAssetDecimals(listed.Asset))
		connect.AssertEqual(t, listed.QuoteDecimals, cryptoAssetQuoteDecimals(listed.Asset))
		connect.AssertEqual(t, listed.PaymentLink, listed.Asset != model.CryptoAssetTao)
	}
	balance := &SubscriptionBalanceResult{}
	decorateCryptoPay(balance, &session.ClientSession{Ctx: context.Background()})
	connect.AssertEqual(t, cryptoTestAssetNames(balance.CryptoAssets), []string{model.CryptoAssetUsdcSolana, model.CryptoAssetZec, model.CryptoAssetXmr, model.CryptoAssetSol})
	connect.AssertEqual(t, cryptoTestAssetNames(balance.CryptoUnavailableAssets), []string{model.CryptoAssetTao, model.CryptoAssetBtc, model.CryptoAssetEth})
	// a refused coin is refused at the intent too
	cryptoTestHasAuthMethod(t, true)
	intent, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetBtc, ItemId: StripeItemData1Tib}, solanaTestPaymentSession(server.NewId()))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, intent.Error.Code, PurchaseErrorCodeAssetUnavailable)
}

// The canonical forms: Bitcoin mainnet only, bech32 in lowercase and bech32m
// checked; ETH in EIP-55 case; SOL a 32-byte key; TAO any SS58 prefix as 42.
func TestCryptoCoinbaseCoinAddressForms(t *testing.T) {
	good := map[string]string{
		// BIP 173 and BIP 350 vectors
		"BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4":                     "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
		"bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0": "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0",
		"1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2":                             "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2",
		"3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy":                             "3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy",
		"bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u":                     "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u",
	}
	for address, want := range good {
		canonical, err := cryptoBitcoinAddressCanonical(address)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, canonical, want)
	}
	for _, bad := range []string{
		// testnet, a bech32 checksum on a v1 program (BIP 350), mixed case, a typo
		"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"bc1pw508d6qejxtdg4y5r3zarvary0c5xw7kw508d6qejxtdg4y5r3zarvary0c5xw7k7grplx",
		"bc1qW508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
		"1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN3",
		"",
	} {
		if _, err := cryptoBitcoinAddressCanonical(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	eth, err := cryptoEthereumAddressCanonical("0xd8da6bf26964af9d7eed9e03e53415d37aa96045")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, eth, "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045")
	_, err = cryptoEthereumAddressCanonical("0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96046")
	connect.AssertEqual(t, err != nil, true)
	_, err = cryptoEthereumAddressCanonical("0xd8dA6bF26964aF9D7eEd9e03E53415D37aA96045")
	connect.AssertEqual(t, err != nil, true)
	_, err = cryptoSolanaAddressCanonical("4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM")
	connect.AssertEqual(t, err, nil)
	_, err = cryptoSolanaAddressCanonical("4Fj9RCwJqHLdLNK28DwWHunHqWapx")
	connect.AssertEqual(t, err != nil, true)
	// Alice's account, under the generic prefix 42 and under Polkadot's 0
	tao, err := cryptoTaoAddressCanonical("5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, tao, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
	tao, err = cryptoTaoAddressCanonical("15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, tao, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
	_, err = cryptoTaoAddressCanonical("5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQZ")
	connect.AssertEqual(t, err != nil, true)
}

// The status of an overpaid invoice says how much more arrived; the review a
// sanctions screen holds reads "screening" to the buyer.
func TestCryptoStatusShowsExcessAndScreening(t *testing.T) {
	config := cryptoTestParseConfig(t, cryptoTestMainConfigWithCoinbaseCoins())
	intent := cryptoTestCoinIntent(model.CryptoAssetBtc, "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u", 54622)
	intentId := intent.IntentId
	height := int64(975000)
	receipt := &model.CryptoPaymentReceipt{
		Asset: intent.Asset, TxId: "a", OutputKey: "7", IntentId: &intentId, AmountAtomic: 60000,
		BlockHeight: &height, Confirmations: 0, FirstSeenAt: intent.QuotedAt, State: model.CryptoReceiptStateConfirmed,
	}
	status := cryptoStatusResult(context.Background(), config, intent, []*model.CryptoPaymentReceipt{receipt}, "", intent.QuotedAt)
	connect.AssertEqual(t, status.Status, CryptoPaymentStatusConfirming)
	connect.AssertEqual(t, status.Excess, "0.00005378")
	connect.AssertEqual(t, status.ExcessAtomic, "5378")
	connect.AssertEqual(t, status.Network, "bitcoin")
	receipt.State = model.CryptoReceiptStateReview
	receipt.ReviewReason = model.CryptoReviewReasonSanctionsHold
	status = cryptoStatusResult(context.Background(), config, intent, []*model.CryptoPaymentReceipt{receipt}, "", intent.QuotedAt)
	connect.AssertEqual(t, status.Status, CryptoPaymentStatusReview)
	connect.AssertEqual(t, status.ReviewReason, CryptoReviewReasonScreening)
	receipt.ReviewReason = model.CryptoReviewReasonScreeningUnavailable
	status = cryptoStatusResult(context.Background(), config, intent, []*model.CryptoPaymentReceipt{receipt}, "", intent.QuotedAt)
	connect.AssertEqual(t, status.ReviewReason, CryptoReviewReasonScreening)
	receipt.ReviewReason = model.CryptoReviewReasonTransferShape
	status = cryptoStatusResult(context.Background(), config, intent, []*model.CryptoPaymentReceipt{receipt}, "", intent.QuotedAt)
	connect.AssertEqual(t, status.ReviewReason, model.CryptoReviewReasonTransferShape)
	statusJson, _ := json.Marshal(status)
	connect.AssertEqual(t, strings.Contains(string(statusJson), `"excess":"0.00005378","excess_atomic":"5378","network":"bitcoin"`), true)
}

// Until crypto_pay.yml names a Coinbase coin it is dormant, even with every
// vault file in place: no deposit address is asked of Coinbase, no chain is
// read, it is in neither list, and a quote or an invoice is refused.
func TestCryptoCoinbaseCoinsLeftOutAskNothing(t *testing.T) {
	fake, coinbaseSettings := cryptoTestCoinbase(t)
	for _, code := range []string{"TAO", "ETH", "SOL"} {
		fake.accounts[code] = "33333333-3333-3333-3333-333333333333"
	}
	// the production file, which names none of them
	restoreConfig := setCryptoPayConfigForTest(cryptoTestParseConfig(t, cryptoTestMainConfigYaml))
	t.Cleanup(restoreConfig)
	previousSettings := cryptoWalletSettingsFunc
	previousWalletFor := cryptoWalletFor
	walletsBuilt := 0
	cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
		// every vault file complete
		return &cryptoWalletSettings{Asset: asset, ChainRpcUrl: "http://127.0.0.1:1/v2/synthetic", ChainProfile: cryptoNetworkMainnet, Coinbase: coinbaseSettings}, nil
	}
	cryptoWalletFor = func(asset string, settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) (cryptoWallet, error) {
		walletsBuilt += 1
		return previousWalletFor(asset, settings, assetConfig)
	}
	t.Cleanup(func() {
		cryptoWalletSettingsFunc = previousSettings
		cryptoWalletFor = previousWalletFor
	})
	restoreSeams := cryptoTestHermeticSeams()
	t.Cleanup(restoreSeams)
	resetCryptoAvailability()
	t.Cleanup(resetCryptoAvailability)

	clientSession := &session.ClientSession{Ctx: context.Background()}
	for _, asset := range cryptoPayCoinbaseAssets {
		refill, err := RunCryptoAddressRefill(clientSession, asset)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refill.Configured, false)
		scan, err := RunCryptoPaymentScan(clientSession, asset)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Configured, false)
		connect.AssertEqual(t, cryptoAssetEnabled(cryptoPayConfigFunc(), asset), false)
	}
	connect.AssertEqual(t, len(fake.created), 0)
	connect.AssertEqual(t, fake.accountReads, 0)
	connect.AssertEqual(t, walletsBuilt, 0)
	available, unavailable := cryptoPaymentAssets(context.Background(), cryptoPayConfigFunc())
	for _, listed := range append(available, unavailable...) {
		connect.AssertEqual(t, slices.Contains(cryptoPayCoinbaseAssets, listed.Asset), false)
	}
}
