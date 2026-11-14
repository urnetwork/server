package controller

import (
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/model"
)

// The rails every purchase path asks before it sells a data pack
// (data_pack_rails.go), against packs set here rather than the environment's
// pro.yml.

// dataPackTestSkus are the packs as config/all/pro.yml sells them: 1 TiB and
// 10 TiB on every rail, and 256 GiB over x402 and with Gamma only.
func dataPackTestSkus() []model.ProDataCodeSku {
	every := []string{model.DataRailCard, model.DataRailCrypto, model.DataRailX402, model.DataRailGamma}
	return []model.ProDataCodeSku{
		{Sku: StripeItemData1Tib, Data: model.Tib, PriceUsd: 3, Rails: every},
		{Sku: StripeItemData10Tib, Data: 10 * model.Tib, PriceUsd: 20, Rails: every},
		{Sku: X402SkuData256Gib, Data: 256 * model.Gib, PriceUsd: 0.75, Rails: []string{model.DataRailX402, model.DataRailGamma}},
	}
}

func dataPackTestInstall(t testing.TB, skus ...model.ProDataCodeSku) {
	t.Cleanup(model.Testing_SetDataCodeSkus(skus...))
}

func TestDataPackOnRail(t *testing.T) {
	dataPackTestInstall(t, dataPackTestSkus()...)

	for _, rail := range []string{model.DataRailX402, model.DataRailGamma} {
		sku, refusal := dataPackOnRail(X402SkuData256Gib, rail)
		connect.AssertEqual(t, refusal, "")
		connect.AssertEqual(t, sku.Data, 256*model.Gib)
		priceUsd, ok := dataPackPriceUsd(X402SkuData256Gib, rail)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, priceUsd, 0.75)
	}
	for _, rail := range []string{model.DataRailCard, model.DataRailCrypto} {
		sku, refusal := dataPackOnRail(X402SkuData256Gib, rail)
		connect.AssertEqual(t, sku == nil, true)
		connect.AssertEqual(t, refusal, "The 256 GiB data pack is sold only over x402 and with Gamma.")
		_, ok := dataPackPriceUsd(X402SkuData256Gib, rail)
		connect.AssertEqual(t, ok, false)
		connect.AssertEqual(t, dataPackSoldOn(X402SkuData256Gib, rail), false)
	}
	for _, rail := range model.DataRails {
		connect.AssertEqual(t, dataPackSoldOn(StripeItemData1Tib, rail), true)
	}

	// an id that names no pack: no refusal of its own, each path answers it
	sku, refusal := dataPackOnRail("data_2tib", model.DataRailCard)
	connect.AssertEqual(t, sku == nil, true)
	connect.AssertEqual(t, refusal, "")

	connect.AssertEqual(t, dataPackIdsOn(model.DataRailCard), []string{StripeItemData1Tib, StripeItemData10Tib})
	connect.AssertEqual(t, dataPackIdsOn(model.DataRailCrypto), []string{StripeItemData1Tib, StripeItemData10Tib})
	connect.AssertEqual(t, dataPackIdsOn(model.DataRailX402), []string{StripeItemData1Tib, StripeItemData10Tib, X402SkuData256Gib})
	connect.AssertEqual(t, dataPackIdsOn(model.DataRailGamma), []string{StripeItemData1Tib, StripeItemData10Tib, X402SkuData256Gib})

	// a credit reads the amount whatever the rails
	byteCount, ok := dataPackByteCount(X402SkuData256Gib)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, byteCount, 256*model.Gib)
}

func TestDataPackRailRefusalNamesWhereThePackIsSold(t *testing.T) {
	for _, test := range []struct {
		rails []string
		want  string
	}{
		{[]string{}, "The 1 TiB data pack is not for sale."},
		{[]string{model.DataRailGamma}, "The 1 TiB data pack is sold only with Gamma."},
		{[]string{model.DataRailX402, model.DataRailGamma}, "The 1 TiB data pack is sold only over x402 and with Gamma."},
		{[]string{model.DataRailGamma, model.DataRailCard, model.DataRailCrypto}, "The 1 TiB data pack is sold only by card, with crypto invoices and with Gamma."},
	} {
		connect.AssertEqual(t, dataPackRailRefusal(&model.ProDataCodeSku{Sku: StripeItemData1Tib, Data: model.Tib, PriceUsd: 3, Rails: test.rails}), test.want)
	}
	connect.AssertEqual(t, dataPackSize(256*model.Gib), "256 GiB")
	connect.AssertEqual(t, dataPackSize(10*model.Tib), "10 TiB")
}

// A data pack's id says it is one, so a credit never reads it as a plan.
func TestIsDataPackItem(t *testing.T) {
	for _, item := range []string{StripeItemData1Tib, StripeItemData10Tib, X402SkuData256Gib, "data_2tib"} {
		connect.AssertEqual(t, isDataPackItem(item), true)
	}
	for _, item := range []string{model.SolanaPlanMonthly, model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding, StripeItemProMonthly, X402SkuProMonth, "supporter", ""} {
		connect.AssertEqual(t, isDataPackItem(item), false)
	}
}

// crypto_pay.yml cannot put a pack on a coin that pro.yml does not sell with
// crypto: it is dropped when the file is read, and refused when it is sold.
func TestCryptoPayConfigDropsPacksNotSoldWithCrypto(t *testing.T) {
	dataPackTestInstall(t, dataPackTestSkus()...)
	config := &CryptoPayConfig{Assets: map[string]*CryptoPayAssetConfig{
		model.CryptoAssetZec: {Enabled: true, Items: []string{StripeItemData1Tib, X402SkuData256Gib, "yearly", StripeItemData10Tib}},
	}}
	config.normalize()
	connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).Items, []string{StripeItemData1Tib, StripeItemData10Tib})
	connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).SellsItem(StripeItemData1Tib), true)
	connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).SellsItem(X402SkuData256Gib), false)
	// listed after the file was read: still not sold
	config.Asset(model.CryptoAssetZec).Items = append(config.Asset(model.CryptoAssetZec).Items, X402SkuData256Gib)
	connect.AssertEqual(t, config.Asset(model.CryptoAssetZec).SellsItem(X402SkuData256Gib), false)
	connect.AssertEqual(t, cryptoPayKnownItems(), []string{StripeItemData1Tib, StripeItemData10Tib})
}

// Gamma sells every pack pro.yml sells with Gamma, as a purchase and as the
// top-up item, and names them when it refuses another.
func TestGammaItemsArePacksSoldWithGamma(t *testing.T) {
	dataPackTestInstall(t, dataPackTestSkus()...)
	connect.AssertEqual(t, gammaKnownItems(), []string{StripeItemData1Tib, StripeItemData10Tib, X402SkuData256Gib})
	connect.AssertEqual(t, gammaTopupItems(), []string{StripeItemData1Tib, StripeItemData10Tib, X402SkuData256Gib})
	connect.AssertEqual(t, gammaTopupItemMessage(), "The top-up item is data_1tib, data_10tib or data_256gib.")
	// the receipt names it
	connect.AssertEqual(t, gammaItemDescription(X402SkuData256Gib), "URnetwork 256 GiB data")
	connect.AssertEqual(t, cryptoItemDescription(StripeItemData1Tib), "URnetwork 1 TiB data")
	connect.AssertEqual(t, cryptoItemDescription(StripeItemData10Tib), "URnetwork 10 TiB data")

	dataPackTestInstall(t, dataPackTestSkus()[0])
	connect.AssertEqual(t, gammaTopupItemMessage(), "The top-up item is data_1tib.")
	dataPackTestInstall(t)
	connect.AssertEqual(t, gammaTopupItemMessage(), "No data pack is sold with Gamma now.")
}

// Google Play sells UR Pro only: a Play product named like a data pack is not
// one the server credits, whatever play.yml lists.
func TestPlayCreditsNoDataPack(t *testing.T) {
	skus := playCreditableSkus(map[string]*Sku{
		"supporter":             {FeeFraction: 0.3, Supporter: true},
		"monthly_transfer_1tib": {FeeFraction: 0.3, BalanceByteCountHumanReadable: "1TiB"},
		X402SkuData256Gib:       {FeeFraction: 0.3, BalanceByteCountHumanReadable: "256GiB"},
		StripeItemData1Tib:      {FeeFraction: 0.3, BalanceByteCountHumanReadable: "1TiB"},
	})
	connect.AssertEqual(t, len(skus), 2)
	connect.AssertEqual(t, skus["supporter"] != nil, true)
	connect.AssertEqual(t, skus["monthly_transfer_1tib"] != nil, true)
	connect.AssertEqual(t, skus[X402SkuData256Gib] == nil, true)
	connect.AssertEqual(t, skus[StripeItemData1Tib] == nil, true)
}

// x402 sells exactly the packs pro.yml sells over x402, whatever x402.yml
// lists: a pack moved off x402 leaves the catalog even when x402.yml names it.
func TestX402DataSkusAreThePacksSoldOverX402(t *testing.T) {
	c := &X402Config{Enabled: true, Networks: []string{"base"}, Asset: "usdc", Skus: map[string]x402SkuConfig{
		X402SkuData1Tib:  {},
		X402SkuData10Tib: {},
	}}
	dataPackTestInstall(t, dataPackTestSkus()...)
	ids := func() []string {
		ids := []string{}
		for _, sku := range x402SkusForConfig(c) {
			ids = append(ids, sku.SkuId)
		}
		return ids
	}
	connect.AssertEqual(t, ids(), []string{StripeItemData1Tib, StripeItemData10Tib, X402SkuData256Gib})

	skus := dataPackTestSkus()
	skus[1].Rails = []string{model.DataRailCard}
	dataPackTestInstall(t, skus...)
	connect.AssertEqual(t, ids(), []string{StripeItemData1Tib, X402SkuData256Gib})
	connect.AssertEqual(t, x402SkuByIdForConfig(c, StripeItemData10Tib) == nil, true)
	_, err := x402TermsForConfig(c, x402PurchaseResource, StripeItemData10Tib, "")
	connect.AssertNotEqual(t, err, nil)
}
