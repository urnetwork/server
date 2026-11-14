package model

// The gift of privacy's sales spec (mmm/GIFTOFPRIVACY.md §4, §10.1): the
// `gift:` block of pro.yml, beside the plan it gives. A gift code is a year of
// Pro from its redemption, sold at one price worldwide by quantity tier, up to
// max_quantity in an order.
//
// Like the rest of pro.yml it is read once, and a malformed value panics: a
// duration that does not parse, a tier list that is not in strictly rising
// order, a negative price or limit. An absent block, or an absent pro.yml, is
// INERT: Enabled is false and every value is zero, so every purchase path
// refuses (gift_sales_closed) and UnitPriceCents prices nothing. A block that
// is switched on but cannot sell (no duration, no redeem window, no limit, or a
// quantity from 1 to the limit without a price) is logged loudly and reads as
// switched off, rather than sell a code that grants nothing or expires on
// arrival.

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

type giftTierYaml struct {
	MinQuantity  int     `yaml:"min_quantity"`
	UnitPriceUsd float64 `yaml:"unit_price_usd"`
}

type giftConfigYaml struct {
	Enabled             bool           `yaml:"enabled"`
	Season              string         `yaml:"season"`
	ProDuration         string         `yaml:"pro_duration"`
	RedeemableFor       string         `yaml:"redeemable_for"`
	MaxQuantity         int            `yaml:"max_quantity"`
	Tiers               []giftTierYaml `yaml:"tiers"`
	CryptoMaxInvoiceUsd float64        `yaml:"crypto_max_invoice_usd"`
}

// GiftTier is one price of the gift: from MinQuantity gifts in an order, every
// gift in it costs UnitPriceCents (USD cents).
type GiftTier struct {
	MinQuantity    int
	UnitPriceCents int64
}

// GiftConfig is the parsed `gift:` block of pro.yml.
type GiftConfig struct {
	// the switch: while false every purchase route refuses, and the site shows
	// no peek. A config publish turns gift sales on.
	Enabled bool
	Season  string
	// the Pro a code grants, from its redemption (queued after a paid period)
	ProDuration time.Duration
	// how long after purchase a code can be redeemed
	RedeemableFor time.Duration
	MaxQuantity   int
	// in rising MinQuantity order
	Tiers []GiftTier
	// the cap of a gift order's crypto invoice; the data rail keeps its own
	CryptoMaxInvoiceUsd float64
}

// UnitPriceCents is the price of each gift in an order of quantity gifts: the
// tier with the largest MinQuantity at or below quantity. 0 when the quantity
// is below one, above MaxQuantity, or below every tier, which no purchase
// path sells.
func (c *GiftConfig) UnitPriceCents(quantity int) int64 {
	if c == nil || quantity < 1 || c.MaxQuantity < quantity {
		return 0
	}
	price := int64(0)
	for _, tier := range c.Tiers {
		if tier.MinQuantity <= quantity {
			price = tier.UnitPriceCents
		}
	}
	return price
}

// ProDays is the Pro a code grants, in whole days (365 for a year).
func (c *GiftConfig) ProDays() int64 {
	if c == nil {
		return 0
	}
	return giftDurationDays(c.ProDuration)
}

// RedeemableDays is how long a code can be redeemed after purchase, in whole
// days.
func (c *GiftConfig) RedeemableDays() int64 {
	if c == nil {
		return 0
	}
	return giftDurationDays(c.RedeemableFor)
}

func giftDurationDays(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64(math.Round(duration.Hours() / 24))
}

// Why an enabled block cannot sell: "" when it can.
func (c *GiftConfig) unsellableReason() string {
	switch {
	case c.ProDuration <= 0:
		return "no pro_duration"
	case c.RedeemableFor <= 0:
		return "no redeemable_for"
	case c.MaxQuantity < 1:
		return "no max_quantity"
	}
	for quantity := 1; quantity <= c.MaxQuantity; quantity++ {
		if c.UnitPriceCents(quantity) <= 0 {
			return fmt.Sprintf("no price for %d gifts", quantity)
		}
	}
	return ""
}

func parseGiftDuration(name string, value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		panic(fmt.Errorf("pro.yml: gift.%s: invalid duration %q: %w", name, value, err))
	}
	if duration < 0 {
		panic(fmt.Errorf("pro.yml: gift.%s: negative duration %q", name, value))
	}
	return duration
}

func giftUsdToCents(usd float64) int64 {
	return int64(math.Round(usd * 100))
}

// parseGiftConfig applies the rules above to the block as pro.yml wrote it.
func parseGiftConfig(y giftConfigYaml) *GiftConfig {
	c := &GiftConfig{
		Season:        strings.TrimSpace(y.Season),
		ProDuration:   parseGiftDuration("pro_duration", y.ProDuration),
		RedeemableFor: parseGiftDuration("redeemable_for", y.RedeemableFor),
		MaxQuantity:   y.MaxQuantity,
	}
	if c.MaxQuantity < 0 {
		panic(fmt.Errorf("pro.yml: gift.max_quantity is negative (%d)", c.MaxQuantity))
	}
	if math.IsNaN(y.CryptoMaxInvoiceUsd) || math.IsInf(y.CryptoMaxInvoiceUsd, 0) || y.CryptoMaxInvoiceUsd < 0 {
		panic(fmt.Errorf("pro.yml: gift.crypto_max_invoice_usd is invalid (%g)", y.CryptoMaxInvoiceUsd))
	}
	c.CryptoMaxInvoiceUsd = y.CryptoMaxInvoiceUsd
	for i, tierYaml := range y.Tiers {
		if tierYaml.MinQuantity < 1 {
			panic(fmt.Errorf("pro.yml: gift.tiers[%d].min_quantity must be at least 1 (%d)", i, tierYaml.MinQuantity))
		}
		if 0 < i && tierYaml.MinQuantity <= y.Tiers[i-1].MinQuantity {
			panic(fmt.Errorf("pro.yml: gift.tiers must rise by min_quantity (tiers[%d] is %d after %d)", i, tierYaml.MinQuantity, y.Tiers[i-1].MinQuantity))
		}
		if math.IsNaN(tierYaml.UnitPriceUsd) || math.IsInf(tierYaml.UnitPriceUsd, 0) || tierYaml.UnitPriceUsd < 0 {
			panic(fmt.Errorf("pro.yml: gift.tiers[%d].unit_price_usd is invalid (%g)", i, tierYaml.UnitPriceUsd))
		}
		c.Tiers = append(c.Tiers, GiftTier{
			MinQuantity:    tierYaml.MinQuantity,
			UnitPriceCents: giftUsdToCents(tierYaml.UnitPriceUsd),
		})
	}
	if y.Enabled {
		if reason := c.unsellableReason(); reason != "" {
			glog.Errorf("[gift]pro.yml gift.enabled is true, but the gift cannot be sold (%s); gift sales stay OFF\n", reason)
		} else {
			c.Enabled = true
		}
	}
	return c
}

var giftConfig = sync.OnceValue(func() *GiftConfig {
	// OPTIONAL, like pro.yml itself (pro.go logs its absence loudly): without it
	// the gift is simply not for sale
	resource, err := server.Config.SimpleResource("pro.yml")
	if err != nil {
		return &GiftConfig{}
	}
	var y struct {
		Gift *giftConfigYaml `yaml:"gift"`
	}
	resource.UnmarshalYaml(&y)
	if y.Gift == nil {
		glog.Infof("[gift]pro.yml has no gift: block; gift sales are off\n")
		return &GiftConfig{}
	}
	return parseGiftConfig(*y.Gift)
})

// a test's own config (Testing_SetGiftConfig), read before pro.yml's
var giftConfigOverride atomic.Pointer[GiftConfig]

// Gift returns the parsed `gift:` block of pro.yml (cached; parsed once).
func Gift() *GiftConfig {
	if override := giftConfigOverride.Load(); override != nil {
		return override
	}
	return giftConfig()
}

// Testing_SetGiftConfig makes Gift() return config until the returned function
// restores the previous one, for tests that sell or redeem gifts whatever this
// environment's pro.yml says. Not for production.
func Testing_SetGiftConfig(config *GiftConfig) (restore func()) {
	previous := giftConfigOverride.Swap(config)
	return func() {
		giftConfigOverride.Store(previous)
	}
}

// Testing_GiftConfig is a sellable config in the shape of mmm/GIFTOFPRIVACY.md
// §10.1, switched on, for tests.
func Testing_GiftConfig() *GiftConfig {
	return &GiftConfig{
		Enabled:       true,
		Season:        "winter-2026",
		ProDuration:   8760 * time.Hour,
		RedeemableFor: 8760 * time.Hour,
		MaxQuantity:   20,
		Tiers: []GiftTier{
			{MinQuantity: 1, UnitPriceCents: 3999},
			{MinQuantity: 2, UnitPriceCents: 2999},
			{MinQuantity: 5, UnitPriceCents: 2399},
		},
		CryptoMaxInvoiceUsd: 500,
	}
}
