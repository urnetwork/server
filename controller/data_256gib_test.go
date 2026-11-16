package controller

// The 256 GiB data pack: data_256gib, 256 GiB for $0.75 at the 1 TiB pack's
// $3/TiB. It is the pack for x402 clients with a $1 per-payment cap, which every
// other price is over (Cloudflare Agents' withX402Client cannot raise its cap),
// and it is sold over x402 and with Gamma only (the owner, 2026-11-11). By card
// its fees would be about 40% of the sale. config/all/pro.yml says so in the
// pack's rails, and these tests read the environment's pro.yml, the one the
// config release ships; gamma_256gib_db_test.go has the Gamma purchase and the
// x402 grant against the database.
//
// Before this change pro.yml had no ids or rails, and each path kept its own
// list of 1 TiB and 10 TiB: x402 offered no pack at $1 or less, and the card,
// crypto and store paths refused the pack only because it was not in their
// lists, as an unknown item or not at all.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// the pack's id, as agents send it: wire contract, pinned as a literal
const data256GibTestSkuId = "data_256gib"

// what every other path answers for it
const data256GibTestRefusal = "The 256 GiB data pack is sold only over x402 and with Gamma."

// x402 sells the pack whatever x402.yml lists: at $0.75, 750000 atomic USDC, for
// exactly 256 GiB, quoted on every network with no cap to raise, and a payment
// for those terms is settled and granted as 256 GiB of data, not Pro.
func TestX402SellsThe256GibPack(t *testing.T) {
	skipWithoutProYml(t)
	proSkus := slices.Clone(model.Pro().DataCodeSkus)
	setup := newX402V2TestSetup(t)
	// the setup prices 1 TiB alone; sell what this environment's pro.yml sells
	t.Cleanup(model.Testing_SetSkuPrices(5, 600*model.Gib, proSkus))

	// this x402.yml lists pro_1month and data_1tib only: the pack is pro.yml's
	sku := x402SkuByIdForConfig(setup.config, data256GibTestSkuId)
	if sku == nil {
		t.Fatalf("the x402 catalog has no %s: %v", data256GibTestSkuId, x402SkusForConfig(setup.config))
	}
	connect.AssertEqual(t, sku.PriceUsd, 0.75)
	connect.AssertEqual(t, sku.Amount, "750000")
	connect.AssertEqual(t, sku.ByteCount, 256*model.Gib)
	connect.AssertEqual(t, sku.Pro, false)

	// GET /x402/skus lists it with the amount a capped client checks
	w := httptest.NewRecorder()
	X402SkusHandler(w, httptest.NewRequest(http.MethodGet, "/x402/skus", nil))
	connect.AssertEqual(t, w.Code, http.StatusOK)
	catalog := struct {
		Skus []struct {
			SkuId     string          `json:"sku_id"`
			PriceUsd  float64         `json:"price_usd"`
			Amount    string          `json:"amount"`
			Pro       bool            `json:"pro"`
			ByteCount model.ByteCount `json:"byte_count"`
		} `json:"skus"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, listedSku := range catalog.Skus {
		if listedSku.SkuId == data256GibTestSkuId {
			listed = true
			connect.AssertEqual(t, listedSku.PriceUsd, 0.75)
			connect.AssertEqual(t, listedSku.Amount, "750000")
			connect.AssertEqual(t, listedSku.ByteCount, 256*model.Gib)
			connect.AssertEqual(t, listedSku.Pro, false)
		}
	}
	connect.AssertEqual(t, listed, true)

	// the terms: $0.75 on every network, in both versions, and nothing to raise
	terms, err := x402TermsForConfig(setup.config, x402PurchaseResource, data256GibTestSkuId, "payment required")
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, len(terms.V2.Accepts), 2)
	for _, accept := range terms.V2.Accepts {
		connect.AssertEqual(t, accept.Amount, "750000")
	}
	connect.AssertEqual(t, len(terms.V1.Accepts), 2)
	for _, accept := range terms.V1.Accepts {
		connect.AssertEqual(t, accept.MaxAmountRequired, "750000")
	}
	for _, text := range []string{terms.V1.Error, terms.V2.Error} {
		connect.AssertEqual(t, strings.Contains(text, "costs $0.75 in USDC"), true)
		connect.AssertEqual(t, strings.Contains(text, "per-payment cap"), false)
	}

	// a v2 payment for the Base term settles and grants the pack
	_, decoded := x402V2TestClientPayment(t)
	payment := x402V2TestCopy(t, decoded)
	acceptedJson, err := json.Marshal(terms.V2.Accepts[0])
	if err != nil {
		t.Fatal(err)
	}
	accepted := map[string]any{}
	if err := json.Unmarshal(acceptedJson, &accepted); err != nil {
		t.Fatal(err)
	}
	payment["accepted"] = accepted
	networkId := server.NewId()
	result, err := X402Purchase(context.Background(), networkId, x402V2TestPayment(t, payment), &X402PurchaseArgs{SkuId: data256GibTestSkuId}, x402PurchaseResource)
	if err != nil {
		t.Fatalf("the purchase failed: %v (facilitator saw %s)", err, x402CdpTestDescribe(setup.facilitator.received()))
	}
	connect.AssertEqual(t, result.Complete, true)
	connect.AssertEqual(t, result.SkuId, data256GibTestSkuId)
	connect.AssertEqual(t, result.Pro, false)
	connect.AssertEqual(t, result.ByteCount, 256*model.Gib)
	for _, path := range []string{"/verify", "/settle"} {
		requests := setup.facilitator.receivedOn(path)
		connect.AssertEqual(t, len(requests), 1)
		body := struct {
			PaymentRequirements struct {
				Amount string `json:"amount"`
			} `json:"paymentRequirements"`
		}{}
		if err := json.Unmarshal(requests[0].body, &body); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, body.PaymentRequirements.Amount, "750000")
	}
	granted := setup.grants.granted[result.Transaction]
	if granted == nil {
		t.Fatalf("the settled transaction was not granted: %v", setup.grants.granted)
	}
	connect.AssertEqual(t, granted.SkuId, data256GibTestSkuId)
	connect.AssertEqual(t, granted.ByteCount, 256*model.Gib)
	connect.AssertEqual(t, granted.Pro, false)
	recorded := setup.stripe.recorded()
	connect.AssertEqual(t, len(recorded), 1)
	connect.AssertEqual(t, recorded[0].form.Get("amount"), "75")
}

// By card the pack is refused, as item_unavailable with where it is sold, on
// the signed-in checkout and on /pay/data/checkout, before anything of Stripe's
// is touched: it never reaches a Stripe price.
func TestCardRefusesThe256GibPack(t *testing.T) {
	skipWithoutProYml(t)
	cryptoTestHasAuthMethod(t, true)
	cryptoTestResetLimiters()

	for _, uiMode := range []string{"", StripeUiModeHosted, StripeUiModeEmbedded} {
		result, err := StripeCreateCheckoutSession(&StripeCreateCheckoutSessionArgs{ItemId: data256GibTestSkuId, UiMode: uiMode}, solanaTestPaymentSession(server.NewId()))
		connect.AssertEqual(t, err, nil)
		if result.Error == nil {
			t.Fatalf("checkout (%q) was not refused: %+v", uiMode, result)
		}
		connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeItemUnavailable)
		connect.AssertEqual(t, result.Error.Message, data256GibTestRefusal)
		connect.AssertEqual(t, result.CheckoutUrl, "")
		connect.AssertEqual(t, result.ClientSecret, "")
		connect.AssertEqual(t, result.SessionId, "")
	}

	for _, networkName := range []string{"", "somenetwork"} {
		cryptoTestResetLimiters()
		result, err := PayDataCheckout(&PayDataCheckoutArgs{ItemId: data256GibTestSkuId, NetworkName: networkName}, session.Testing_CreateClientSession(context.Background(), nil))
		connect.AssertEqual(t, err, nil)
		if result.Error == nil {
			t.Fatalf("/pay/data/checkout (%q) was not refused: %+v", networkName, result)
		}
		connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeItemUnavailable)
		connect.AssertEqual(t, result.Error.Message, data256GibTestRefusal)
		connect.AssertEqual(t, result.Url, "")
	}
}

// With crypto the pack is refused whatever the coin, even one whose
// crypto_pay.yml lists it, as item_unavailable with where it is sold: the
// signed-in invoice, the invoice without sign-in, the quote and USDC by
// Solana Pay. No coin's catalog offers it.
func TestCryptoRefusesThe256GibPack(t *testing.T) {
	skipWithoutProYml(t)
	config := cryptoTestConfig()
	for _, assetConfig := range config.Assets {
		assetConfig.Items = append(assetConfig.Items, data256GibTestSkuId)
	}
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{
		model.CryptoAssetXmr: newFakeCryptoWallet(model.CryptoAssetXmr),
		model.CryptoAssetZec: newFakeCryptoWallet(model.CryptoAssetZec),
	})
	cryptoTestHasAuthMethod(t, true)
	clientSession := solanaTestPaymentSession(server.NewId())

	assets := []string{model.CryptoAssetUsdcSolana, model.CryptoAssetXmr, model.CryptoAssetZec}
	for _, asset := range assets {
		connect.AssertEqual(t, cryptoPayConfigFunc().Asset(asset).SellsItem(data256GibTestSkuId), false)

		cryptoTestResetLimiters()
		intent, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: asset, ItemId: data256GibTestSkuId}, clientSession)
		connect.AssertEqual(t, err, nil)
		if intent.Error == nil {
			t.Fatalf("a %s invoice was not refused", asset)
		}
		connect.AssertEqual(t, intent.Error.Code, PurchaseErrorCodeItemUnavailable)
		connect.AssertEqual(t, intent.Error.Message, data256GibTestRefusal)

		for _, networkName := range []string{"", "somenetwork"} {
			cryptoTestResetLimiters()
			dataIntent, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: asset, ItemId: data256GibTestSkuId, NetworkName: networkName}, clientSession)
			connect.AssertEqual(t, err, nil)
			if dataIntent.Error == nil {
				t.Fatalf("a %s invoice without sign-in (%q) was not refused", asset, networkName)
			}
			connect.AssertEqual(t, dataIntent.Error.Code, PurchaseErrorCodeItemUnavailable)
			connect.AssertEqual(t, dataIntent.Error.Message, data256GibTestRefusal)
		}

		cryptoTestResetLimiters()
		quote, err := CryptoPaymentQuote(&CryptoPaymentQuoteArgs{Asset: asset, ItemId: data256GibTestSkuId}, clientSession)
		connect.AssertEqual(t, err, nil)
		if quote.Error == nil {
			t.Fatalf("a %s quote was not refused", asset)
		}
		connect.AssertEqual(t, quote.Error.Code, PurchaseErrorCodeItemUnavailable)
		connect.AssertEqual(t, quote.Error.Message, data256GibTestRefusal)
	}

	// the catalog the apps and ur.io offer coins from
	lastSuccessAt := server.NowUtc()
	walletHeight := int64(1000)
	cryptoTestScanCursors(t, map[string]*model.CryptoScanCursor{
		model.CryptoAssetZec: {Asset: model.CryptoAssetZec, LastSuccessAt: &lastSuccessAt, WalletHeight: &walletHeight, NetworkHeight: &walletHeight},
		model.CryptoAssetXmr: {Asset: model.CryptoAssetXmr, LastSuccessAt: &lastSuccessAt, WalletHeight: &walletHeight, NetworkHeight: &walletHeight},
	})
	available, unavailable := cryptoPaymentAssets(context.Background(), cryptoPayConfigFunc())
	connect.AssertEqual(t, len(available), 3)
	for _, asset := range append(available, unavailable...) {
		connect.AssertEqual(t, asset.Items, []string{StripeItemData1Tib, StripeItemData10Tib})
	}

	// USDC by Solana Pay
	cryptoTestResetLimiters()
	solana, err := PayDataSolanaIntent(&PayDataSolanaIntentArgs{
		ItemId:      data256GibTestSkuId,
		NetworkName: "somenetwork",
		Reference:   "7Gk3sQx9pLmN2vB4cD6eF8hJ1kM5nP7rS9tU2wY4zA6B",
	}, session.Testing_CreateClientSession(context.Background(), nil))
	connect.AssertEqual(t, err, nil)
	if solana.Error == nil {
		t.Fatalf("a Solana Pay intent was not refused: %+v", solana)
	}
	connect.AssertEqual(t, solana.Error.Code, PurchaseErrorCodeItemUnavailable)
	connect.AssertEqual(t, solana.Error.Message, data256GibTestRefusal)
}

// The App Store sells UR Pro only: a product named like the pack is never
// credited, even where apple.yml would list it.
func TestAppStoreRefusesThe256GibPack(t *testing.T) {
	for _, allowed := range [][]string{
		{"supporter_monthly_26", "supporter_yearly_26"},
		{"supporter_monthly_26", "supporter_yearly_26", data256GibTestSkuId},
	} {
		connect.AssertEqual(t, appleProductAllowed(allowed, data256GibTestSkuId), false)
		connect.AssertEqual(t, appleProductAllowed(allowed, "supporter_yearly_26"), true)
	}
}

// Gamma's top-up choices take the pack: a setting that names it is refused
// only with the list of what Gamma sells, which names it too.
func TestGammaTopupChoicesNameThe256GibPack(t *testing.T) {
	skipWithoutProYml(t)
	clientSession := solanaTestPaymentSession(server.NewId())
	unknown := "data_7tib"
	result, err := SetGammaSettings(&GammaSettingsArgs{TopupItemId: &unknown}, clientSession)
	connect.AssertEqual(t, err, nil)
	if result.Error == nil {
		t.Fatal("an unknown top-up item was not refused")
	}
	connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeItemUnavailable)
	connect.AssertEqual(t, result.Error.Message, "The top-up item is data_1tib, data_10tib or data_256gib.")
}
