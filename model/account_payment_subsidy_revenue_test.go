package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The subsidy pool is max(`min_payout_usd`, `subscription_net_revenue_fraction`
// x revenue) per period, scaled to the plan window. The planner added the full
// revenue of every balance its sweeps drew from: one misrecorded balance (an App
// Store price from a non-USD storefront read as USD) set the pool of every plan
// that drew from it, and a balance spanning many windows added its whole revenue
// to each of them. The amounts and floors here are synthetic.

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

// a 30-day period with the floor `minPayoutUsd` and the 0.2 revenue fraction
func subsidyRevenueTestConfig(minPayoutUsd float64) *SubsidyConfig {
	subsidyConfig := *EnvSubsidyConfig()
	subsidyConfig.Days = 30
	subsidyConfig.MinDaysFraction = 0
	subsidyConfig.MinPayoutUsd = minPayoutUsd
	subsidyConfig.SubscriptionNetRevenueFraction = 0.2
	subsidyConfig.UsdPerActiveUser = 0
	subsidyConfig.MinWalletPayoutUsd = 0
	subsidyConfig.ReliabilitySubsidyPerPayoutUsd = 0
	return &subsidyConfig
}

// Plans a week in which each balance's payer sends traffic through one provider,
// and returns the plan's subsidy.
func planSubsidyRevenueTestWeek(t testing.TB, ctx context.Context, subsidyConfig *SubsidyConfig, transferBalances []*TransferBalance) *SubsidyPayment {
	t.Helper()
	usedTransferByteCount := ByteCount(1024 * 1024 * 1024)

	providerNetworkId := server.NewId()
	providerClientId := server.NewId()
	testingCreatePaymentClient(ctx, providerNetworkId, providerClientId)

	for _, transferBalance := range transferBalances {
		payerClientId := server.NewId()
		testingCreatePaymentClient(ctx, transferBalance.NetworkId, payerClientId)
		AddTransferBalance(ctx, transferBalance)

		escrow, err := CreateTransferEscrow(ctx, transferBalance.NetworkId, payerClientId, providerNetworkId, providerClientId, usedTransferByteCount)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, CloseContract(ctx, escrow.ContractId, payerClientId, usedTransferByteCount, false), nil)
		connect.AssertEqual(t, CloseContract(ctx, escrow.ContractId, providerClientId, usedTransferByteCount, false), nil)
	}

	// the subsidy window runs from the earliest contract create to the latest close
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE transfer_contract SET create_time = $1`,
			server.NowUtc().Add(-7*24*time.Hour),
		))
	})

	paymentPlan, err := PlanPaymentsWithConfig(ctx, subsidyConfig)
	connect.AssertEqual(t, err, nil)
	subsidyPayment := GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
	if subsidyPayment == nil {
		t.Fatal("the plan paid no subsidy")
	}
	return subsidyPayment
}

// A plan whose revenue term is below the floor pays the floor for its window.
func assertSubsidyPoolIsFloor(t testing.TB, subsidyConfig *SubsidyConfig, subsidyPayment *SubsidyPayment) {
	t.Helper()
	window := subsidyPayment.EndTime.Sub(subsidyPayment.StartTime)
	subsidyScale := min(float64(window/time.Minute)/float64(subsidyConfig.Duration()/time.Minute), 1.0)
	floorPayout := UsdToNanoCents(subsidyScale * subsidyConfig.MinPayoutUsd)
	// each payee's share rounds by up to a nano cent
	if difference := subsidyPayment.NetPayout - floorPayout; difference < -16 || 16 < difference {
		t.Fatalf("subsidy paid $%.6f over %s, expected the floor $%.6f", NanoCentsToUsd(subsidyPayment.NetPayout), window, NanoCentsToUsd(floorPayout))
	}
}

// A misrecorded month's grant without a purchase token and a misrecorded yearly
// purchase, like the September and October 2026 balances, beside a monthly
// subscription.
func TestPaymentPlanSubsidyRevenueSkipsMisrecordedBalances(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		subsidyConfig := subsidyRevenueTestConfig(300)
		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour

		subsidyPayment := planSubsidyRevenueTestWeek(t, ctx, subsidyConfig, []*TransferBalance{
			{
				NetworkId:             server.NewId(),
				StartTime:             now.Add(-20 * day),
				EndTime:               now.Add(-20 * day).Add(subsidyRevenueTestPeriod),
				StartBalanceByteCount: 1024 * 1024 * 1024 * 1024,
				BalanceByteCount:      1024 * 1024 * 1024 * 1024,
				SubsidyNetRevenue:     UsdToNanoCents(3.50),
			},
			{
				NetworkId:             server.NewId(),
				StartTime:             now.Add(-20 * day),
				EndTime:               now.Add(12 * day),
				StartBalanceByteCount: 1024 * 1024 * 1024 * 1024,
				BalanceByteCount:      1024 * 1024 * 1024 * 1024,
				SubsidyNetRevenue:     UsdToNanoCents(91000.00),
			},
			{
				NetworkId:             server.NewId(),
				StartTime:             now.Add(-180 * day),
				EndTime:               now.Add(186 * day),
				StartBalanceByteCount: 1024 * 1024 * 1024 * 1024,
				BalanceByteCount:      1024 * 1024 * 1024 * 1024,
				SubsidyNetRevenue:     UsdToNanoCents(970000.00),
			},
		})

		assertSubsidyNetRevenueUsd(t, "plan", subsidyPayment.NetRevenue, 3.50)
		assertSubsidyPoolIsFloor(t, subsidyConfig, subsidyPayment)
	})
}

// A yearly subscription counts $36.50 / 365 days = $3.00 per period in a weekly
// plan, not its whole year. With a $5 floor, the whole year (0.2 x $36.50 =
// $7.30) set the pool instead of the floor.
func TestPaymentPlanSubsidyRevenueProRatesBalanceSpanningWindows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		subsidyConfig := subsidyRevenueTestConfig(5)
		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour

		subsidyPayment := planSubsidyRevenueTestWeek(t, ctx, subsidyConfig, []*TransferBalance{
			{
				NetworkId:             server.NewId(),
				StartTime:             now.Add(-100 * day),
				EndTime:               now.Add(265 * day),
				StartBalanceByteCount: 1024 * 1024 * 1024 * 1024,
				BalanceByteCount:      1024 * 1024 * 1024 * 1024,
				SubsidyNetRevenue:     UsdToNanoCents(36.50),
			},
		})

		assertSubsidyNetRevenueUsd(t, "plan", subsidyPayment.NetRevenue, 3.00)
		assertSubsidyPoolIsFloor(t, subsidyConfig, subsidyPayment)
	})
}

// A week of monthly subscriptions counts the same revenue as before.
func TestPaymentPlanSubsidyRevenueNormalWeekUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		subsidyConfig := subsidyRevenueTestConfig(300)
		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour

		transferBalances := []*TransferBalance{}
		netUsd := 0.0
		for i, usd := range []float64{3.50, 4.79, 3.50} {
			startTime := now.Add(-time.Duration(10+i) * day)
			transferBalances = append(transferBalances, &TransferBalance{
				NetworkId:             server.NewId(),
				StartTime:             startTime,
				EndTime:               startTime.Add(subsidyRevenueTestPeriod),
				StartBalanceByteCount: 1024 * 1024 * 1024 * 1024,
				BalanceByteCount:      1024 * 1024 * 1024 * 1024,
				SubsidyNetRevenue:     UsdToNanoCents(usd),
			})
			netUsd += usd
		}
		subsidyPayment := planSubsidyRevenueTestWeek(t, ctx, subsidyConfig, transferBalances)

		assertSubsidyNetRevenueUsd(t, "plan", subsidyPayment.NetRevenue, netUsd)
		assertSubsidyPoolIsFloor(t, subsidyConfig, subsidyPayment)
	})
}
