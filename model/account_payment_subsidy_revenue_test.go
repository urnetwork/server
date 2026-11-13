package model

import (
	"testing"
	"time"
)

// The subsidy pool is max(`min_payout_usd`, `subscription_net_revenue_fraction`
// x revenue) per period, scaled to the window, as operator block points weigh
// it. The payout planner added the full revenue of every balance its sweeps
// drew from: one misrecorded balance (an App Store price from a non-USD
// storefront read as USD) set the pool of every plan that drew from it, and a
// balance spanning many windows added its whole revenue to each of them. The
// amounts and floors here are synthetic.

// a 30-day period, like `days: 30`
const subsidyRevenueTestPeriod = 30 * 24 * time.Hour

// a balance of `usd` revenue over [startTime, startTime+duration)
func subsidyRevenueTestBalance(startTime time.Time, duration time.Duration, usd float64) *subsidyRevenueBalance {
	return &subsidyRevenueBalance{
		startTime:         startTime,
		endTime:           startTime.Add(duration),
		subsidyNetRevenue: UsdToNanoCents(usd),
	}
}

// pro-rating in float may round by a nano cent
func assertSubsidyNetRevenueUsd(t testing.TB, label string, netRevenue NanoCents, expectedUsd float64) {
	t.Helper()
	if difference := netRevenue - UsdToNanoCents(expectedUsd); difference < -1 || 1 < difference {
		t.Fatalf("%s: subsidy net revenue $%.9f, expected $%.9f", label, NanoCentsToUsd(netRevenue), expectedUsd)
	}
}

func TestSubsidyNetRevenuePerPeriodProRatesToWindow(t *testing.T) {
	day := 24 * time.Hour
	week := 7 * day
	windowStartTime := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	windowEndTime := windowStartTime.Add(week)

	cases := []struct {
		label                  string
		subsidyRevenueBalances []*subsidyRevenueBalance
		expectedUsd            float64
	}{
		{
			// a period-long balance covering the window counts its full revenue, as before
			label:                  "monthly balance covering the window",
			subsidyRevenueBalances: []*subsidyRevenueBalance{subsidyRevenueTestBalance(windowStartTime.Add(-10*day), subsidyRevenueTestPeriod, 3.50)},
			expectedUsd:            3.50,
		},
		{
			// a month with a day of grace earns its revenue over 32 days
			label:                  "monthly grant with grace covering the window",
			subsidyRevenueBalances: []*subsidyRevenueBalance{subsidyRevenueTestBalance(windowStartTime.Add(-10*day), 32*day, 3.20)},
			expectedUsd:            3.00,
		},
		{
			// a year's purchase counts a period's share of its revenue, not all of it
			label:                  "yearly balance covering the window",
			subsidyRevenueBalances: []*subsidyRevenueBalance{subsidyRevenueTestBalance(windowStartTime.Add(-100*day), 365*day, 36.50)},
			expectedUsd:            3.00,
		},
		{
			label:                  "balance starting in the middle of the window",
			subsidyRevenueBalances: []*subsidyRevenueBalance{subsidyRevenueTestBalance(windowStartTime.Add(week/2), subsidyRevenueTestPeriod, 3.50)},
			expectedUsd:            1.75,
		},
		{
			// a renewal in the middle of the window takes over from the ending balance
			label: "renewal in the middle of the window",
			subsidyRevenueBalances: []*subsidyRevenueBalance{
				subsidyRevenueTestBalance(windowStartTime.Add(week/2-subsidyRevenueTestPeriod), subsidyRevenueTestPeriod, 3.50),
				subsidyRevenueTestBalance(windowStartTime.Add(week/2), subsidyRevenueTestPeriod, 3.50),
			},
			expectedUsd: 3.50,
		},
		{
			label:                  "balance ended before the window",
			subsidyRevenueBalances: []*subsidyRevenueBalance{subsidyRevenueTestBalance(windowStartTime.Add(-subsidyRevenueTestPeriod-day), subsidyRevenueTestPeriod, 3.50)},
			expectedUsd:            0,
		},
		{
			label:                  "balance starting after the window",
			subsidyRevenueBalances: []*subsidyRevenueBalance{subsidyRevenueTestBalance(windowEndTime, subsidyRevenueTestPeriod, 3.50)},
			expectedUsd:            0,
		},
		{
			label:                  "empty balance window",
			subsidyRevenueBalances: []*subsidyRevenueBalance{subsidyRevenueTestBalance(windowStartTime.Add(day), 0, 3.50)},
			expectedUsd:            0,
		},
	}
	for _, c := range cases {
		netRevenue := subsidyNetRevenuePerPeriod(c.subsidyRevenueBalances, windowStartTime, windowEndTime, subsidyRevenueTestPeriod)
		assertSubsidyNetRevenueUsd(t, c.label, netRevenue, c.expectedUsd)
	}
}

// Consecutive plan windows share a balance's revenue once: each window's revenue
// (its rate per period times its share of the period) adds up to the balance's
// revenue, whatever the plan cadence.
func TestSubsidyNetRevenuePerPeriodCountsSpanningBalanceOnce(t *testing.T) {
	day := 24 * time.Hour
	balanceStartTime := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	balanceDuration := 366 * day
	subsidyRevenueBalances := []*subsidyRevenueBalance{subsidyRevenueTestBalance(balanceStartTime, balanceDuration, 80.00)}

	for _, window := range []time.Duration{4 * day, 7 * day, 30 * day} {
		netWindowUsd := 0.0
		planCount := 0
		for windowStartTime := balanceStartTime.Add(-window / 2); windowStartTime.Before(balanceStartTime.Add(balanceDuration)); windowStartTime = windowStartTime.Add(window) {
			netRevenue := subsidyNetRevenuePerPeriod(subsidyRevenueBalances, windowStartTime, windowStartTime.Add(window), subsidyRevenueTestPeriod)
			netWindowUsd += NanoCentsToUsd(netRevenue) * float64(window) / float64(subsidyRevenueTestPeriod)
			planCount += 1
		}
		if netWindowUsd < 79.99 || 80.01 < netWindowUsd {
			t.Fatalf("%s windows: %d plans counted $%.6f of an $80.00 balance", window, planCount, netWindowUsd)
		}
	}
}

func TestSubsidyNetRevenuePerPeriodSkipsImplausibleBalance(t *testing.T) {
	day := 24 * time.Hour
	windowStartTime := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	windowEndTime := windowStartTime.Add(7 * day)
	monthlyBalance := subsidyRevenueTestBalance(windowStartTime.Add(-5*day), subsidyRevenueTestPeriod, 3.50)

	cases := []struct {
		label       string
		balance     *subsidyRevenueBalance
		expectedUsd float64
	}{
		{
			// a month's grant pro-rated from a misrecorded yearly price
			label:       "misrecorded monthly grant",
			balance:     subsidyRevenueTestBalance(windowStartTime.Add(-5*day), 32*day, 91000.00),
			expectedUsd: 3.50,
		},
		{
			// the misrecorded yearly purchase itself
			label:       "misrecorded yearly purchase",
			balance:     subsidyRevenueTestBalance(windowStartTime.Add(-180*day), 366*day, 970000.00),
			expectedUsd: 3.50,
		},
		{
			label:       "just above a plausible price",
			balance:     subsidyRevenueTestBalance(windowStartTime.Add(-5*day), subsidyRevenueTestPeriod, maxPlausibleBalanceSubsidyNetRevenueUsd+0.01),
			expectedUsd: 3.50,
		},
		{
			label:       "at a plausible price",
			balance:     subsidyRevenueTestBalance(windowStartTime.Add(-5*day), subsidyRevenueTestPeriod, maxPlausibleBalanceSubsidyNetRevenueUsd),
			expectedUsd: 3.50 + maxPlausibleBalanceSubsidyNetRevenueUsd,
		},
	}
	for _, c := range cases {
		netRevenue := subsidyNetRevenuePerPeriod(
			[]*subsidyRevenueBalance{monthlyBalance, c.balance},
			windowStartTime,
			windowEndTime,
			subsidyRevenueTestPeriod,
		)
		assertSubsidyNetRevenueUsd(t, c.label, netRevenue, c.expectedUsd)
	}
}
