package model

import (
	"strings"
	"testing"
)

// TestPaymentPlanSubsidyRangeUsesSelectedSweepSet is the deterministic
// regression for production Payout queries that spent minutes scanning the
// complete transfer_escrow_sweep history, and then up to 36 minutes joining the
// selected sweeps back to the 2B-row transfer_contract inside the plan
// transaction. planPayments has already materialized the exact
// unpaid/safely-canceled and optionally close-time-bounded set together with
// its contracts' times, so the subsidy range must read only that relation.
func TestPaymentPlanSubsidyRangeUsesSelectedSweepSet(t *testing.T) {
	query := strings.Join(strings.Fields(paymentPlanSubsidyRangeSQL), " ")

	for _, want := range []string{
		"FROM temp_account_payment",
		"MIN(temp_account_payment.contract_create_time)",
		"MAX(temp_account_payment.contract_close_time)",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("subsidy-range query does not contain %q: %s", want, query)
		}
	}
	for _, forbidden := range []string{
		"transfer_escrow_sweep",
		"transfer_contract",
		"JOIN",
		"$1",
	} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("subsidy-range query contains redundant historical source or bound %q: %s", forbidden, query)
		}
	}
}
