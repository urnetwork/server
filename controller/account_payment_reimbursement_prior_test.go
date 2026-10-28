// Tests of `payouts reimburse --allow_prior_reimbursement` that need no
// database: its digest and its plan.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The prior reimbursements of the rows of payoutReimbursementTestRows: two of
// the first network, given out of order, and one of the second.
func payoutReimbursementPriorTestRows() []*PayoutReimbursementRow {
	rows := payoutReimbursementTestRows()
	rows[0].PriorReimbursements = []*PayoutReimbursementPrior{
		{PaymentId: server.RequireParseId("00000000-0000-8000-8000-0000000000c2")},
		{PaymentId: server.RequireParseId("00000000-0000-8000-8000-0000000000c1")},
	}
	rows[1].PriorReimbursements = []*PayoutReimbursementPrior{
		{PaymentId: server.RequireParseId("00000000-0000-8000-8000-0000000000d1")},
	}
	return rows
}

// With --allow_prior_reimbursement the digest is the sha256 of the v1 text, then
// the flag, then each row's prior reimbursements by network and payment id, in
// that order whatever order the rows and priors come in. It differs from the
// v1 digest even without prior reimbursements and changes with any change to
// them, while the v1 digest, which the ids derive from, does not.
func TestPayoutReimbursementAllowPriorDigest(t *testing.T) {
	reason := "synthetic per-window reliability subsidy"
	v1 := []string{
		"urnetwork payouts reimburse v1",
		"reason " + strconv.Quote(reason),
		"fee_nano_cents 10000000",
		`row 00000000-0000-8000-8000-00000000000a 00000000-0000-8000-8000-0000000000a1 "SOL" "SyntheticSolanaAddress1111111111111111111111" 12.345678 12355678000`,
		`row 00000000-0000-8000-8000-00000000000b 00000000-0000-8000-8000-0000000000b1 "MATIC" "0x00000000000000000000000000000000000000B1" 0.000123 10123000`,
	}
	sha := func(lines ...string) string {
		sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		return hex.EncodeToString(sum[:])
	}

	plain := payoutReimbursementDigest(reason, 10_000_000, payoutReimbursementTestRows())
	none := payoutReimbursementAllowPriorDigest(reason, 10_000_000, payoutReimbursementTestRows())
	if plain != sha(v1...) || none != sha(append(slices.Clone(v1), "allow_prior_reimbursement")...) || none == plain {
		t.Fatal("the flag alone is not bound", plain, none)
	}

	rows := payoutReimbursementPriorTestRows()
	digest := payoutReimbursementAllowPriorDigest(reason, 10_000_000, rows)
	canonical := append(slices.Clone(v1),
		"allow_prior_reimbursement",
		"prior 00000000-0000-8000-8000-00000000000a 00000000-0000-8000-8000-0000000000c1",
		"prior 00000000-0000-8000-8000-00000000000a 00000000-0000-8000-8000-0000000000c2",
		"prior 00000000-0000-8000-8000-00000000000b 00000000-0000-8000-8000-0000000000d1",
	)
	if digest != sha(canonical...) {
		t.Fatal("the digest is not the sha256 of the canonical rows, reason, flag and prior reimbursements", digest)
	}
	if payoutReimbursementDigest(reason, 10_000_000, rows) != plain {
		t.Fatal("prior reimbursements changed the digest the ids derive from")
	}
	reordered := []*PayoutReimbursementRow{rows[1], rows[0]}
	slices.Reverse(rows[0].PriorReimbursements)
	if payoutReimbursementAllowPriorDigest(reason, 10_000_000, reordered) != digest {
		t.Fatal("the digest depends on the order of the rows or their prior reimbursements")
	}

	changed := map[string]func(rows []*PayoutReimbursementRow){
		"a prior reimbursement dropped": func(rows []*PayoutReimbursementRow) {
			rows[0].PriorReimbursements = rows[0].PriorReimbursements[:1]
		},
		"a prior reimbursement added": func(rows []*PayoutReimbursementRow) {
			rows[1].PriorReimbursements = append(rows[1].PriorReimbursements, &PayoutReimbursementPrior{PaymentId: server.RequireParseId("00000000-0000-8000-8000-0000000000d2")})
		},
		"a prior reimbursement replaced": func(rows []*PayoutReimbursementRow) {
			rows[1].PriorReimbursements[0].PaymentId = server.RequireParseId("00000000-0000-8000-8000-0000000000d2")
		},
		"a prior reimbursement on the other network": func(rows []*PayoutReimbursementRow) {
			rows[1].PriorReimbursements = append(rows[1].PriorReimbursements, rows[0].PriorReimbursements[1])
			rows[0].PriorReimbursements = rows[0].PriorReimbursements[:1]
		},
		"no prior reimbursements": func(rows []*PayoutReimbursementRow) {
			rows[0].PriorReimbursements, rows[1].PriorReimbursements = nil, nil
		},
	}
	for name, change := range changed {
		rows := payoutReimbursementPriorTestRows()
		change(rows)
		if payoutReimbursementAllowPriorDigest(reason, 10_000_000, rows) == digest {
			t.Fatalf("%s keeps the digest", name)
		}
	}
}

// Each marker of the plan carries the digest the send matched and the flag, and
// lists its network's prior reimbursements in increasing order; without the
// flag a marker has neither.
func TestPayoutReimbursementPlanListsThePriorReimbursements(t *testing.T) {
	report := &PayoutReimbursementReport{
		PaymentPlanId:           server.NewId(),
		Digest:                  strings.Repeat("ef", 32),
		Reason:                  "synthetic per-window reliability subsidy",
		AllowPriorReimbursement: true,
		FeeNanoCents:            10_000_000,
		Rows:                    payoutReimbursementPriorTestRows(),
	}
	for _, row := range report.Rows {
		row.PaymentId = payoutReimbursementPaymentId(report.Digest, row.NetworkId)
		row.AmountNanoCents = row.PayoutNanoCents - report.FeeNanoCents
	}
	plan := payoutReimbursementPlan(report)
	wantPriors := [][]server.Id{
		{server.RequireParseId("00000000-0000-8000-8000-0000000000c1"), server.RequireParseId("00000000-0000-8000-8000-0000000000c2")},
		{server.RequireParseId("00000000-0000-8000-8000-0000000000d1")},
	}
	if plan.PaymentPlanId != report.PaymentPlanId || plan.Digest != report.Digest || plan.Reason != report.Reason || !plan.AllowPriorReimbursement || len(plan.Markers) != 2 {
		t.Fatalf("plan %+v", plan)
	}
	for i, marker := range plan.Markers {
		row := report.Rows[i]
		if marker.Version != 1 || marker.Digest != report.Digest || marker.PaymentPlanId != report.PaymentPlanId || marker.PaymentId != row.PaymentId ||
			marker.NetworkId != row.NetworkId || marker.WalletId != row.WalletId || marker.PayoutNanoCents != row.PayoutNanoCents ||
			!marker.AllowPriorReimbursement || !slices.Equal(marker.PriorPaymentIds, wantPriors[i]) {
			t.Fatalf("marker %d %+v", i, marker)
		}
	}

	report.AllowPriorReimbursement = false
	report.Rows = payoutReimbursementTestRows()
	for _, marker := range payoutReimbursementPlan(report).Markers {
		if marker.AllowPriorReimbursement || marker.PriorPaymentIds != nil {
			t.Fatalf("a marker without the flag %+v", marker)
		}
	}
}
