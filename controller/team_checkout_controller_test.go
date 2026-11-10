package controller

// Team checkout's pure parts (QUOTA-M6-02): the anchor, the items a subscription is sold with and
// the check of a Stripe price against pro.yml. The route runs against a fake Stripe in
// team_checkout_controller_db_test.go.

import (
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// pro.yml's Team prices: $100 for 5 seats, $20 a seat above, $3 a TiB of overage.
func testTeamPrices() model.Testing_TeamPrices {
	return model.Testing_TeamPrices{
		BaseUsd:        100,
		SeatUsd:        20,
		IncludedSeats:  5,
		TransferTibUsd: 3,
	}
}

// The anchor is the first 00:00:00 UTC after the session's expiry, an hour after it is made:
// across a day end, a month end and a leap day, and never the expiry itself.
func TestTeamCheckoutAnchorTime(t *testing.T) {
	at := func(year int, month time.Month, day int, hour int, minute int, second int) time.Time {
		return time.Date(year, month, day, hour, minute, second, 0, time.UTC)
	}
	sessionTimeout := DefaultTeamCheckoutSettings().SessionTimeout
	connect.AssertEqual(t, sessionTimeout, time.Hour)
	for _, c := range []struct {
		now    time.Time
		anchor time.Time
	}{
		// mid day: the next midnight
		{at(2026, 11, 10, 14, 30, 0), at(2026, 11, 11, 0, 0, 0)},
		// the expiry is the last second of the day
		{at(2026, 11, 10, 22, 59, 59), at(2026, 11, 11, 0, 0, 0)},
		// the expiry is midnight itself: the midnight after it
		{at(2026, 11, 10, 23, 0, 0), at(2026, 11, 12, 0, 0, 0)},
		// the expiry crosses the day end
		{at(2026, 11, 10, 23, 30, 0), at(2026, 11, 12, 0, 0, 0)},
		// a month end
		{at(2026, 11, 30, 10, 0, 0), at(2026, 12, 1, 0, 0, 0)},
		{at(2026, 11, 30, 23, 30, 0), at(2026, 12, 2, 0, 0, 0)},
		// a year end
		{at(2026, 12, 31, 23, 30, 0), at(2027, 1, 2, 0, 0, 0)},
		// a leap day
		{at(2028, 2, 28, 23, 30, 0), at(2028, 3, 1, 0, 0, 0)},
		{at(2028, 2, 28, 12, 0, 0), at(2028, 2, 29, 0, 0, 0)},
	} {
		expireTime := c.now.Add(sessionTimeout)
		anchor := teamCheckoutAnchorTime(expireTime)
		connect.AssertEqual(t, anchor, c.anchor)
		connect.AssertEqual(t, anchor.After(expireTime), true)
		// the first period is under a day and an hour
		connect.AssertEqual(t, anchor.Sub(c.now) <= 25*time.Hour, true)
	}
	// a session made in another zone anchors on UTC's day
	zone := time.FixedZone("east", 9*60*60)
	connect.AssertEqual(
		t,
		teamCheckoutAnchorTime(time.Date(2026, 11, 11, 3, 0, 0, 0, zone)),
		at(2026, 11, 11, 0, 0, 0),
	)
}

// A subscription is sold with the base and the seats, with transfer overage while pro.yml prices
// it and storage overage only once it is priced; no base or seat price, no price id for a priced
// item, and no prices at all sell nothing.
func TestStripeTeamCheckoutItems(t *testing.T) {
	defer model.Testing_SetTeamPrices(testTeamPrices())()
	config := &model.Pro().Team
	prices := StripeTeamPrices{Base: "price_base", Seats: "price_seats", TransferOverage: "price_transfer"}

	items, ok := stripeTeamCheckoutItems(prices, config)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, len(items), 3)
	connect.AssertEqual(t, items[0].item, stripeTeamItemBase)
	connect.AssertEqual(t, items[1].item, stripeTeamItemSeats)
	connect.AssertEqual(t, items[2].item, stripeTeamItemTransferOverage)

	// an env with no team_prices
	_, ok = stripeTeamCheckoutItems(StripeTeamPrices{}, config)
	connect.AssertEqual(t, ok, false)
	// a priced overage with no price id
	_, ok = stripeTeamCheckoutItems(StripeTeamPrices{Base: "price_base", Seats: "price_seats"}, config)
	connect.AssertEqual(t, ok, false)
	// a storage price id while pro.yml does not price storage: no item
	withStorageId := prices
	withStorageId.StorageOverage = "price_storage"
	items, ok = stripeTeamCheckoutItems(withStorageId, config)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, len(items), 3)

	// storage priced: its item, and its price id is required
	priced := testTeamPrices()
	priced.StorageGibMonthUsd = 0.02
	restore := model.Testing_SetTeamPrices(priced)
	items, ok = stripeTeamCheckoutItems(withStorageId, config)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, len(items), 4)
	connect.AssertEqual(t, items[3].item, stripeTeamItemStorageOverage)
	_, ok = stripeTeamCheckoutItems(prices, config)
	connect.AssertEqual(t, ok, false)
	restore()

	// transfer overage unpriced: withheld, so no item
	unpriced := testTeamPrices()
	unpriced.TransferTibUsd = 0
	restore = model.Testing_SetTeamPrices(unpriced)
	items, ok = stripeTeamCheckoutItems(prices, config)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, len(items), 2)
	restore()

	// no base price sells nothing
	noBase := testTeamPrices()
	noBase.BaseUsd = 0
	restore = model.Testing_SetTeamPrices(noBase)
	_, ok = stripeTeamCheckoutItems(prices, config)
	connect.AssertEqual(t, ok, false)
	restore()
}

func testTeamStripePrice(item stripeTeamItem) *stripe.Price {
	price := &stripe.Price{
		Active:        true,
		Currency:      stripe.CurrencyUSD,
		BillingScheme: stripe.PriceBillingSchemePerUnit,
		Recurring: &stripe.PriceRecurring{
			Interval:      stripe.PriceRecurringIntervalMonth,
			IntervalCount: 1,
			UsageType:     stripe.PriceRecurringUsageTypeLicensed,
		},
	}
	switch item {
	case stripeTeamItemBase:
		price.UnitAmount = 10000
		price.UnitAmountDecimal = 10000
	case stripeTeamItemSeats:
		price.BillingScheme = stripe.PriceBillingSchemeTiered
		price.TiersMode = stripe.PriceTiersModeGraduated
		price.Tiers = []*stripe.PriceTier{
			{UpTo: 5},
			{UnitAmount: 2000, UnitAmountDecimal: 2000},
		}
		price.Recurring.UsageType = stripe.PriceRecurringUsageTypeMetered
		price.Recurring.Meter = "mtr_synthetic_seats"
	case stripeTeamItemTransferOverage:
		// $3 a TiB is 300/1024 cents a GiB
		price.UnitAmountDecimal = 0.29296875
		price.Recurring.UsageType = stripe.PriceRecurringUsageTypeMetered
		price.Recurring.Meter = "mtr_synthetic_transfer"
	case stripeTeamItemStorageOverage:
		// $0.02 a GiB-month is 2 * 12 / 365 cents a GiB-day
		price.UnitAmountDecimal = 0.065753424658
		price.Recurring.UsageType = stripe.PriceRecurringUsageTypeMetered
		price.Recurring.Meter = "mtr_synthetic_storage"
	}
	return price
}

// Each item's Stripe price passes at pro.yml's amount and shape, and each way it can differ is
// named: the amount, a seat tier, the included seats, the interval, the currency, an archived
// price, a metered base and a licensed overage.
func TestStripeTeamPriceMismatch(t *testing.T) {
	prices := testTeamPrices()
	prices.StorageGibMonthUsd = 0.02
	defer model.Testing_SetTeamPrices(prices)()
	config := &model.Pro().Team

	items := []stripeTeamItem{stripeTeamItemBase, stripeTeamItemSeats, stripeTeamItemTransferOverage, stripeTeamItemStorageOverage}
	for _, item := range items {
		connect.AssertEqual(t, stripeTeamPriceMismatch(item, testTeamStripePrice(item), config), "")
	}
	differs := func(item stripeTeamItem, change func(price *stripe.Price)) {
		t.Helper()
		price := testTeamStripePrice(item)
		change(price)
		if stripeTeamPriceMismatch(item, price, config) == "" {
			t.Fatalf("a differing %s price passed", item)
		}
	}
	for _, item := range items {
		differs(item, func(price *stripe.Price) { price.Active = false })
		differs(item, func(price *stripe.Price) { price.Currency = stripe.CurrencyEUR })
		differs(item, func(price *stripe.Price) { price.Recurring.Interval = stripe.PriceRecurringIntervalYear })
		differs(item, func(price *stripe.Price) { price.Recurring.IntervalCount = 3 })
		differs(item, func(price *stripe.Price) { price.Recurring = nil })
		differs(item, func(price *stripe.Price) {
			price.TransformQuantity = &stripe.PriceTransformQuantity{DivideBy: 1024}
		})
	}
	if stripeTeamPriceMismatch(stripeTeamItemBase, nil, config) == "" {
		t.Fatal("no price passed")
	}
	// the base at another amount, metered, or tiered
	differs(stripeTeamItemBase, func(price *stripe.Price) { price.UnitAmount = 9900; price.UnitAmountDecimal = 9900 })
	differs(stripeTeamItemBase, func(price *stripe.Price) { price.Recurring.UsageType = stripe.PriceRecurringUsageTypeMetered })
	differs(stripeTeamItemBase, func(price *stripe.Price) { price.BillingScheme = stripe.PriceBillingSchemeTiered })
	// seats: another seat price, other included seats, a charge for the included seats, volume
	// tiers, a flat fee, a third tier, a licensed price
	differs(stripeTeamItemSeats, func(price *stripe.Price) { price.Tiers[1].UnitAmountDecimal = 1500 })
	differs(stripeTeamItemSeats, func(price *stripe.Price) { price.Tiers[0].UpTo = 10 })
	differs(stripeTeamItemSeats, func(price *stripe.Price) { price.Tiers[0].UnitAmountDecimal = 100 })
	differs(stripeTeamItemSeats, func(price *stripe.Price) { price.Tiers[0].FlatAmountDecimal = 100 })
	differs(stripeTeamItemSeats, func(price *stripe.Price) { price.TiersMode = stripe.PriceTiersModeVolume })
	differs(stripeTeamItemSeats, func(price *stripe.Price) { price.Tiers[1].FlatAmountDecimal = 500 })
	differs(stripeTeamItemSeats, func(price *stripe.Price) { price.Tiers[1].UpTo = 25 })
	differs(stripeTeamItemSeats, func(price *stripe.Price) {
		price.Tiers = append(price.Tiers, &stripe.PriceTier{UnitAmountDecimal: 1000})
	})
	differs(stripeTeamItemSeats, func(price *stripe.Price) { price.Recurring.UsageType = stripe.PriceRecurringUsageTypeLicensed })
	differs(stripeTeamItemSeats, func(price *stripe.Price) { price.Recurring.Meter = "" })
	// overage: another rate, or not metered
	differs(stripeTeamItemTransferOverage, func(price *stripe.Price) { price.UnitAmountDecimal = 0.3 })
	differs(stripeTeamItemTransferOverage, func(price *stripe.Price) { price.Recurring.UsageType = stripe.PriceRecurringUsageTypeLicensed })
	differs(stripeTeamItemStorageOverage, func(price *stripe.Price) { price.UnitAmountDecimal = 0.07 })
	differs(stripeTeamItemStorageOverage, func(price *stripe.Price) { price.Recurring.Meter = "" })

	// an overage pro.yml does not price is never matched by any Stripe price
	restore := model.Testing_SetTeamPrices(testTeamPrices())
	if stripeTeamPriceMismatch(stripeTeamItemStorageOverage, testTeamStripePrice(stripeTeamItemStorageOverage), config) == "" {
		t.Fatal("a storage price passed while pro.yml prices none")
	}
	restore()
}
