// Closing the parked USDC payments (SN-RECONCILE.md, "Closing the parked
// payments"). Once usdc_payout.yml turns payments off, a pending payment that
// Circle does not hold is never reserved, submitted or scheduled again. The
// close-out (`bringyourctl payouts close-parked`) cancels each one and journals
// a usdc_payout_closed event. The obligation is forfeited: post-cutoff earnings
// are paid through the subnet, and legacy debt never becomes subnet credit.
//
// The journal row is the marker. Its event id derives from the payment id
// (usdcPayoutClosedEventIdSql), so every check finds it with a primary key
// probe, as checks find a manual reimbursement's marker. A closed payment looks
// safely canceled, so every read that would re-plan or restore a safely
// canceled payment excludes it:
//   - the planner never selects its sweeps (the canceled arm of planPayments),
//     so they are never re-planned and its bonuses never move with them
//     (carryPaymentBonuses moves only the bonuses of selected sweeps)
//   - legacy component recovery never restores it (recoverLegacyComponents)
//   - the planner's unresolved-liability sample never counts its sweeps
//
// The daily hung-payment cancellation cancels only payments that are not
// canceled, so it never reaches a closed payment. A payment canceled any other
// way is unaffected.
package model

import (
	"context"
	"crypto/md5"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

const UsdcPayoutClosedEventType = "usdc_payout_closed"

// The reasons a parked payment is held, in the order the sender would hold it
// if payments were on (AdvancePayment): first a missing wallet, then a payout
// that does not cover the transfer fee, then the earning admission's
// attribution review hold. A payment with none of them is parked only because
// payments are off.
const (
	UsdcParkedWalletLess = "wallet_less"
	UsdcParkedDust       = "dust"
	UsdcParkedReviewHeld = "review_held"
	UsdcParkedOther      = "other"
)

// The event id of the closure of the payment `%[1]s`, a uuid expression: the
// md5 of a fixed prefix and the payment id's text. SQL and Go
// (UsdcPayoutClosedEventId) derive the same id, and it is never the payment id
// itself, which a manual reimbursement or legacy replacement marker takes.
const usdcPayoutClosedEventIdSql = `md5('` + UsdcPayoutClosedEventType + `/' || (%[1]s)::text)::uuid`

func UsdcPayoutClosedEventId(paymentId server.Id) server.Id {
	sum := md5.Sum([]byte(UsdcPayoutClosedEventType + "/" + paymentId.String()))
	return server.RequireIdFromBytes(sum[:])
}

// The account_payment row `%[1]s` was closed by the close-out. The probe is
// fenced by OFFSET 0, so it stays one primary key probe per row and the planner
// never turns it into a scan of the payment journal.
const usdcPayoutClosedSql = `EXISTS (SELECT 1 FROM audit_account_payment usdc_payout_closed
	WHERE usdc_payout_closed.event_id = ` + usdcPayoutClosedEventIdSql + `
	AND usdc_payout_closed.payment_id = %[1]s
	AND usdc_payout_closed.event_type = '` + UsdcPayoutClosedEventType + `' OFFSET 0)`

func usdcPayoutClosed(paymentAlias string) string {
	return fmt.Sprintf(usdcPayoutClosedSql, paymentAlias+".payment_id")
}

// A parked payment `p`: a pending payment that Circle cannot hold. It is not
// completed or canceled; it has no idempotency key, payment record or tx hash,
// so no attempt can have reached Circle; its plan's staged sweep assignment is
// not pending (paymentPlanAssignmentPendingSql: a pending plan's payments are
// not payable, and their selected sweeps are still being moved to them); and it
// was not closed before. A keyed or recorded payment is never parked: its
// reconciliation decides it.
var usdcParkedPaymentSql = `NOT p.completed AND NOT p.canceled AND
	p.circle_idempotency_key IS NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL AND
	NOT ` + paymentPlanAssignmentPendingSql + ` AND
	NOT ` + usdcPayoutClosed("p")

// The columns of a parked payment `p` that the close-out reads.
//
// wallet_less: the sender would find no wallet to pay. Neither the payment's
// own wallet nor, for a payment the sender re-points (UpdatePaymentWallet, never
// a manual reimbursement), the network's payout wallet is an active wallet of
// the payment's network.
var usdcParkedPaymentColumnsSql = `p.payment_id, p.payment_plan_id, p.network_id, p.wallet_id, p.payout_nano_cents,
	p.attribution_review_required, p.create_time,
	` + manualReimbursementMarked("p") + ` AS manual_reimbursement,
	NOT EXISTS (SELECT 1 FROM account_wallet own
		WHERE own.wallet_id = p.wallet_id AND own.network_id = p.network_id AND own.active)
	AND (` + manualReimbursementMarked("p") + ` OR NOT EXISTS (SELECT 1 FROM payout_wallet
		INNER JOIN account_wallet payout ON payout.wallet_id = payout_wallet.wallet_id AND payout.network_id = p.network_id AND payout.active
		WHERE payout_wallet.network_id = p.network_id)) AS wallet_less`

// One parked payment as the close-out read it.
type UsdcParkedPayment struct {
	PaymentId                 server.Id
	PaymentPlanId             server.Id
	NetworkId                 server.Id
	WalletId                  *server.Id
	Payout                    NanoCents
	AttributionReviewRequired bool
	CreateTime                time.Time
	ManualReimbursement       bool
	WalletLess                bool
}

func (self *UsdcParkedPayment) scanArgs() []any {
	return []any{&self.PaymentId, &self.PaymentPlanId, &self.NetworkId, &self.WalletId, &self.Payout,
		&self.AttributionReviewRequired, &self.CreateTime, &self.ManualReimbursement, &self.WalletLess}
}

// The first hold the sender would apply if payments were on. The payout covers
// the fee `feeUsd` by the sender's own arithmetic (advancePayment): the payout
// in USD less the fee must round to a positive number of nano cents.
func (self *UsdcParkedPayment) Reason(feeUsd float64) string {
	switch {
	case self.WalletLess:
		return UsdcParkedWalletLess
	case UsdToNanoCents(NanoCentsToUsd(self.Payout)-feeUsd) <= 0:
		return UsdcParkedDust
	case self.AttributionReviewRequired:
		return UsdcParkedReviewHeld
	default:
		return UsdcParkedOther
	}
}

// Every parked payment, by payment id. Read only.
func GetUsdcParkedPayments(ctx context.Context) (parked []*UsdcParkedPayment) {
	server.Db(ctx, func(conn server.PgConn) {
		parked = []*UsdcParkedPayment{}
		result, err := conn.Query(ctx, `SELECT `+usdcParkedPaymentColumnsSql+`
			FROM account_payment p
			WHERE `+usdcParkedPaymentSql+`
			ORDER BY p.payment_id`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				payment := &UsdcParkedPayment{}
				server.Raise(result.Scan(payment.scanArgs()...))
				parked = append(parked, payment)
			}
		})
	})
	return
}

// The pending payments that hold a closure event: closed, then un-canceled by
// a writer that does not know the marker, such as an older planner's legacy
// recovery. The close-out never closes one again. Read only.
func GetUsdcPayoutClosedPendingCount(ctx context.Context) (count int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM account_payment p
			WHERE NOT p.completed AND NOT p.canceled AND `+usdcPayoutClosed("p")).Scan(&count))
	})
	return
}

// Closes the payments of `batch` that are still parked, each with the payout
// the census read, in one READ COMMITTED transaction. The payments are locked
// in id order and read again; each is canceled at `closeTime` and journaled
// with one usdc_payout_closed event, keyed by its derived event id, whose
// details `details` writes from the locked row. A payment that changed since
// the census (now keyed, recorded, canceled, completed, staged, closed, or with
// another payout) is not written and not returned.
func CloseUsdcParkedPayments(
	ctx context.Context,
	batch []*UsdcParkedPayment,
	closeTime time.Time,
	details func(*UsdcParkedPayment) string,
) (closed []*UsdcParkedPayment) {
	if len(batch) == 0 {
		return []*UsdcParkedPayment{}
	}
	paymentIds := make([]server.Id, 0, len(batch))
	payouts := make([]NanoCents, 0, len(batch))
	for _, payment := range batch {
		paymentIds = append(paymentIds, payment.PaymentId)
		payouts = append(payouts, payment.Payout)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		// the callback can run again; nothing of an earlier attempt committed
		closed = []*UsdcParkedPayment{}
		result, err := tx.Query(ctx, `SELECT `+usdcParkedPaymentColumnsSql+`
			FROM account_payment p
			INNER JOIN unnest($1::uuid[], $2::bigint[]) AS census(payment_id, payout_nano_cents) ON
				census.payment_id = p.payment_id AND census.payout_nano_cents = p.payout_nano_cents
			WHERE `+usdcParkedPaymentSql+`
			ORDER BY p.payment_id
			FOR UPDATE OF p`, paymentIds, payouts)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				payment := &UsdcParkedPayment{}
				server.Raise(result.Scan(payment.scanArgs()...))
				closed = append(closed, payment)
			}
		})
		if len(closed) == 0 {
			return
		}
		closedIds := make([]server.Id, 0, len(closed))
		closedDetails := make([]string, 0, len(closed))
		for _, payment := range closed {
			closedIds = append(closedIds, payment.PaymentId)
			closedDetails = append(closedDetails, details(payment))
		}
		tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment p
			SET canceled = true, cancel_time = $2
			WHERE p.payment_id = ANY($1) AND NOT p.completed AND NOT p.canceled AND
				p.circle_idempotency_key IS NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL`,
			closedIds, closeTime))
		if tag.RowsAffected() != int64(len(closed)) {
			panic(fmt.Errorf("usdc close-out canceled %d of %d locked payments", tag.RowsAffected(), len(closed)))
		}
		tag = server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment (event_id, event_time, payment_id, event_type, event_details)
			SELECT `+fmt.Sprintf(usdcPayoutClosedEventIdSql, "closed.payment_id")+`, $3, closed.payment_id, '`+UsdcPayoutClosedEventType+`', closed.details
			FROM unnest($1::uuid[], $2::text[]) AS closed(payment_id, details)`,
			closedIds, closedDetails, closeTime))
		if tag.RowsAffected() != int64(len(closed)) {
			panic(fmt.Errorf("usdc close-out journaled %d of %d closed payments", tag.RowsAffected(), len(closed)))
		}
	}, server.TxReadCommitted)
	return
}
