package controller

// Bitcoin's confirmation grace as the API states it (mmm/BTCPAY.md §16.8):
// the quote's expiry stays the end of the window to send in, and the grace
// after it is its own field, confirmation_grace_seconds, on the coin list,
// the quote, the invoice and its status (10800 for Bitcoin, left out for every
// other coin, so their answers are byte for byte what they were). Bitcoin's
// network warning says it, and an invoice in its grace is not re-quoted, since
// a payment sent within the quote may still be unmined. Hermetic: no
// database, no network.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The grace is Bitcoin's alone, 3 hours, after a quote window that is every
// coin's own as before (30 minutes, an hour for USDC).
func TestCryptoConfirmationGraceIsBitcoinsAlone(t *testing.T) {
	connect.AssertEqual(t, cryptoBitcoinConfirmationGrace, 3*time.Hour)
	connect.AssertEqual(t, cryptoBitcoinConfirmationGrace, cryptoTestBtcGrace)
	config := cryptoTestConfig()
	config.normalize()
	quotedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, asset := range cryptoPayAssets {
		policy := cryptoIntentPolicyFor(config, asset)
		intent := cryptoTestIntent(asset, 70, quotedAt)
		ttl := 30 * time.Minute
		if asset == model.CryptoAssetUsdcSolana {
			ttl = time.Hour
		}
		connect.AssertEqual(t, cryptoAssetQuoteTtl(config, asset), ttl)
		if asset == model.CryptoAssetBtc {
			connect.AssertEqual(t, cryptoAssetConfirmationGrace(asset), 3*time.Hour)
			connect.AssertEqual(t, CryptoAssetConfirmationGrace(asset), 3*time.Hour)
			connect.AssertEqual(t, policy.confirmationGrace, 3*time.Hour)
			connect.AssertEqual(t, policy.onTimeUntil(intent), intent.ExpiresAt.Add(3*time.Hour))
			continue
		}
		connect.AssertEqual(t, cryptoAssetConfirmationGrace(asset), time.Duration(0))
		connect.AssertEqual(t, policy.confirmationGrace, time.Duration(0))
		connect.AssertEqual(t, policy.onTimeUntil(intent), intent.ExpiresAt)
	}
	connect.AssertEqual(t, cryptoGraceHours(3*time.Hour), "3 hours")
	connect.AssertEqual(t, cryptoGraceHours(time.Hour), "1 hour")
	connect.AssertEqual(t, cryptoGraceHours(90*time.Minute), "90 minutes")

	// the block scan matches an invoice only while it is watched, so
	// Bitcoin's watch always outlasts the grace; the other coins' is theirs
	watch := func(asset string, watchAfter time.Duration) time.Duration {
		config := cryptoTestConfig()
		config.Assets[asset] = &CryptoPayAssetConfig{Enabled: true, WatchAfter: watchAfter}
		config.normalize()
		return config.WatchAfterFor(asset)
	}
	connect.AssertEqual(t, watch(model.CryptoAssetBtc, 0), 24*time.Hour)
	connect.AssertEqual(t, watch(model.CryptoAssetBtc, 2*time.Hour), 24*time.Hour)
	connect.AssertEqual(t, watch(model.CryptoAssetBtc, 3*time.Hour), 24*time.Hour)
	connect.AssertEqual(t, watch(model.CryptoAssetBtc, 4*time.Hour), 4*time.Hour)
	connect.AssertEqual(t, watch(model.CryptoAssetEth, 2*time.Hour), 2*time.Hour)
	connect.AssertEqual(t, watch(model.CryptoAssetTao, 0), 24*time.Hour)
}

// A Bitcoin invoice past its expiry and within its grace is not re-quoted: a
// payment sent within the quote may still be unmined, and a new amount would
// be judged against it. Before the expiry, and once the grace is over, it is
// re-quoted as before; every other coin is re-quoted whenever it was.
func TestCryptoBitcoinRequoteWaitsOutTheGrace(t *testing.T) {
	config := cryptoTestConfig()
	config.normalize()
	quotedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	intent := cryptoTestBtcIntent(t, quotedAt)
	policy := cryptoIntentPolicyFor(config, model.CryptoAssetBtc)
	graceEnd := intent.ExpiresAt.Add(3 * time.Hour)
	for at, held := range map[time.Time]bool{
		quotedAt.Add(10 * time.Minute):     false,
		intent.ExpiresAt:                   false,
		intent.ExpiresAt.Add(time.Second):  true,
		intent.ExpiresAt.Add(time.Hour):    true,
		graceEnd:                           true,
		graceEnd.Add(time.Second):          false,
		graceEnd.Add(24 * time.Hour):       false,
		intent.ExpiresAt.Add(-time.Second): false,
	} {
		connect.AssertEqual(t, cryptoRequoteInGrace(intent, policy, at), held)
	}
	message := cryptoRequoteInGraceMessage(intent, policy)
	connect.AssertEqual(t, strings.Contains(message, "can take up to 3 hours"), true)
	connect.AssertEqual(t, strings.Contains(message, "re-quoted from 2026-11-11T15:30:00Z"), true)
	connect.AssertEqual(t, strings.Contains(message, "start a new invoice"), true)

	for _, asset := range cryptoPayAssets {
		if asset == model.CryptoAssetBtc {
			continue
		}
		other := cryptoTestIntent(asset, 71, quotedAt)
		otherPolicy := cryptoIntentPolicyFor(config, asset)
		for _, at := range []time.Time{other.ExpiresAt.Add(time.Second), other.ExpiresAt.Add(time.Hour), other.ExpiresAt.Add(3 * time.Hour)} {
			connect.AssertEqual(t, cryptoRequoteInGrace(other, otherPolicy, at), false)
		}
	}
}

// The status keeps the honest expiry (the end of the window to send in) and
// carries the grace: a Bitcoin invoice with nothing received is pending, with
// no error, through the grace, and only then expired with quote_expired. Every
// other coin's status is what it was, without the field.
func TestCryptoBitcoinStatusStatesTheWindowAndTheGrace(t *testing.T) {
	config := cryptoTestConfig()
	config.normalize()
	ctx := context.Background()
	quotedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	intent := cryptoTestBtcIntent(t, quotedAt)
	graceEnd := intent.ExpiresAt.Add(3 * time.Hour)

	status := cryptoStatusResult(ctx, config, intent, nil, "", intent.ExpiresAt.Add(time.Minute))
	connect.AssertEqual(t, status.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, status.Error, (*CryptoPaymentError)(nil))
	connect.AssertEqual(t, *status.ExpiresAt, quotedAt.Add(30*time.Minute))
	connect.AssertEqual(t, status.ConfirmationGraceSeconds, 10800)
	statusJson, err := json.Marshal(status)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(string(statusJson), `"expires_at":"2026-11-11T12:30:00Z","confirmation_grace_seconds":10800,`), true)

	status = cryptoStatusResult(ctx, config, intent, nil, "", graceEnd)
	connect.AssertEqual(t, status.Status, CryptoPaymentStatusPending)
	status = cryptoStatusResult(ctx, config, intent, nil, "", graceEnd.Add(time.Second))
	connect.AssertEqual(t, status.Status, CryptoPaymentStatusExpired)
	connect.AssertEqual(t, status.Error.Code, PurchaseErrorCodeQuoteExpired)
	connect.AssertEqual(t, *status.ExpiresAt, quotedAt.Add(30*time.Minute))

	for _, asset := range cryptoPayAssets {
		if asset == model.CryptoAssetBtc {
			continue
		}
		other := cryptoTestIntent(asset, 72, quotedAt)
		otherStatus := cryptoStatusResult(ctx, config, other, nil, "", other.ExpiresAt.Add(time.Minute))
		connect.AssertEqual(t, otherStatus.Status, CryptoPaymentStatusExpired)
		connect.AssertEqual(t, otherStatus.Error.Code, PurchaseErrorCodeQuoteExpired)
		connect.AssertEqual(t, otherStatus.ConfirmationGraceSeconds, 0)
		otherJson, err := json.Marshal(otherStatus)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, strings.Contains(string(otherJson), "confirmation_grace"), false)
	}
}

// Bitcoin's network warning, which an agent or a person paying by hand reads,
// says to send before the quote expires and how long the confirmation may
// take; every other coin's is what it was.
func TestCryptoBitcoinNetworkWarningStatesTheGrace(t *testing.T) {
	connect.AssertEqual(
		t,
		cryptoAssetNetworkWarning(model.CryptoAssetBtc),
		"Send BTC on the Bitcoin network to this address, exactly the amount, before the quote expires. BTC on another network (Lightning, Base, Ethereum, Solana) is not credited. A payment needs 1 confirmation, which can take a while: one sent before the quote expires is credited at the quoted price if it confirms within 3 hours of the expiry.",
	)
	unchanged := map[string]string{
		model.CryptoAssetUsdcSolana: "Send USDC on Solana (the USDC token, mint " + solanaUsdcMint + ") to this address, exactly the amount. USDC on another network, or SOL, is not credited. A transfer sent by hand carries the memo.",
		model.CryptoAssetZec:        "Send ZEC to this shielded address. ZEC sent from a transparent address is credited; ZEC sent to a transparent address is not. A payment needs 10 confirmations.",
		model.CryptoAssetXmr:        "Send XMR to this subaddress, exactly the amount.",
		model.CryptoAssetTao:        "Send TAO on the Bittensor network (Finney) to this address, exactly the amount. TAO has no memo: the address alone identifies this invoice. TAO sent on another network or as a wrapped token is not credited.",
		model.CryptoAssetEth:        "Send ETH on Ethereum mainnet to this address, exactly the amount. ETH on another network (Base, Arbitrum, Optimism) or a token is not credited. A payment is credited once its block is finalized, about 15 minutes.",
		model.CryptoAssetSol:        "Send native SOL on Solana to this address, exactly the amount. A token (SPL) transfer is not credited. The address alone identifies this invoice: no memo is needed.",
	}
	for asset, warning := range unchanged {
		connect.AssertEqual(t, cryptoAssetNetworkWarning(asset), warning)
	}
}

// The coin list and the quote carry the grace beside the quote's window: 10800
// seconds for Bitcoin after its 1800, left out for every other coin, whose
// listing and quote are what they were.
func TestCryptoBitcoinListingAndQuoteStateTheGrace(t *testing.T) {
	config := cryptoTestConfig()
	config.Assets[model.CryptoAssetBtc] = &CryptoPayAssetConfig{
		Enabled: true,
		Plans:   []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding},
		Items:   []string{StripeItemData1Tib, StripeItemData10Tib},
	}
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{})
	cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
		return &cryptoWalletSettings{
			Asset: asset, ChainRpcUrl: "https://synthetic.invalid/v2/k", Coinbase: &cryptoCoinbaseSettings{Host: "h", KeyName: "k", PrivateKey: "p"},
			WalletRpcUrl: "http://127.0.0.1:1/", WalletRpcUser: "u", WalletRpcPass: "p", PrimaryAddress: "5x", WalletPassword: "w", NodeRpcUrl: "http://127.0.0.1:2/",
		}, nil
	}
	now := server.NowUtc()
	fresh := now.Add(-5 * time.Second)
	height := func(value int64) *int64 { return &value }
	cursors := map[string]*model.CryptoScanCursor{}
	for _, asset := range cryptoPayScannedAssets {
		cursors[asset] = &model.CryptoScanCursor{Asset: asset, LastSuccessAt: &fresh, WalletHeight: height(100), NetworkHeight: height(100)}
	}
	cryptoTestScanCursors(t, cursors)
	cryptoTestRates(t, map[string]map[string]float64{
		cryptoRateSourceKraken:   {model.CryptoAssetBtc: 73596.90},
		cryptoRateSourceCoinbase: {model.CryptoAssetBtc: 73596.90},
	})
	clientSession := &session.ClientSession{Ctx: context.Background()}

	result, err := CryptoPaymentAssets(clientSession)
	connect.AssertEqual(t, err, nil)
	listed := map[string]*CryptoPaymentAsset{}
	for _, asset := range append(result.Assets, result.UnavailableAssets...) {
		listed[asset.Asset] = asset
	}
	btc := listed[model.CryptoAssetBtc]
	if btc == nil {
		t.Fatalf("btc is not listed")
	}
	connect.AssertEqual(t, btc.QuoteTtlSeconds, 1800)
	connect.AssertEqual(t, btc.ConfirmationGraceSeconds, 10800)
	btcJson, err := json.Marshal(btc)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(string(btcJson), `"quote_ttl_seconds":1800,"confirmation_grace_seconds":10800,`), true)
	for asset, other := range listed {
		if asset == model.CryptoAssetBtc {
			continue
		}
		connect.AssertEqual(t, other.ConfirmationGraceSeconds, 0)
		otherJson, err := json.Marshal(other)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, strings.Contains(string(otherJson), "confirmation_grace"), false)
	}

	quote, err := CryptoPaymentQuote(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetBtc, ItemId: StripeItemData1Tib}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, quote.Error, (*CryptoPaymentError)(nil))
	connect.AssertEqual(t, quote.QuoteTtlSeconds, 1800)
	connect.AssertEqual(t, quote.ConfirmationGraceSeconds, 10800)
	quote, err = CryptoPaymentQuote(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, quote.Error, (*CryptoPaymentError)(nil))
	connect.AssertEqual(t, quote.QuoteTtlSeconds, 3600)
	quoteJson, err := json.Marshal(quote)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(string(quoteJson), "confirmation_grace"), false)
}
