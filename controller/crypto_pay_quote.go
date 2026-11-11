package controller

// POST /pay/crypto/quote: what a plan or a data pack costs in a coin now,
// without making an invoice (docs/CRYPTO-PAY-API.md). An agent or a script
// compares coins with it before it pays: no receive address is drawn and
// nothing is written. The invoice (POST /pay/crypto/intent or
// /pay/data/crypto-intent) quotes again when it is made, by the same rules,
// so the amount it gives is the one to pay.
//
// The bearer token is optional: with one, the price is the network's (its
// tier, and the welcome offer when the network may redeem it); without one, a
// plan is quoted at the caller's tier and the welcome offer is refused.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// CryptoPaymentQuoteArgs is POST /pay/crypto/quote: exactly one of plan and
// item_id.
type CryptoPaymentQuoteArgs struct {
	Asset  string `json:"asset"`
	Plan   string `json:"plan,omitempty"`
	ItemId string `json:"item_id,omitempty"`
	// the store's storefront country, when known (the price tier)
	StorefrontCountry string `json:"storefront_country,omitempty"`
}

// CryptoPaymentQuoteResult is a quote: the price and the coin amount, as an
// invoice made now would give them.
type CryptoPaymentQuoteResult struct {
	Asset        string  `json:"asset"`
	Plan         string  `json:"plan"`
	ItemId       string  `json:"item_id"`
	Tier         string  `json:"tier"`
	OfferApplied bool    `json:"offer_applied"`
	PriceUsd     float64 `json:"price_usd"`
	// the item's regular price (the offer's full yearly price)
	RegularPriceUsd float64 `json:"regular_price_usd"`
	// the coin's USD rate the amount is quoted at (1 for USDC)
	RateUsd float64 `json:"rate_usd"`
	// the amount an invoice made now asks: price × (1 + the coin's buffer) ÷
	// rate, rounded up to quote_decimals. USDC's invoice adds a unique
	// sub-cent suffix to the price
	Amount                string     `json:"amount"`
	AmountAtomic          string     `json:"amount_atomic"`
	Network               string     `json:"network"`
	Decimals              int        `json:"decimals"`
	QuoteDecimals         int        `json:"quote_decimals"`
	PaymentLink           bool       `json:"payment_link"`
	QuoteTtlSeconds       int        `json:"quote_ttl_seconds"`
	ConfirmationsRequired int        `json:"confirmations_required"`
	QuotedAt              *time.Time `json:"quoted_at"`
	// a refusal (the same codes as POST /pay/crypto/intent); left out on success
	Error *CryptoPaymentError `json:"error,omitempty"`
}

func cryptoQuoteError(code string, message string) *CryptoPaymentQuoteResult {
	return &CryptoPaymentQuoteResult{Error: &CryptoPaymentError{Code: code, Message: message}}
}

// CryptoPaymentQuote answers POST /pay/crypto/quote.
func CryptoPaymentQuote(args *CryptoPaymentQuoteArgs, clientSession *session.ClientSession) (*CryptoPaymentQuoteResult, error) {
	if !payDataLookupLimiter.allow(clientSession) {
		return nil, errors.New("429 Too many requests from this address. Try again in a minute.")
	}
	ctx := clientSession.Ctx
	config := cryptoPayConfigFunc()
	asset := strings.TrimSpace(args.Asset)
	if !slices.Contains(cryptoPayAssets, asset) {
		return cryptoQuoteError(PurchaseErrorCodeInvalidRequest, "Unknown asset."), nil
	}
	if !cryptoAssetEnabled(config, asset) {
		return cryptoQuoteError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}
	assetConfig := config.Asset(asset)
	plan := strings.TrimSpace(args.Plan)
	itemId := strings.TrimSpace(args.ItemId)
	if (plan == "") == (itemId == "") {
		return cryptoQuoteError(PurchaseErrorCodeInvalidRequest, "Name a plan or a data pack."), nil
	}
	if plan != "" && !assetConfig.SellsPlan(plan) {
		return cryptoQuoteError(PurchaseErrorCodePlanUnavailable, "That plan is not available with this coin."), nil
	}
	if itemId != "" && !assetConfig.SellsItem(itemId) {
		return cryptoQuoteError(PurchaseErrorCodeItemUnavailable, "That data pack is not available with this coin."), nil
	}

	result := &CryptoPaymentQuoteResult{
		Asset:                 asset,
		Plan:                  plan,
		ItemId:                itemId,
		Network:               cryptoAssetNetwork(asset),
		Decimals:              cryptoAssetDecimals(asset),
		QuoteDecimals:         cryptoAssetQuoteDecimals(asset),
		PaymentLink:           cryptoAssetPaymentLink(asset),
		QuoteTtlSeconds:       int(cryptoAssetQuoteTtl(config, asset) / time.Second),
		ConfirmationsRequired: cryptoConfirmationsRequired(config, asset),
	}
	if plan != "" {
		offerApplied := false
		if plan == model.SolanaPlanYearlyOnboarding {
			if clientSession.ByJwt == nil || EligibleOnboardingOffer(clientSession, clientSession.ByJwt.NetworkId) == nil {
				return cryptoQuoteError(PurchaseErrorCodeOfferUnavailable, "The welcome offer is not available."), nil
			}
			offerApplied = true
		}
		tier := ResolvePriceTier(clientSession, args.StorefrontCountry)
		priceUsd, ok := solanaPlanPriceUsdForTier(plan, tier.Tier)
		if !ok || priceUsd <= 0 {
			return cryptoQuoteError(PurchaseErrorCodePlanUnavailable, "That plan is not available."), nil
		}
		result.Tier = tier.Tier.Name
		result.OfferApplied = offerApplied
		result.PriceUsd = priceUsd
		result.RegularPriceUsd = priceUsd
		if offerApplied {
			result.RegularPriceUsd = tier.Tier.YearlyUsd
		}
	} else {
		priceUsd, ok := dataPackPriceUsd(itemId)
		if !ok {
			glog.Errorf("[cryptopay]no usable price in pro.yml for data pack %s\n", itemId)
			return cryptoQuoteError(PurchaseErrorCodeItemUnavailable, "That data pack is not available."), nil
		}
		result.PriceUsd = priceUsd
		result.RegularPriceUsd = priceUsd
	}
	if config.Quote.MaxInvoiceUsd < result.PriceUsd {
		return cryptoQuoteError(PurchaseErrorCodeItemUnavailable, "This purchase is not available with this coin."), nil
	}

	now := server.NowUtc()
	result.QuotedAt = &now
	if asset == model.CryptoAssetUsdcSolana {
		result.RateUsd = 1
		amountAtomic := model.UsdToNanoCents(result.PriceUsd) / 1000
		result.AmountAtomic = fmt.Sprintf("%d", amountAtomic)
		result.Amount = cryptoFormatAtomic(amountAtomic, cryptoAssetDecimals(asset))
		return result, nil
	}
	if available, _ := cryptoAssetAvailable(ctx, config, asset); !available {
		return cryptoQuoteError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}
	rateQuote, reason := cryptoQuoteRate(ctx, config, asset)
	if rateQuote == nil {
		cryptoPayRefusalReports.report("rate/"+asset, func() {
			glog.Warningf("[cryptopay]%s cannot be quoted now: %s\n", asset, reason)
		})
		return cryptoQuoteError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}
	amountAtomic, ok := cryptoAssetQuoteAmountAtomic(asset, result.PriceUsd, rateQuote.RateUsd, config.BufferPercentFor(asset))
	if !ok {
		return cryptoQuoteError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}
	result.RateUsd = rateQuote.RateUsd
	result.AmountAtomic = fmt.Sprintf("%d", amountAtomic)
	result.Amount = cryptoFormatAtomic(amountAtomic, cryptoAssetDecimals(asset))
	cryptoPayQuoteResults.WithLabelValues(asset, "previewed").Inc()
	return result, nil
}
