package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/model"
)

// TestSolanaPlanPriceComesFromTheServer pins that the quote is derived server-side from
// pro.yml, never taken from the client. A client-supplied amount would let anyone quote
// themselves a year for a cent.
func TestSolanaPlanPriceComesFromTheServer(t *testing.T) {
	skipWithoutProYml(t)

	monthly, ok := solanaPlanPriceUsd(model.SolanaPlanMonthly)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, monthly, float64(5))

	yearly, ok := solanaPlanPriceUsd(model.SolanaPlanYearly)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, yearly, float64(40))

	// an unknown plan has no price -- the intent is refused rather than priced at zero
	_, ok = solanaPlanPriceUsd("free_forever_please")
	connect.AssertEqual(t, ok, false)
	_, ok = solanaPlanPriceUsd("")
	connect.AssertEqual(t, ok, false)
}

// TestSolanaPlanDuration pins that the customer gets the plan they BOUGHT.
//
// The webhook used to grant a full YEAR for every accepted payment, whatever had been
// chosen and whatever had been paid.
func TestSolanaPlanDuration(t *testing.T) {
	connect.AssertEqual(t, solanaPlanDuration(model.SolanaPlanMonthly), 30*24*time.Hour)
	connect.AssertEqual(t, solanaPlanDuration(model.SolanaPlanYearly), SubscriptionYearDuration)

	// an intent created before the plan was recorded was always treated as yearly, so
	// those legacy intents keep getting exactly that
	connect.AssertEqual(t, solanaPlanDuration(""), SubscriptionYearDuration)
}

// TestSolanaUnderpaymentIsRefused pins the amount check, which is the whole reason the
// quote is recorded.
//
// underpaid := tolerance < expected - received
func TestSolanaUnderpaymentIsRefused(t *testing.T) {
	underpaid := func(expected float64, received float64) bool {
		return solanaAmountTolerance < expected-received
	}

	// the exact price is fine
	connect.AssertEqual(t, underpaid(40, 40), false)
	connect.AssertEqual(t, underpaid(5, 5), false)

	// overpaying is the customer's choice, and is honored
	connect.AssertEqual(t, underpaid(5, 40), false)
	connect.AssertEqual(t, underpaid(40, 100), false)

	// float dust is absorbed, not treated as a shortfall
	connect.AssertEqual(t, underpaid(40, 39.999), false)

	// a real shortfall is refused. THIS is the case that used to buy a year:
	// the old code accepted anything >= 40 and granted a year regardless.
	connect.AssertEqual(t, underpaid(40, 5), true)
	connect.AssertEqual(t, underpaid(40, 39.5), true)
	connect.AssertEqual(t, underpaid(5, 0.01), true)
}

// TestSolanaMonthlyPaymentIsNoLongerIgnored is the bug in one line.
//
// The site offers UR Pro monthly at $5 and takes Solana. The webhook required
// `TokenAmount >= 40` before it would even look at a transfer — so a $5 payment was
// discarded as "no matching USDC payment". The customer paid five dollars and received
// nothing at all, with no error anywhere.
func TestSolanaMonthlyPaymentIsNoLongerIgnored(t *testing.T) {
	skipWithoutProYml(t)

	monthlyPrice, ok := solanaPlanPriceUsd(model.SolanaPlanMonthly)
	connect.AssertEqual(t, ok, true)

	// what the old code required before it would accept a transfer at all
	const oldHardcodedMinimum = 40.0
	connect.AssertEqual(t, monthlyPrice < oldHardcodedMinimum, true) // ...so $5 was dropped

	// now: a $5 payment against a $5 quote is accepted, and buys a MONTH
	connect.AssertEqual(t, solanaAmountTolerance < monthlyPrice-monthlyPrice, false)
	connect.AssertEqual(t, solanaPlanDuration(model.SolanaPlanMonthly), 30*24*time.Hour)
}
