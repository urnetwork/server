// Pure tests of `payouts close-parked`: the plan week, the amounts, the reasons,
// the totals, the digest and the report text. No database. Every id and amount
// is synthetic.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestPayoutCloseParkedWeek(t *testing.T) {
	for at, want := range map[time.Time]string{
		time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC):                 "2026-10-26",
		time.Date(2026, 10, 25, 23, 59, 59, 999999000, time.UTC):      "2026-10-19",
		time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC):                "2026-10-19",
		time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC):                "2026-10-26",
		time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC):                 "2026-10-26",
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC):                   "2026-12-28",
		time.Date(2026, 10, 26, 1, 0, 0, 0, time.FixedZone("", 7200)): "2026-10-19",
	} {
		if got := payoutCloseParkedWeek(at); got != want {
			t.Fatalf("%s is in the week of %s, want %s", at, got, want)
		}
	}
}

// Amounts print exactly: 1 USD is 1,000,000,000 nano cents, and the fee of 0.01
// USD is 10,000,000.
func TestFormatPayoutCloseParkedUsd(t *testing.T) {
	for nanoCents, want := range map[model.NanoCents]string{
		0:                    "0.000000000",
		1:                    "0.000000001",
		10_000_000:           "0.010000000",
		1_000_000_000:        "1.000000000",
		123_456_789_012:      "123.456789012",
		-10_000_001:          "-0.010000001",
		9223372036854775807:  "9223372036.854775807",
		-9223372036854775808: "-9223372036.854775808",
	} {
		if got := formatPayoutCloseParkedUsd(nanoCents); got != want {
			t.Fatalf("%d nano cents printed as %s, want %s", nanoCents, got, want)
		}
	}
	if model.UsdToNanoCents(providerPaymentFeeUsd) != 10_000_000 {
		t.Fatal("the fee is not 10,000,000 nano cents")
	}
}

// The reason is the first hold the sender would apply: no wallet, then a payout
// that does not cover the fee by the sender's arithmetic, then the review hold.
func TestUsdcParkedPaymentReason(t *testing.T) {
	reason := func(walletLess bool, payout model.NanoCents, review bool) string {
		payment := &model.UsdcParkedPayment{WalletLess: walletLess, Payout: payout, AttributionReviewRequired: review}
		return payment.Reason(providerPaymentFeeUsd)
	}
	for _, c := range []struct {
		walletLess bool
		payout     model.NanoCents
		review     bool
		want       string
	}{
		{false, 10_000_001, false, model.UsdcParkedOther},
		{false, 10_000_000, false, model.UsdcParkedDust},
		{false, 0, false, model.UsdcParkedDust},
		{false, 9_999_999, true, model.UsdcParkedDust},
		{false, 10_000_001, true, model.UsdcParkedReviewHeld},
		{true, 10_000_001, true, model.UsdcParkedWalletLess},
		{true, 1, false, model.UsdcParkedWalletLess},
		{false, 123_456_789_012, false, model.UsdcParkedOther},
	} {
		if got := reason(c.walletLess, c.payout, c.review); got != c.want {
			t.Fatalf("wallet-less %t, payout %d, review %t: %s, want %s", c.walletLess, c.payout, c.review, got, c.want)
		}
		// the sender's own test of a fresh attempt (advancePayment)
		covered := model.UsdToNanoCents(model.NanoCentsToUsd(c.payout)-providerPaymentFeeUsd) > 0
		if !c.walletLess && covered == (reason(false, c.payout, false) == model.UsdcParkedDust) {
			t.Fatalf("payout %d: dust disagrees with the sender's fee test", c.payout)
		}
	}
}

// Totals by reason, in the sender's order, and by plan week and reason, in week
// order; every total sums its rows exactly.
func TestPayoutCloseParkedSetTotals(t *testing.T) {
	sunday := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	monday := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	payment := func(created time.Time, payout model.NanoCents, walletLess bool, review bool, reimbursement bool) *model.UsdcParkedPayment {
		return &model.UsdcParkedPayment{PaymentId: server.NewId(), CreateTime: created, Payout: payout, WalletLess: walletLess,
			AttributionReviewRequired: review, ManualReimbursement: reimbursement}
	}
	payments := []*model.UsdcParkedPayment{
		payment(monday, 5_000_000_000, false, false, false),
		payment(monday, 1, false, true, false),
		payment(sunday, 20_000_000, false, true, false),
		payment(sunday, 3, true, false, false),
		payment(monday, 4_000_000_000, true, false, true),
		payment(sunday, 10_000_000, false, false, false),
	}
	set := newPayoutCloseParkedSet(payments)
	total := func(week string, reason string, count int, payout model.NanoCents) PayoutCloseParkedTotal {
		return PayoutCloseParkedTotal{Week: week, Reason: reason, Count: count, PayoutNanoCents: payout}
	}
	values := func(totals []*PayoutCloseParkedTotal) []PayoutCloseParkedTotal {
		out := []PayoutCloseParkedTotal{}
		for _, total := range totals {
			out = append(out, *total)
		}
		return out
	}
	if got, want := values(set.Reasons), []PayoutCloseParkedTotal{
		total("", model.UsdcParkedWalletLess, 2, 4_000_000_003),
		total("", model.UsdcParkedDust, 2, 10_000_001),
		total("", model.UsdcParkedReviewHeld, 1, 20_000_000),
		total("", model.UsdcParkedOther, 1, 5_000_000_000),
	}; !slices.Equal(got, want) {
		t.Fatalf("reasons %+v, want %+v", got, want)
	}
	if got, want := values(set.Weeks), []PayoutCloseParkedTotal{
		total("2026-10-19", model.UsdcParkedWalletLess, 1, 3),
		total("2026-10-19", model.UsdcParkedDust, 1, 10_000_000),
		total("2026-10-19", model.UsdcParkedReviewHeld, 1, 20_000_000),
		total("2026-10-26", model.UsdcParkedWalletLess, 1, 4_000_000_000),
		total("2026-10-26", model.UsdcParkedDust, 1, 1),
		total("2026-10-26", model.UsdcParkedOther, 1, 5_000_000_000),
	}; !slices.Equal(got, want) {
		t.Fatalf("weeks %+v, want %+v", got, want)
	}
	if set.Count != 6 || set.PayoutNanoCents != 9_030_000_004 || *set.ManualReimbursements != total("", "", 1, 4_000_000_000) {
		t.Fatal("totals", set.Count, set.PayoutNanoCents, set.ManualReimbursements)
	}
	for i := 1; i < len(set.Rows); i += 1 {
		if 0 <= set.Rows[i-1].PaymentId.Cmp(set.Rows[i].PaymentId) {
			t.Fatal("the rows are not in id order")
		}
	}
	if empty := newPayoutCloseParkedSet(nil); empty.Count != 0 || empty.Rows == nil || empty.Reasons == nil || empty.Weeks == nil || empty.ManualReimbursements == nil {
		t.Fatal("an empty set has missing totals", empty)
	}
}

// sha256 over the versioned header and each payment's id and payout, in id
// order, whatever the order of the rows.
func TestPayoutCloseParkedDigest(t *testing.T) {
	a := server.RequireParseId("01a17fd1-0000-7000-8000-00000000000a")
	b := server.RequireParseId("01a17fd1-0000-7000-8000-00000000000b")
	rows := []*PayoutCloseParkedRow{
		{PaymentId: b, PayoutNanoCents: 10_000_000, Reason: model.UsdcParkedDust, Week: "2026-10-19"},
		{PaymentId: a, PayoutNanoCents: 1_234_567_891, Reason: model.UsdcParkedOther, Week: "2026-10-26"},
	}
	sum := sha256.Sum256([]byte("urnetwork payouts close-parked v1\npayment 01a17fd1-0000-7000-8000-00000000000a 1234567891\npayment 01a17fd1-0000-7000-8000-00000000000b 10000000"))
	digest := payoutCloseParkedDigest(rows)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("digest", digest)
	}
	if payoutCloseParkedDigest([]*PayoutCloseParkedRow{rows[1], rows[0]}) != digest {
		t.Fatal("the digest depends on the row order")
	}
	// reasons and weeks are reported, not bound
	if payoutCloseParkedDigest([]*PayoutCloseParkedRow{{PaymentId: b, PayoutNanoCents: 10_000_000}, {PaymentId: a, PayoutNanoCents: 1_234_567_891}}) != digest {
		t.Fatal("the digest binds more than the ids and payouts")
	}
	for _, changed := range [][]*PayoutCloseParkedRow{
		{rows[0]},
		{rows[0], {PaymentId: a, PayoutNanoCents: 1_234_567_892}},
		{rows[0], rows[1], {PaymentId: server.NewId(), PayoutNanoCents: 1}},
		{},
	} {
		if payoutCloseParkedDigest(changed) == digest {
			t.Fatal("a different census kept the digest", changed)
		}
	}
	empty := sha256.Sum256([]byte("urnetwork payouts close-parked v1"))
	if payoutCloseParkedDigest(nil) != hex.EncodeToString(empty[:]) {
		t.Fatal("the digest of no payments")
	}
}

// The default report prints totals, never a payment id; with ids it lists each
// payment, and an apply adds what it closed and skipped.
func TestPayoutCloseParkedReportText(t *testing.T) {
	parkedId, skippedId := server.NewId(), server.NewId()
	week := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	parked := newPayoutCloseParkedSet([]*model.UsdcParkedPayment{
		{PaymentId: parkedId, CreateTime: week, Payout: 2_500_000_000, ManualReimbursement: true},
		{PaymentId: skippedId, CreateTime: week, Payout: 10_000_000},
	})
	report := &PayoutCloseParkedReport{
		RunId:        server.NewId(),
		Time:         time.Date(2026, 10, 27, 23, 0, 0, 0, time.UTC),
		Mode:         "apply",
		UsdcPayments: server.UsdcPayoutOff,
		FeeNanoCents: 10_000_000,
		Digest:       strings.Repeat("ab", 32),
		Parked:       parked,
		Closed:       newPayoutCloseParkedSet([]*model.UsdcParkedPayment{{PaymentId: parkedId, CreateTime: week, Payout: 2_500_000_000, ManualReimbursement: true}}),
		Skipped:      []*PayoutCloseParkedRow{parked.Rows[slices.IndexFunc(parked.Rows, func(row *PayoutCloseParkedRow) bool { return row.PaymentId == skippedId })]},
	}
	text := report.Text(false)
	for _, line := range []string{
		"payouts close-parked run " + report.RunId.String() + " at 2026-10-27T23:00:00Z, mode apply\n",
		"usdc payments: off (usdc_payout.yml); closing requires payments: off\n",
		"amounts: exact USD, 1 USD = 1000000000 nano cents; the sender's fee is 0.010000000 USD (10000000 nano cents)\n",
		"parked: 2 payment(s), 2.510000000 USD (2510000000 nano cents)\n",
		"    dust               1           0.010000000 USD\n",
		"    other              1           2.500000000 USD\n",
		"    2026-10-19 dust               1           0.010000000 USD\n",
		"  of which manual reimbursements: 1, 2.500000000 USD\n",
		"closed before and pending again (never closed twice): 0\n",
		"digest " + report.Digest + "\n",
		"closed: 1 payment(s), 2.500000000 USD (2500000000 nano cents)\n",
		"skipped, changed since the census: 1\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("the report lacks %q:\n%s", line, text)
		}
	}
	if strings.Contains(text, parkedId.String()) || strings.Contains(text, skippedId.String()) {
		t.Fatal("the default report prints a payment id")
	}
	withIds := report.Text(true)
	for _, line := range []string{
		"  parked " + parkedId.String() + " week 2026-10-19 other 2.500000000 USD manual_reimbursement\n",
		"  parked " + skippedId.String() + " week 2026-10-19 dust 0.010000000 USD\n",
		"  closed " + parkedId.String() + " week 2026-10-19 other 2.500000000 USD manual_reimbursement\n",
		"  skipped " + skippedId.String() + " week 2026-10-19 dust 0.010000000 USD\n",
	} {
		if !strings.Contains(withIds, line) {
			t.Fatalf("the report with ids lacks %q:\n%s", line, withIds)
		}
	}
	report.Mode, report.Closed, report.Skipped = "dry_run", nil, nil
	if dry := report.Text(false); strings.Contains(dry, "closed:") || strings.Contains(dry, "skipped") {
		t.Fatal("a dry run reports a close-out", dry)
	}
}
