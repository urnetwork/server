package model

import (
	"strings"
	"testing"
)

// TestPaymentPlanSubsidyRangeUsesSelectedSweepSet is the deterministic
// regression for a production Payout query that spent minutes scanning the
// complete transfer_escrow_sweep history. planPayments has already materialized
// the exact unpaid/safely-canceled and optionally close-time-bounded set, so the
// subsidy range must reuse that relation without a second historical scan or
// independently-applied bound.
func TestPaymentPlanSubsidyRangeUsesSelectedSweepSet(t *testing.T) {
	query := strings.Join(strings.Fields(paymentPlanSubsidyRangeSQL), " ")

	for _, want := range []string{
		"FROM temp_account_payment",
		"transfer_contract.contract_id = temp_account_payment.contract_id",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("subsidy-range query does not contain %q: %s", want, query)
		}
	}
	for _, forbidden := range []string{
		"transfer_escrow_sweep",
		"WHERE transfer_contract.close_time",
		"$1",
	} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("subsidy-range query contains redundant historical source or bound %q: %s", forbidden, query)
		}
	}
}
