// `bringyourctl payouts close-parked` closes the parked USDC payments once
// usdc_payout.yml turns payments off (SN-RECONCILE.md, "Closing the parked
// payments"; owner-approved 2026-10-27). A parked payment is a pending payment
// that Circle cannot hold (model.GetUsdcParkedPayments). Closing one cancels it
// and journals a usdc_payout_closed event naming the switch, and forfeits its
// USDC obligation: no plan re-plans its sweeps (model/account_payment_usdc_close.go).
//
// The dry run only reads, in a read-only process. It reports the count and the
// payout of the parked payments by plan week and reason, without payment ids,
// and a digest over each payment's id and payout. `--apply --expect=<digest>`
// runs the same census again and refuses before any write unless USDC payments
// are declared off and its digest matches. It then closes the census in bounded
// batches, one transaction each, so no transaction holds many payments for long.
// A payment that changed since the census is skipped; a rerun closes nothing
// twice.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The switch every closure names.
const payoutCloseParkedSwitch = "usdc_payout.yml payments: off"

// Payments closed per transaction. Tests lower it to force several batches.
var payoutCloseParkedBatchSize = 500

// Observes each batch of the apply before it is written. Nil in production;
// tests use it to change a payment between the census and its batch.
var payoutCloseParkedBeforeBatch func(paymentIds []server.Id)

var ErrPayoutCloseParkedPaymentsNotOff = errors.New("payouts close-parked: usdc_payout.yml does not declare payments: off")

var ErrPayoutCloseParkedDigestMismatch = errors.New("payouts close-parked: --expect does not match the digest of this census")

// Closing requires USDC payments declared off. The reverse of
// RequireUsdcPaymentsOn, which treats an unreadable usdc_payout.yml as off: here
// only a readable file that declares payments: off allows the close-out, and an
// absent file means payments are on.
func requireUsdcPaymentsOff() error {
	policy, err := server.LoadUsdcPayoutPolicy()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPayoutCloseParkedPaymentsNotOff, err)
	}
	if policy == nil || policy.Payments != server.UsdcPayoutOff {
		return ErrPayoutCloseParkedPaymentsNotOff
	}
	return nil
}

// The UTC Monday that starts the ISO week of a payment's creation: the week of
// the plan that created it.
func payoutCloseParkedWeek(createTime time.Time) string {
	createTime = createTime.UTC()
	day := time.Date(createTime.Year(), createTime.Month(), createTime.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)).Format(time.DateOnly)
}

// An exact decimal USD amount: 1 USD is 1,000,000,000 nano cents.
func formatPayoutCloseParkedUsd(nanoCents model.NanoCents) string {
	sign := ""
	magnitude := uint64(nanoCents)
	if nanoCents < 0 {
		sign = "-"
		magnitude = -magnitude
	}
	return fmt.Sprintf("%s%d.%09d", sign, magnitude/1_000_000_000, magnitude%1_000_000_000)
}

// One parked payment of a census, or one payment the apply closed.
type PayoutCloseParkedRow struct {
	PaymentId           server.Id       `json:"payment_id"`
	Week                string          `json:"week"`
	Reason              string          `json:"reason"`
	PayoutNanoCents     model.NanoCents `json:"payout_nano_cents"`
	ManualReimbursement bool            `json:"manual_reimbursement,omitempty"`
}

// The count and payout of the payments of one reason, in one plan week when
// `Week` is set.
type PayoutCloseParkedTotal struct {
	Week            string          `json:"week,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Count           int             `json:"count"`
	PayoutNanoCents model.NanoCents `json:"payout_nano_cents"`
}

// A set of payments with its totals: all of them, by reason, by plan week and
// reason, and its manual reimbursements.
type PayoutCloseParkedSet struct {
	Count                int                       `json:"count"`
	PayoutNanoCents      model.NanoCents           `json:"payout_nano_cents"`
	Reasons              []*PayoutCloseParkedTotal `json:"reasons"`
	Weeks                []*PayoutCloseParkedTotal `json:"weeks"`
	ManualReimbursements *PayoutCloseParkedTotal   `json:"manual_reimbursements"`
	Rows                 []*PayoutCloseParkedRow   `json:"rows"`
}

// The reasons in the order the sender would hold a payment on them.
var payoutCloseParkedReasons = []string{model.UsdcParkedWalletLess, model.UsdcParkedDust, model.UsdcParkedReviewHeld, model.UsdcParkedOther}

func payoutCloseParkedRow(payment *model.UsdcParkedPayment) *PayoutCloseParkedRow {
	return &PayoutCloseParkedRow{
		PaymentId:           payment.PaymentId,
		Week:                payoutCloseParkedWeek(payment.CreateTime),
		Reason:              payment.Reason(providerPaymentFeeUsd),
		PayoutNanoCents:     payment.Payout,
		ManualReimbursement: payment.ManualReimbursement,
	}
}

func newPayoutCloseParkedSet(payments []*model.UsdcParkedPayment) *PayoutCloseParkedSet {
	set := &PayoutCloseParkedSet{
		Reasons:              []*PayoutCloseParkedTotal{},
		Weeks:                []*PayoutCloseParkedTotal{},
		ManualReimbursements: &PayoutCloseParkedTotal{},
		Rows:                 []*PayoutCloseParkedRow{},
	}
	for _, payment := range payments {
		set.Rows = append(set.Rows, payoutCloseParkedRow(payment))
	}
	slices.SortFunc(set.Rows, func(a *PayoutCloseParkedRow, b *PayoutCloseParkedRow) int {
		return a.PaymentId.Cmp(b.PaymentId)
	})
	reasons := map[string]*PayoutCloseParkedTotal{}
	weeks := map[[2]string]*PayoutCloseParkedTotal{}
	for _, row := range set.Rows {
		set.Count += 1
		set.PayoutNanoCents += row.PayoutNanoCents
		reason, ok := reasons[row.Reason]
		if !ok {
			reason = &PayoutCloseParkedTotal{Reason: row.Reason}
			reasons[row.Reason] = reason
			set.Reasons = append(set.Reasons, reason)
		}
		reason.Count += 1
		reason.PayoutNanoCents += row.PayoutNanoCents
		week, ok := weeks[[2]string{row.Week, row.Reason}]
		if !ok {
			week = &PayoutCloseParkedTotal{Week: row.Week, Reason: row.Reason}
			weeks[[2]string{row.Week, row.Reason}] = week
			set.Weeks = append(set.Weeks, week)
		}
		week.Count += 1
		week.PayoutNanoCents += row.PayoutNanoCents
		if row.ManualReimbursement {
			set.ManualReimbursements.Count += 1
			set.ManualReimbursements.PayoutNanoCents += row.PayoutNanoCents
		}
	}
	reasonOrder := func(reason string) int {
		return slices.Index(payoutCloseParkedReasons, reason)
	}
	slices.SortFunc(set.Reasons, func(a *PayoutCloseParkedTotal, b *PayoutCloseParkedTotal) int {
		return reasonOrder(a.Reason) - reasonOrder(b.Reason)
	})
	slices.SortFunc(set.Weeks, func(a *PayoutCloseParkedTotal, b *PayoutCloseParkedTotal) int {
		if a.Week != b.Week {
			return strings.Compare(a.Week, b.Week)
		}
		return reasonOrder(a.Reason) - reasonOrder(b.Reason)
	})
	return set
}

// sha256 over each parked payment's id and payout, in id order: the apply
// closes exactly the payments, with exactly the payouts, that the dry run
// reported.
func payoutCloseParkedDigest(rows []*PayoutCloseParkedRow) string {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a *PayoutCloseParkedRow, b *PayoutCloseParkedRow) int {
		return a.PaymentId.Cmp(b.PaymentId)
	})
	lines := []string{"urnetwork payouts close-parked v1"}
	for _, row := range sorted {
		lines = append(lines, fmt.Sprintf("payment %s %d", row.PaymentId, row.PayoutNanoCents))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// The audit record of one closure. It names the switch and the run, and keeps
// what the run read: the reason, the plan week and the forfeited payout.
type payoutCloseParkedEvent struct {
	Switch              string          `json:"switch"`
	RunId               server.Id       `json:"run_id"`
	Digest              string          `json:"digest"`
	Reason              string          `json:"reason"`
	Week                string          `json:"plan_week"`
	PaymentPlanId       server.Id       `json:"payment_plan_id"`
	NetworkId           server.Id       `json:"network_id"`
	WalletId            *server.Id      `json:"wallet_id"`
	PayoutNanoCents     model.NanoCents `json:"payout_nano_cents"`
	ManualReimbursement bool            `json:"manual_reimbursement"`
}

type PayoutCloseParkedReport struct {
	RunId server.Id `json:"run_id"`
	Time  time.Time `json:"time"`
	Mode  string    `json:"mode"`
	// usdc_payout.yml payments: on, off, or why it is unreadable
	UsdcPayments string `json:"usdc_payments"`
	// the sender's fixed fee per payout
	FeeNanoCents model.NanoCents `json:"fee_nano_cents"`
	Digest       string          `json:"digest"`
	// the census: every parked payment
	Parked *PayoutCloseParkedSet `json:"parked"`
	// pending payments closed before and un-canceled since; never closed again
	ClosedPendingCount int64 `json:"closed_pending_count"`
	// --apply: the payments this run closed, and the census payments it
	// skipped because they changed since the census
	Closed  *PayoutCloseParkedSet   `json:"closed,omitempty"`
	Skipped []*PayoutCloseParkedRow `json:"skipped,omitempty"`
}

// Runs the dry run, or with `apply` the close-out, of the parked payments.
func PayoutsCloseParked(clientSession *session.ClientSession, apply bool, expect string) (*PayoutCloseParkedReport, error) {
	ctx := clientSession.Ctx
	report := &PayoutCloseParkedReport{
		RunId:        server.NewId(),
		Time:         server.NowUtc(),
		Mode:         "dry_run",
		FeeNanoCents: model.UsdToNanoCents(providerPaymentFeeUsd),
	}
	if apply {
		report.Mode = "apply"
	}
	policy, policyErr := server.LoadUsdcPayoutPolicy()
	switch {
	case policyErr != nil:
		report.UsdcPayments = "unreadable: " + policyErr.Error()
	default:
		report.UsdcPayments = policy.Payments
	}

	parked := model.GetUsdcParkedPayments(ctx)
	report.Parked = newPayoutCloseParkedSet(parked)
	report.Digest = payoutCloseParkedDigest(report.Parked.Rows)
	report.ClosedPendingCount = model.GetUsdcPayoutClosedPendingCount(ctx)
	if !apply {
		return report, nil
	}

	// refuse before any write
	if err := requireUsdcPaymentsOff(); err != nil {
		return report, fmt.Errorf("%w; nothing was closed", err)
	}
	if expect != report.Digest {
		return report, fmt.Errorf("%w; nothing was closed", ErrPayoutCloseParkedDigestMismatch)
	}
	closed := []*model.UsdcParkedPayment{}
	skipped := []*model.UsdcParkedPayment{}
	finish := func(err error) (*PayoutCloseParkedReport, error) {
		report.Closed = newPayoutCloseParkedSet(closed)
		report.Skipped = newPayoutCloseParkedSet(skipped).Rows
		switch {
		case err != nil:
			err = fmt.Errorf("payouts close-parked: stopped with %d of the census's %d payment(s) closed and %d skipped; run the dry run again: %w",
				len(closed), len(parked), len(skipped), err)
		case 0 < len(skipped):
			err = fmt.Errorf("payouts close-parked: %d payment(s) changed since the census and were not closed; run the dry run again", len(skipped))
		}
		return report, err
	}
	for start := 0; start < len(parked); start += payoutCloseParkedBatchSize {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		// the switch may have turned on since the run began
		if err := requireUsdcPaymentsOff(); err != nil {
			return finish(err)
		}
		batch := parked[start:min(start+payoutCloseParkedBatchSize, len(parked))]
		if payoutCloseParkedBeforeBatch != nil {
			paymentIds := []server.Id{}
			for _, payment := range batch {
				paymentIds = append(paymentIds, payment.PaymentId)
			}
			payoutCloseParkedBeforeBatch(paymentIds)
		}
		batchClosed, err := closePayoutParkedBatch(clientSession, report, batch)
		if err != nil {
			// the batch's transaction rolled back; earlier batches stay closed
			return finish(err)
		}
		closedIds := map[server.Id]bool{}
		for _, payment := range batchClosed {
			closedIds[payment.PaymentId] = true
		}
		closed = append(closed, batchClosed...)
		for _, payment := range batch {
			if !closedIds[payment.PaymentId] {
				skipped = append(skipped, payment)
			}
		}
	}
	return finish(nil)
}

// Closes one batch in its own transaction, journaling each closure with the
// run. A failed transaction rolls back and returns its error.
func closePayoutParkedBatch(
	clientSession *session.ClientSession,
	report *PayoutCloseParkedReport,
	batch []*model.UsdcParkedPayment,
) (closed []*model.UsdcParkedPayment, returnErr error) {
	defer func() {
		if value := recover(); value != nil {
			if err, ok := value.(error); ok {
				closed, returnErr = nil, err
				return
			}
			panic(value)
		}
	}()
	closed = model.CloseUsdcParkedPayments(clientSession.Ctx, batch, server.NowUtc(), func(payment *model.UsdcParkedPayment) string {
		details, err := json.Marshal(&payoutCloseParkedEvent{
			Switch:              payoutCloseParkedSwitch,
			RunId:               report.RunId,
			Digest:              report.Digest,
			Reason:              payment.Reason(providerPaymentFeeUsd),
			Week:                payoutCloseParkedWeek(payment.CreateTime),
			PaymentPlanId:       payment.PaymentPlanId,
			NetworkId:           payment.NetworkId,
			WalletId:            payment.WalletId,
			PayoutNanoCents:     payment.Payout,
			ManualReimbursement: payment.ManualReimbursement,
		})
		server.Raise(err)
		return string(details)
	})
	return
}

func payoutCloseParkedSetText(b *strings.Builder, set *PayoutCloseParkedSet) {
	fmt.Fprintf(b, "  by reason:\n")
	for _, total := range set.Reasons {
		fmt.Fprintf(b, "    %-12s %7d  %20s USD\n", total.Reason, total.Count, formatPayoutCloseParkedUsd(total.PayoutNanoCents))
	}
	fmt.Fprintf(b, "  by plan week (UTC Monday) and reason:\n")
	for _, total := range set.Weeks {
		fmt.Fprintf(b, "    %s %-12s %7d  %20s USD\n", total.Week, total.Reason, total.Count, formatPayoutCloseParkedUsd(total.PayoutNanoCents))
	}
	fmt.Fprintf(b, "  of which manual reimbursements: %d, %s USD\n", set.ManualReimbursements.Count, formatPayoutCloseParkedUsd(set.ManualReimbursements.PayoutNanoCents))
}

func payoutCloseParkedRowsText(b *strings.Builder, name string, rows []*PayoutCloseParkedRow) {
	for _, row := range rows {
		fmt.Fprintf(b, "  %s %s week %s %s %s USD", name, row.PaymentId, row.Week, row.Reason, formatPayoutCloseParkedUsd(row.PayoutNanoCents))
		if row.ManualReimbursement {
			fmt.Fprintf(b, " manual_reimbursement")
		}
		fmt.Fprintf(b, "\n")
	}
}

// Text renders the report for an operator. Payment ids are listed only with
// `ids`.
func (self *PayoutCloseParkedReport) Text(ids bool) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "payouts close-parked run %s at %s, mode %s\n", self.RunId, self.Time.Format(time.RFC3339), self.Mode)
	fmt.Fprintf(b, "usdc payments: %s (usdc_payout.yml); closing requires payments: off\n", self.UsdcPayments)
	fmt.Fprintf(b, "scope: pending payments Circle cannot hold: not completed or canceled, no idempotency key, payment record or tx hash,\n")
	fmt.Fprintf(b, "  no staged sweep assignment, not closed before; closing forfeits the USDC obligation and no plan re-plans its sweeps\n")
	fmt.Fprintf(b, "amounts: exact USD, 1 USD = 1000000000 nano cents; the sender's fee is %s USD (%d nano cents)\n", formatPayoutCloseParkedUsd(self.FeeNanoCents), self.FeeNanoCents)
	fmt.Fprintf(b, "reasons, the first hold the sender would apply if payments were on:\n")
	fmt.Fprintf(b, "  %s: no active wallet of its network to pay\n", model.UsdcParkedWalletLess)
	fmt.Fprintf(b, "  %s: the payout does not cover the fee\n", model.UsdcParkedDust)
	fmt.Fprintf(b, "  %s: attribution review required\n", model.UsdcParkedReviewHeld)
	fmt.Fprintf(b, "  %s: parked only because payments are off\n", model.UsdcParkedOther)
	fmt.Fprintf(b, "parked: %d payment(s), %s USD (%d nano cents)\n", self.Parked.Count, formatPayoutCloseParkedUsd(self.Parked.PayoutNanoCents), self.Parked.PayoutNanoCents)
	payoutCloseParkedSetText(b, self.Parked)
	fmt.Fprintf(b, "closed before and pending again (never closed twice): %d\n", self.ClosedPendingCount)
	if ids {
		payoutCloseParkedRowsText(b, "parked", self.Parked.Rows)
	}
	fmt.Fprintf(b, "digest %s\n", self.Digest)
	if self.Closed != nil {
		fmt.Fprintf(b, "closed: %d payment(s), %s USD (%d nano cents)\n", self.Closed.Count, formatPayoutCloseParkedUsd(self.Closed.PayoutNanoCents), self.Closed.PayoutNanoCents)
		payoutCloseParkedSetText(b, self.Closed)
		if ids {
			payoutCloseParkedRowsText(b, "closed", self.Closed.Rows)
		}
		fmt.Fprintf(b, "skipped, changed since the census: %d\n", len(self.Skipped))
		if ids {
			payoutCloseParkedRowsText(b, "skipped", self.Skipped)
		}
	}
	return b.String()
}
