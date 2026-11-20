package controller

// Data packs and the rails that sell them (config/all/pro.yml data_code.skus).
//
// pro.yml is the one list of packs: each has an item id, an amount, a price
// and the rails that sell it (model.DataRail*). Every path that quotes a pack
// asks this file first, and a path the pack's rails do not name refuses it
// with item_unavailable and a message that says where it is sold. So a pack is
// sold where pro.yml says and refused everywhere else, never by omission:
//
//	card    StripeCreateCheckoutSession, PayDataCheckout
//	crypto  CreateCryptoPaymentIntent, CreateDataCryptoPaymentIntent,
//	        CryptoPaymentQuote, CryptoPaymentAssets, PayDataSolanaIntent
//	x402    X402Skus, X402Purchase
//	gamma   GammaTerms, CreateGammaPaymentIntent, SetGammaSettings, the top-ups
//
// The App Store and Google Play sell UR Pro only: they credit the store's
// subscription products, never a pack id.
//
// The 256 GiB pack (data_256gib, $0.75) is sold over x402 and with Gamma only.
// It is the pack for clients with a $1 per-payment cap, which every other
// price is over; by card its fees would be about 40% of the sale (the owner,
// 2026-11-11).
//
// Crediting a pack already sold reads its amount by id (dataPackByteCount),
// whatever the rails say now: what was paid for is delivered.

import (
	"fmt"
	"strings"

	"github.com/urnetwork/server/v2026/model"
)

// dataPackOnRail is the pro.yml data pack itemId, if the rail sells it. When
// it does not, refusal says where the pack is sold. Both are empty for an
// item id that names no pack, which each path answers in its own words.
func dataPackOnRail(itemId string, rail string) (sku *model.ProDataCodeSku, refusal string) {
	sku = model.Pro().DataCodeSku(itemId)
	if sku == nil {
		return nil, ""
	}
	if !sku.SoldOn(rail) {
		return nil, dataPackRailRefusal(sku)
	}
	return sku, ""
}

// dataPackRefusal is the refusal message for a pro.yml pack the rail does not
// sell, or "" when the rail sells it or the id names no pack.
func dataPackRefusal(itemId string, rail string) string {
	_, refusal := dataPackOnRail(itemId, rail)
	return refusal
}

// dataPackSoldOn reports whether the rail sells the pack.
func dataPackSoldOn(itemId string, rail string) bool {
	sku, _ := dataPackOnRail(itemId, rail)
	return sku != nil
}

// dataPackIdsOn are the item ids of the packs the rail sells, in pro.yml
// order.
func dataPackIdsOn(rail string) []string {
	ids := []string{}
	for _, sku := range model.Pro().DataCodeSkusOn(rail) {
		ids = append(ids, sku.Sku)
	}
	return ids
}

// dataPackPriceUsd is the pro.yml price of a pack on a rail that sells it:
// the same number on every rail, so a buyer is never shown two prices. ok =
// false for an unknown pack, a rail that does not sell it, or an unusable
// price.
func dataPackPriceUsd(itemId string, rail string) (float64, bool) {
	sku, _ := dataPackOnRail(itemId, rail)
	if sku == nil || sku.PriceUsd <= 0 || sku.Data <= 0 {
		return 0, false
	}
	return sku.PriceUsd, true
}

// dataPackByteCount is what a pack grants, for the credit of a pack that was
// sold. It does not ask the rails: a pack quoted while its rail sold it is
// delivered even if pro.yml has since moved it.
func dataPackByteCount(itemId string) (model.ByteCount, bool) {
	sku := model.Pro().DataCodeSku(itemId)
	if sku == nil || sku.Data <= 0 {
		return 0, false
	}
	return sku.Data, true
}

// isDataPackItem reports whether an item id names a data pack rather than a
// plan, by its form alone (model.DataCodeSkuPrefix): a path that credits never
// mistakes a pack, even one pro.yml no longer lists, for a plan.
func isDataPackItem(itemId string) bool {
	return strings.HasPrefix(itemId, model.DataCodeSkuPrefix)
}

// dataPackSize is a pack's amount as a person reads it: "256 GiB", "1 TiB".
func dataPackSize(byteCount model.ByteCount) string {
	return strings.TrimSuffix(model.SalesDataItem(byteCount), " data")
}

// dataPackRailRefusal says where a pack is sold, for a rail that does not
// sell it: "The 256 GiB data pack is sold only over x402 and with Gamma."
func dataPackRailRefusal(sku *model.ProDataCodeSku) string {
	where := []string{}
	for _, rail := range model.DataRails {
		if sku.SoldOn(rail) {
			where = append(where, dataRailPhrase(rail))
		}
	}
	size := dataPackSize(sku.Data)
	switch len(where) {
	case 0:
		return fmt.Sprintf("The %s data pack is not for sale.", size)
	case 1:
		return fmt.Sprintf("The %s data pack is sold only %s.", size, where[0])
	default:
		return fmt.Sprintf(
			"The %s data pack is sold only %s and %s.",
			size,
			strings.Join(where[:len(where)-1], ", "),
			where[len(where)-1],
		)
	}
}

// dataRailPhrase is how a refusal names a rail.
func dataRailPhrase(rail string) string {
	switch rail {
	case model.DataRailCard:
		return "by card"
	case model.DataRailCrypto:
		return "with crypto invoices"
	case model.DataRailX402:
		return "over x402"
	case model.DataRailGamma:
		return "with Gamma"
	}
	return rail
}
