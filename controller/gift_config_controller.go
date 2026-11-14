package controller

// GET /pay/gift/config: what the gift dialog sells, from the pro.yml `gift:`
// block (mmm/GIFTOFPRIVACY.md §10.2). The site reads its prices from here, so
// the page and the charge cannot drift apart, and shows no gift peek while
// `enabled` is false.

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// GiftTierResult is one price tier: the unit price of every gift in an order
// of at least min_quantity gifts.
type GiftTierResult struct {
	MinQuantity    int   `json:"min_quantity"`
	UnitPriceCents int64 `json:"unit_price_cents"`
}

// GiftConfigResult is GET /pay/gift/config.
type GiftConfigResult struct {
	// whether gifts are sold now; while false every purchase route refuses
	// with gift_sales_closed and the site shows no peek
	Enabled bool `json:"enabled"`
	// the season the cards are made for (winter-2026)
	Season string `json:"season"`
	// usd: one price worldwide, never a regional tier
	Currency    string            `json:"currency"`
	MaxQuantity int               `json:"max_quantity"`
	Tiers       []*GiftTierResult `json:"tiers"`
	// the Pro a code grants from its redemption, and how long after purchase
	// it can be redeemed, in days
	ProDays        int64 `json:"pro_days"`
	RedeemableDays int64 `json:"redeemable_days"`
	// the coins that can take a gift invoice now (POST /pay/gift/crypto-intent),
	// in the order every UI shows them; [] while gifts are not sold
	CryptoAssets []string `json:"crypto_assets"`
}

const giftCurrency = "usd"

// GiftConfig answers GET /pay/gift/config.
func GiftConfig(clientSession *session.ClientSession) (*GiftConfigResult, error) {
	if !giftReadLimiter.allow(clientSession) {
		return nil, errors.New("429 Too many requests from this address. Try again in a minute.")
	}
	giftConfig := model.Gift()
	result := &GiftConfigResult{
		Enabled:        giftConfig.Enabled,
		Season:         giftConfig.Season,
		Currency:       giftCurrency,
		MaxQuantity:    giftConfig.MaxQuantity,
		Tiers:          []*GiftTierResult{},
		ProDays:        int64(giftConfig.ProDuration / (24 * time.Hour)),
		RedeemableDays: int64(giftConfig.RedeemableFor / (24 * time.Hour)),
		CryptoAssets:   []string{},
	}
	for _, tier := range giftConfig.Tiers {
		result.Tiers = append(result.Tiers, &GiftTierResult{
			MinQuantity:    tier.MinQuantity,
			UnitPriceCents: tier.UnitPriceCents,
		})
	}
	if giftConfig.Enabled {
		result.CryptoAssets = giftCryptoAssets(clientSession.Ctx, giftConfig)
	}
	return result, nil
}

// giftCryptoAssets are the coins that can take a gift invoice now: the coins
// GET /pay/crypto/assets offers (switched on, configured, their scanner caught
// up) that sell balance codes, since a gift order's invoice is a code
// delivery. None while gift.crypto_max_invoice_usd is not set. Never fails
// the config: a coin whose state cannot be read is left out.
func giftCryptoAssets(ctx context.Context, giftConfig *model.GiftConfig) (assets []string) {
	assets = []string{}
	if giftConfig.CryptoMaxInvoiceUsd <= 0 {
		return assets
	}
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[gift]could not list the coins for gift invoices: %v\n", r)
			assets = []string{}
		}
	}()
	config := cryptoPayConfigFunc()
	available, _ := cryptoPaymentAssets(ctx, config)
	for _, asset := range available {
		if config.Asset(asset.Asset).CodeDeliveryEnabled() {
			assets = append(assets, asset.Asset)
		}
	}
	return assets
}
