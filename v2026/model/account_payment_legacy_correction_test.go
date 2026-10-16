// The corrected amounts are pure: they depend only on the payment, its plan's
// subsidy epoch and the policy, so these cases need no database.
package model

import (
	"testing"
	"time"
)

func legacyCorrectionTestPolicy() *LegacyProviderPaymentCorrectionPolicy {
	return &LegacyProviderPaymentCorrectionPolicy{
		MinPayoutUsd:                   4000,
		UsdPerActiveUser:               0.1,
		SubscriptionNetRevenueFraction: 0.2,
		ReliabilityPeriod:              30 * 24 * time.Hour,
		TransferFee:                    UsdToNanoCents(0.01),
	}
}

// The defects: a revenue-driven plan subsidy above the configured floor, and a
// reliability subsidy per payout paid in full by a short plan.
func TestLegacyProviderPaymentCorrectionAmounts(t *testing.T) {
	policy := legacyCorrectionTestPolicy()
	start := time.Date(2026, 9, 8, 8, 46, 0, 0, time.UTC)
	window := func(days int, activeUserCount int64, netRevenueUsd float64) *LegacyProviderPaymentSubsidyWindow {
		return &LegacyProviderPaymentSubsidyWindow{
			StartTime:       start,
			EndTime:         start.Add(time.Duration(days) * 24 * time.Hour),
			ActiveUserCount: activeUserCount,
			NetRevenue:      UsdToNanoCents(netRevenueUsd),
		}
	}
	payment := func(subsidy NanoCents, reliability NanoCents, other NanoCents) *AccountPayment {
		return &AccountPayment{SubsidyPayout: subsidy, ReliabilitySubsidy: reliability, Payout: subsidy + reliability + other}
	}
	cases := []struct {
		name               string
		payment            *AccountPayment
		window             *LegacyProviderPaymentSubsidyWindow
		subsidyPayout      NanoCents
		reliabilitySubsidy NanoCents
		payout             NanoCents
		dust               bool
	}{
		// 0.2 x $50,000 = $10,000 set a 4-day epoch's subsidy instead of the $4,000 floor
		{name: "revenue-driven subsidy returns to the floor", payment: payment(1_000_000_000, 300_000_000, 0), window: window(4, 100, 50_000),
			subsidyPayout: 400_000_000, reliabilitySubsidy: 40_000_000, payout: 440_000_000},
		{name: "a floor subsidy is unchanged and reliability is pro rata", payment: payment(1_000_000_000, 300_000_000, 0), window: window(7, 100, 1_000),
			subsidyPayout: 1_000_000_000, reliabilitySubsidy: 70_000_000, payout: 1_070_000_000},
		// the formula without its revenue term keeps the active-user term
		{name: "the active-user term bounds the correction", payment: payment(1_200_000_000, 50_000_000, 0), window: window(30, 50_000, 30_000),
			subsidyPayout: 1_000_000_000, reliabilitySubsidy: 50_000_000, payout: 1_050_000_000},
		{name: "other components are carried", payment: payment(1_000_000_000, 300_000_000, 7), window: window(4, 100, 50_000),
			subsidyPayout: 400_000_000, reliabilitySubsidy: 40_000_000, payout: 440_000_007},
		{name: "a correction at or below the fee is dust", payment: payment(20_000_000, 0, 0), window: window(4, 100, 50_000),
			subsidyPayout: 8_000_000, reliabilitySubsidy: 0, payout: 8_000_000, dust: true},
		{name: "rounding is half away from zero", payment: payment(0, 3, 0), window: window(15, 100, 0),
			subsidyPayout: 0, reliabilitySubsidy: 2, payout: 2, dust: true},
	}
	for _, c := range cases {
		correction := CorrectLegacyProviderPayment(c.payment, c.window, policy)
		if correction.SubsidyPayout != c.subsidyPayout || correction.ReliabilitySubsidy != c.reliabilitySubsidy || correction.Payout != c.payout || correction.Dust != c.dust {
			t.Errorf("%s: corrected subsidy=%d reliability=%d payout=%d dust=%t, want %d %d %d %t", c.name,
				correction.SubsidyPayout, correction.ReliabilitySubsidy, correction.Payout, correction.Dust,
				c.subsidyPayout, c.reliabilitySubsidy, c.payout, c.dust)
		}
		if c.payment.SubsidyPayout < correction.SubsidyPayout || c.payment.ReliabilitySubsidy < correction.ReliabilitySubsidy {
			t.Errorf("%s: a correction increased a component", c.name)
		}
	}
}

// An epoch at or above the reliability period and a floor subsidy are already correct.
func TestLegacyProviderPaymentCorrectionUnchanged(t *testing.T) {
	start := time.Date(2026, 4, 5, 0, 7, 0, 0, time.UTC)
	window := &LegacyProviderPaymentSubsidyWindow{StartTime: start, EndTime: start.Add(30 * 24 * time.Hour), ActiveUserCount: 5_000, NetRevenue: UsdToNanoCents(4_000)}
	payment := &AccountPayment{SubsidyPayout: 912_000_000, ReliabilitySubsidy: 235_000_000, Payout: 1_147_000_000}
	correction := CorrectLegacyProviderPayment(payment, window, legacyCorrectionTestPolicy())
	if correction.Changed() || correction.Payout != payment.Payout || correction.Dust {
		t.Fatalf("an already correct payment changed: %+v", correction)
	}
}
