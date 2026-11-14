// Expiration arithmetic must remain bounded and fair even for damaged inputs.
package model

import (
	"math"
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Valid original reports determine billing; a malformed peer neither blocks
// closure nor authorizes more usage, and adjudication retains its selected side.
func TestDeadlineReportAmountsReconcileOriginalEvidence(t *testing.T) {
	for _, sample := range []struct {
		name    string
		reports []contractSettlementReport
		outcome ContractOutcome
		used    ByteCount
	}{
		{name: "absent"},
		{name: "one checkpoint", reports: []contractSettlementReport{{party: ContractPartySource, byteCount: 17, checkpoint: true}}, used: 17},
		{name: "one final", reports: []contractSettlementReport{{party: ContractPartyDestination, byteCount: 19}}, used: 19},
		{name: "unequal originals", reports: []contractSettlementReport{{party: ContractPartySource, byteCount: 17}, {party: ContractPartyDestination, byteCount: 20}}, used: 18},
		{name: "bad peer", reports: []contractSettlementReport{{party: ContractPartySource, byteCount: -1}, {party: ContractPartyDestination, byteCount: 20}}, used: 20},
		{name: "unknown peer", reports: []contractSettlementReport{{party: ContractParty("invalid"), byteCount: 1000}, {party: ContractPartyDestination, byteCount: 20}}, used: 20},
		{name: "overflow-safe mean", reports: []contractSettlementReport{{party: ContractPartySource, byteCount: math.MaxInt64}, {party: ContractPartyDestination, byteCount: math.MaxInt64}}, used: math.MaxInt64},
		{name: "source adjudication", reports: []contractSettlementReport{{party: ContractPartySource, byteCount: 17}, {party: ContractPartyDestination, byteCount: 20}}, outcome: ContractOutcomeDisputeResolvedToSource, used: 17},
		{name: "missing adjudicated peer", reports: []contractSettlementReport{{party: ContractPartySource, byteCount: 17}}, outcome: ContractOutcomeDisputeResolvedToDestination},
	} {
		original := slices.Clone(sample.reports)
		adjustments := []string{}
		used, _ := deadlineReportAmounts(sample.reports, sample.outcome, &adjustments)
		if used != sample.used || !slices.Equal(original, sample.reports) {
			t.Fatal("reconciliation changed evidence or billed unsupported bytes", sample.name, used, sample.used)
		}
	}
}

// A shortfall spends only backed bytes. Invalid valuation or payer identity
// releases the reservation without inventing either a debit or a recipient.
func TestDeadlineEscrowPayoutsBoundEveryFundingFailure(t *testing.T) {
	payer, other, balanceId := server.NewId(), server.NewId(), server.NewId()
	for _, sample := range []struct {
		name                     string
		amount, available, start ByteCount
		revenue                  NanoCents
		network                  *server.Id
		participants             bool
		charged                  ByteCount
		paid                     NanoCents
	}{
		{name: "normal", amount: 100, available: 100, start: 100, revenue: 1000, network: &payer, participants: true, charged: 17, paid: 85},
		{name: "short balance", amount: 100, available: 7, start: 100, revenue: 1000, network: &payer, participants: true, charged: 7, paid: 35},
		{name: "short reservation", amount: 3, available: 100, start: 100, revenue: 1000, network: &payer, participants: true, charged: 3, paid: 15},
		{name: "negative balance", amount: 100, available: -7, start: 100, revenue: 1000, network: &payer, participants: true},
		{name: "missing grant", amount: 100},
		{name: "wrong payer", amount: 100, available: 100, start: 100, revenue: 1000, network: &other, participants: true},
		{name: "zero valuation denominator", amount: 100, available: 100, revenue: 1000, network: &payer, participants: true},
		{name: "negative reservation", amount: -1, available: 100, start: 100, revenue: 1000, network: &payer, participants: true},
		{name: "negative revenue", amount: 100, available: 100, start: 100, revenue: -1, network: &payer, participants: true},
		{name: "overflowing valuation", amount: 100, available: 100, start: 1, revenue: math.MaxInt64, network: &payer, participants: true},
		{name: "ambiguous participants", amount: 100, available: 100, start: 100, revenue: 1000, network: &payer},
	} {
		adjustments := []string{}
		payout := deadlineEscrowPayouts(17, []contractDeadlineEscrow{{
			contractSettlementEscrow: contractSettlementEscrow{balanceId: balanceId, amount: sample.amount, start: sample.start, revenue: sample.revenue},
			networkId:                sample.network, available: sample.available,
		}}, payer, sample.participants, &adjustments)[balanceId]
		if payout.payoutByteCount != sample.charged || payout.payout != sample.paid || payout.returnByteCount != max(0, sample.amount)-sample.charged || sample.charged < 17 && !slices.Contains(adjustments, "funding_shortfall") {
			t.Fatal("funding failure violated reconciliation", sample.name, payout, adjustments)
		}
	}
}

// A prior debit may sort after an untouched grant. Subtract accepted usage
// across all grants before choosing any new charge, independent of row order.
func TestDeadlineEscrowPayoutsKeepPriorConsumption(t *testing.T) {
	payer, firstId, secondId := server.NewId(), server.NewId(), server.NewId()
	prior := ByteCount(7)
	for _, journal := range []bool{false, true} {
		first := contractDeadlineEscrow{contractSettlementEscrow: contractSettlementEscrow{balanceId: firstId, amount: 100, start: 100}, networkId: &payer, available: 100}
		second := contractDeadlineEscrow{contractSettlementEscrow: contractSettlementEscrow{balanceId: secondId, amount: 100, start: 100}, networkId: &payer, available: 100, settled: !journal, priorPayout: &prior}
		if journal {
			second.journal = &prior
		}
		for _, escrows := range [][]contractDeadlineEscrow{{first, second}, {second, first}} {
			adjustments := []string{}
			payouts := deadlineEscrowPayouts(17, escrows, payer, true, &adjustments)
			if len(payouts) != 1 || payouts[firstId].payoutByteCount != 10 || !slices.Contains(adjustments, "retained_prior_consumption") || slices.Contains(adjustments, "funding_shortfall") {
				t.Fatal("earlier grant repeated accepted consumption", payouts, adjustments)
			}
		}
	}
}
