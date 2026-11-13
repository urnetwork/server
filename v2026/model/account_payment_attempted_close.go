// The canceled USDC payments that an attempt of which reached Circle, with a
// Circle idempotency key, payment record or tx hash, and that the USDC
// close-out never closed (owner decision 2026-11-13: "1,2,3,4 accept
// recommendations, go ahead and fix"). A one-time pass
// (`bringyourctl payouts close-attempted`, controller
// account_payment_attempted_close.go) reads each one's transfer from Circle
// and closes it as forfeited when no transfer moved funds, or records it paid
// when its transfer completed. A closed payment is journaled with the
// close-out's usdc_payout_closed event, under the same event id, so retention
// counts it settled (server.AccountPaymentRetentionSettledSql). A paid one is
// completed as the sender's status check completed one, and journaled too.
// Each write locks the payment and reads it again first; a payment that
// changed since the census is not written.
//
// The pass holds a payment it cannot decide from Circle. The owner can forfeit
// held payments by id (`--forfeit`, owner decision 2026-11-13 on the six
// lookup_failed ones): they are read from Main only and closed all together,
// with the same event, or not at all.
package model

import (
	"context"
	"crypto/md5"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The closure event id of the payment, as SQL derives it
// (server.UsdcPayoutClosedEventIdSql).
func UsdcPayoutClosedEventId(paymentId server.Id) server.Id {
	sum := md5.Sum([]byte(server.UsdcPayoutClosedEventType + "/" + paymentId.String()))
	return server.RequireIdFromBytes(sum[:])
}

// A canceled payment `p` that is not completed, has a Circle idempotency key,
// payment record or tx hash, and was not closed.
var usdcAttemptedPaymentSql = `p.canceled AND NOT p.completed AND
	(p.circle_idempotency_key IS NOT NULL OR p.payment_record IS NOT NULL OR p.tx_hash IS NOT NULL) AND
	NOT ` + server.UsdcPayoutClosedSql("p.payment_id")

// Every Circle transfer a payment owns: its payment record, and the records
// its retired attempts journaled (circle_attempt_outcome,
// circle_late_acceptance), as the sender bound them.
const usdcPaymentRecordOwnershipSql = `
	SELECT payment_id, payment_record record
	FROM account_payment WHERE payment_record IS NOT NULL
	UNION ALL
	SELECT payment_id, event_details::jsonb->'attempt'->>'payment_record'
	FROM audit_account_payment WHERE event_type='circle_attempt_outcome'
	UNION ALL
	SELECT payment_id, event_details::jsonb->>'record'
	FROM audit_account_payment WHERE event_type='circle_late_acceptance'
	UNION ALL
	SELECT payment_id,
		(CASE WHEN event_details::jsonb->>'status' IN ('ACCEPTED','LEGACY_RECONCILED_FOUND','CONTRADICTORY_ACCEPTANCE')
		THEN (event_details::jsonb->>'response')::jsonb ELSE NULL END)->>'record'
	FROM audit_account_payment WHERE event_type='circle_attempt_outcome'
`

// One payment of the pass, with its pinned wallet when that wallet is its
// network's.
type UsdcAttemptedPayment struct {
	PaymentId                 server.Id
	NetworkId                 server.Id
	WalletId                  *server.Id
	Payout                    NanoCents
	CircleIdempotencyKey      *server.Id
	PaymentRecord             *string
	TxHash                    *string
	TokenAmount               *float64
	CreateTime                time.Time
	AttributionReviewRequired bool
	// the legacy correction replaced it with a corrected payment
	Replaced      bool
	WalletAddress *string
	Blockchain    *string
}

func (self *UsdcAttemptedPayment) scanArgs() []any {
	return []any{&self.PaymentId, &self.NetworkId, &self.WalletId, &self.Payout, &self.CircleIdempotencyKey,
		&self.PaymentRecord, &self.TxHash, &self.TokenAmount, &self.CreateTime, &self.AttributionReviewRequired,
		&self.Replaced, &self.WalletAddress, &self.Blockchain}
}

// The columns of `p` the pass reads.
var usdcAttemptedPaymentColumnsSql = `p.payment_id, p.network_id, p.wallet_id, p.payout_nano_cents, p.circle_idempotency_key,
	p.payment_record, p.tx_hash, p.token_amount, p.create_time, p.attribution_review_required,
	` + legacyProviderPaymentReplacedSql("p.payment_id") + `,
	w.wallet_address, w.blockchain`

// Every payment of the pass, by payment id. Read only.
func GetUsdcAttemptedPayments(ctx context.Context) (payments []*UsdcAttemptedPayment) {
	server.Db(ctx, func(conn server.PgConn) {
		payments = []*UsdcAttemptedPayment{}
		result, err := conn.Query(ctx, `SELECT `+usdcAttemptedPaymentColumnsSql+`
			FROM account_payment p
			LEFT JOIN account_wallet w ON w.wallet_id = p.wallet_id AND w.network_id = p.network_id
			WHERE `+usdcAttemptedPaymentSql+`
			ORDER BY p.payment_id`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				payment := &UsdcAttemptedPayment{}
				server.Raise(result.Scan(payment.scanArgs()...))
				payments = append(payments, payment)
			}
		})
	})
	return
}

// The payments owning each Circle transfer, by transfer id. Read only.
func GetUsdcPaymentRecordOwners(ctx context.Context) (owners map[string][]server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		owners = map[string][]server.Id{}
		result, err := conn.Query(ctx, `SELECT DISTINCT record, payment_id FROM (`+usdcPaymentRecordOwnershipSql+`) owned
			WHERE record IS NOT NULL AND record <> ''
			ORDER BY record, payment_id`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var record string
				var paymentId server.Id
				server.Raise(result.Scan(&record, &paymentId))
				owners[record] = append(owners[record], paymentId)
			}
		})
	})
	return
}

// Locks the payment in `tx` if it is still as `payment` read it: canceled,
// not completed, with the same payout, idempotency key, payment record and tx
// hash, and not closed.
func lockUsdcAttemptedPaymentInTx(ctx context.Context, tx server.PgTx, payment *UsdcAttemptedPayment) (locked bool) {
	result, err := tx.Query(ctx, `SELECT true FROM account_payment p
		WHERE p.payment_id = $1 AND p.payout_nano_cents = $2 AND
			p.circle_idempotency_key IS NOT DISTINCT FROM $3::uuid AND
			p.payment_record IS NOT DISTINCT FROM $4::text AND
			p.tx_hash IS NOT DISTINCT FROM $5::text AND
			`+usdcAttemptedPaymentSql+`
		FOR UPDATE OF p`, payment.PaymentId, payment.Payout, payment.CircleIdempotencyKey, payment.PaymentRecord, payment.TxHash)
	server.WithPgResult(result, err, func() {
		locked = result.Next()
	})
	return
}

// Journals the payment's usdc_payout_closed event at `eventTime`, whose
// details are `details`.
func insertUsdcPayoutClosedEventInTx(ctx context.Context, tx server.PgTx, paymentId server.Id, eventTime time.Time, details string) {
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment (event_id, event_time, payment_id, event_type, event_details)
		VALUES ($1, $2, $3, '`+server.UsdcPayoutClosedEventType+`', $4)`,
		UsdcPayoutClosedEventId(paymentId), eventTime, paymentId, details))
}

// Closes the payment as forfeited if it is still as `payment` read it: in one
// READ COMMITTED transaction the row is locked and journaled with one
// usdc_payout_closed event at `closeTime`, whose details are `details`. The
// payment stays canceled. Returns false, writing nothing, when it changed.
func CloseUsdcAttemptedPayment(ctx context.Context, payment *UsdcAttemptedPayment, closeTime time.Time, details string) (closed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		// the callback can run again; nothing of an earlier attempt committed
		closed = false
		if !lockUsdcAttemptedPaymentInTx(ctx, tx, payment) {
			return
		}
		insertUsdcPayoutClosedEventInTx(ctx, tx, payment.PaymentId, closeTime, details)
		closed = true
	}, server.TxReadCommitted)
	return
}

// A payment named for the owner's forfeit, as it is now.
type UsdcForfeitPayment struct {
	UsdcAttemptedPayment
	Canceled  bool
	Completed bool
	// it has a Circle idempotency key, payment record or tx hash
	Attempted bool
	// it has its usdc_payout_closed event
	Closed bool
}

// The named payments that exist, by payment id, whatever their state. Read
// only.
func GetUsdcForfeitPayments(ctx context.Context, paymentIds []server.Id) (payments map[server.Id]*UsdcForfeitPayment) {
	server.Db(ctx, func(conn server.PgConn) {
		payments = map[server.Id]*UsdcForfeitPayment{}
		result, err := conn.Query(ctx, `SELECT `+usdcAttemptedPaymentColumnsSql+`,
			p.canceled, p.completed,
			(p.circle_idempotency_key IS NOT NULL OR p.payment_record IS NOT NULL OR p.tx_hash IS NOT NULL),
			`+server.UsdcPayoutClosedSql("p.payment_id")+`
			FROM account_payment p
			LEFT JOIN account_wallet w ON w.wallet_id = p.wallet_id AND w.network_id = p.network_id
			WHERE p.payment_id = ANY($1::uuid[])`, paymentIds)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				payment := &UsdcForfeitPayment{}
				args := append(payment.scanArgs(), &payment.Canceled, &payment.Completed, &payment.Attempted, &payment.Closed)
				server.Raise(result.Scan(args...))
				payments[payment.PaymentId] = payment
			}
		})
	})
	return
}

// Closes every payment as forfeited, all or none: in one READ COMMITTED
// transaction each row is locked, in payment id order, and only when every one
// is still as `payments` read it is each journaled with one usdc_payout_closed
// event at `closeTime`, whose details are details[payment id]. The payments
// stay canceled. Returns false, writing nothing, when any changed, or when
// `payments` is empty, repeats a payment or misses its details.
func CloseUsdcAttemptedPayments(ctx context.Context, payments []*UsdcAttemptedPayment, closeTime time.Time, details map[server.Id]string) (closed bool) {
	ordered := slices.Clone(payments)
	slices.SortFunc(ordered, func(a *UsdcAttemptedPayment, b *UsdcAttemptedPayment) int {
		return a.PaymentId.Cmp(b.PaymentId)
	})
	if len(ordered) == 0 {
		return false
	}
	for i, payment := range ordered {
		if _, ok := details[payment.PaymentId]; !ok || 0 < i && ordered[i-1].PaymentId == payment.PaymentId {
			return false
		}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		// the callback can run again; nothing of an earlier attempt committed
		closed = false
		// every lock before any write, so a changed payment leaves nothing
		// written
		for _, payment := range ordered {
			if !lockUsdcAttemptedPaymentInTx(ctx, tx, payment) {
				return
			}
		}
		for _, payment := range ordered {
			insertUsdcPayoutClosedEventInTx(ctx, tx, payment.PaymentId, closeTime, details[payment.PaymentId])
		}
		closed = true
	}, server.TxReadCommitted)
	return
}

// Records the payment paid by its Circle transfer `record`, completed on
// chain as `txHash`, if it is still as `payment` read it and no other payment
// owns the transfer: in one READ COMMITTED transaction owning the network's
// account balance, the row is locked, no
// longer canceled, completed at `completeTime` with the transfer as its
// payment record, `receipt` as its receipt and `txHash`, the network's paid
// totals are credited as a completion credits them, and the payment is
// journaled with one usdc_payout_closed event whose details are `details`.
// Returns false, writing nothing, when it changed or the transfer is another
// payment's.
func CompleteUsdcAttemptedPayment(
	ctx context.Context,
	payment *UsdcAttemptedPayment,
	record string,
	txHash string,
	receipt string,
	completeTime time.Time,
	details string,
) (completed bool) {
	if record == "" || txHash == "" {
		return false
	}
	// the paid totals are written under their network's owner
	server.OwnedTx(ctx, accountBalanceOwnershipKeys([]server.Id{payment.NetworkId}), func(tx server.PgTx) {
		// the callback can run again; nothing of an earlier attempt committed
		completed = false
		if !lockUsdcAttemptedPaymentInTx(ctx, tx, payment) {
			return
		}
		var claimed bool
		server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (`+usdcPaymentRecordOwnershipSql+`) owned
			WHERE record = $1 AND payment_id <> $2)`, record, payment.PaymentId).Scan(&claimed))
		if claimed {
			return
		}
		tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment
			SET
				canceled = false,
				completed = true,
				complete_time = $2,
				payment_record = COALESCE(payment_record, $3),
				payment_receipt = $4,
				tx_hash = $5
			WHERE payment_id = $1 AND canceled AND NOT completed`,
			payment.PaymentId, completeTime, record, receipt, txHash))
		if tag.RowsAffected() != 1 {
			panic(fmt.Errorf("usdc attempted close completed %d rows of the locked payment %s", tag.RowsAffected(), payment.PaymentId))
		}
		creditCompletedPaymentAccountBalanceInTx(ctx, tx, payment.PaymentId)
		insertUsdcPayoutClosedEventInTx(ctx, tx, payment.PaymentId, completeTime, details)
		completed = true
	}, server.TxReadCommitted)
	return
}

// Credits the network's lifetime paid totals with the completed payment, as a
// completion credited them, unless the payment completed under attribution
// review.
func creditCompletedPaymentAccountBalanceInTx(ctx context.Context, tx server.PgTx, paymentId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO account_balance (
			network_id,
			paid_byte_count,
			paid_net_revenue_nano_cents
		)
		SELECT
			account_payment.network_id,
			account_payment.payout_byte_count AS paid_byte_count,
			account_payment.payout_nano_cents AS paid_net_revenue_nano_cents
		FROM account_payment
		WHERE
			account_payment.payment_id = $1 AND NOT account_payment.attribution_review_required
		ON CONFLICT (network_id) DO UPDATE
		SET
			paid_byte_count = account_balance.paid_byte_count + EXCLUDED.paid_byte_count,
			paid_net_revenue_nano_cents = account_balance.paid_net_revenue_nano_cents + EXCLUDED.paid_net_revenue_nano_cents
		`,
		paymentId,
	))
}
