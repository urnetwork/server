// Legacy keyed provider payments: a submit before processor requests were
// retained (2026-10-02) created the payment's idempotency key and then either
// failed at Circle or lost Circle's response, without binding a record. A key
// without its retained request never authorizes a retry
// (GetRetainedProviderPaymentRequest), so these payments stay held until a
// reconciliation decides from Circle's own transaction history:
//   - a matching Circle transfer exists: bind its record, and the ordinary
//     status flow completes or fails the payment
//   - Circle affirmatively lists no transfer that could be the attempt:
//     release the key, so the next AdvancePayment reserves a fresh attempt
//     with a retained request
//   - anything else keeps the payment held
//
// Both writes lock the exact observed attempt and journal the decision in the
// same transaction. A payment that changed, gained a retained request, or whose
// transfer is bound elsewhere refuses without a write.
package model

import (
	"context"

	"github.com/urnetwork/server"
)

// A pending payment with a legacy key, and the wallet its submits paid.
// The wallet columns are empty when the wallet row is missing.
type LegacyProviderPaymentAttempt struct {
	Payment         *AccountPayment
	WalletAddress   string
	Blockchain      string
	WalletNetworkId *server.Id
}

const legacyProviderPaymentAttemptSql = `
	NOT p.completed AND
	NOT p.canceled AND
	p.circle_idempotency_key IS NOT NULL AND
	p.payment_record IS NULL AND
	p.tx_hash IS NULL AND
	NOT EXISTS (
		SELECT 1 FROM audit_account_payment request
		WHERE
			request.event_id = p.circle_idempotency_key AND
			request.payment_id = p.payment_id AND
			request.event_type = 'circle_attempt_request'
	)`

// Every pending payment whose idempotency key has no retained request and no
// processor record, ordered by payment id.
func GetLegacyProviderPaymentAttempts(ctx context.Context) (attempts []*LegacyProviderPaymentAttempt, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		type attemptWallet struct {
			paymentId       server.Id
			walletAddress   *string
			blockchain      *string
			walletNetworkId *server.Id
		}
		attemptWallets := []attemptWallet{}
		result, err := conn.Query(
			ctx,
			`
			SELECT
				p.payment_id,
				w.wallet_address,
				w.blockchain,
				w.network_id
			FROM account_payment p
			LEFT JOIN account_wallet w ON w.wallet_id = p.wallet_id
			WHERE `+legacyProviderPaymentAttemptSql+`
			ORDER BY p.payment_id
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var row attemptWallet
				server.Raise(result.Scan(&row.paymentId, &row.walletAddress, &row.blockchain, &row.walletNetworkId))
				attemptWallets = append(attemptWallets, row)
			}
		})
		for _, row := range attemptWallets {
			payment, err := dbGetPayment(ctx, conn, row.paymentId)
			if err != nil {
				returnErr = err
				return
			}
			if payment == nil {
				continue
			}
			attempt := &LegacyProviderPaymentAttempt{
				Payment:         payment,
				WalletNetworkId: row.walletNetworkId,
			}
			if row.walletAddress != nil {
				attempt.WalletAddress = *row.walletAddress
			}
			if row.blockchain != nil {
				attempt.Blockchain = *row.blockchain
			}
			attempts = append(attempts, attempt)
		}
	})
	return
}

// Every Circle transaction id bound to a provider payment attempt: current
// records and records retained by journaled outcomes and late acceptances.
// A bound transaction belongs to its own attempt's key.
func GetProviderPaymentProcessorRecords(ctx context.Context) (records map[string]bool, returnErr error) {
	records = map[string]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT DISTINCT record FROM (`+providerPaymentProcessorOwnershipSql+`) owned WHERE record IS NOT NULL AND record<>''`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var record string
				server.Raise(result.Scan(&record))
				records[record] = true
			}
		})
	})
	return
}

// The blockchain and destination of every pending attempt that has a retained
// request but no record yet: its transfer may exist at Circle unbound.
func GetRetainedProviderPaymentDestinations(ctx context.Context) (destinations map[[2]string]bool, returnErr error) {
	destinations = map[[2]string]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT w.blockchain, w.wallet_address
			FROM account_payment p
			INNER JOIN account_wallet w ON w.wallet_id = p.wallet_id
			WHERE
				NOT p.completed AND
				NOT p.canceled AND
				p.circle_idempotency_key IS NOT NULL AND
				p.payment_record IS NULL AND
				EXISTS (
					SELECT 1 FROM audit_account_payment request
					WHERE
						request.event_id = p.circle_idempotency_key AND
						request.payment_id = p.payment_id AND
						request.event_type = 'circle_attempt_request'
				)
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var blockchain, walletAddress string
				server.Raise(result.Scan(&blockchain, &walletAddress))
				destinations[[2]string{blockchain, walletAddress}] = true
			}
		})
	})
	return
}

// Locks the exact observed legacy attempt in tx. False when anything changed,
// including a retained request appearing for its key.
func lockLegacyProviderPaymentAttemptInTx(ctx context.Context, tx server.PgTx, expected *AccountPayment) bool {
	if expected == nil || expected.CircleIdempotencyKey == nil || expected.PaymentRecord != nil || expected.TxHash != nil {
		return false
	}
	if !lockProviderPaymentAttempt(ctx, tx, expected) {
		return false
	}
	var legacy bool
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM account_payment p WHERE p.payment_id = $1 AND `+legacyProviderPaymentAttemptSql+`)`,
		expected.PaymentId,
	).Scan(&legacy))
	return legacy
}

// BindLegacyProviderPaymentRecord binds the Circle transfer found for a legacy
// attempt, with the amount that attempt submitted. The ordinary AdvancePayment
// status flow then reads the record. observation is journaled with the binding.
func BindLegacyProviderPaymentRecord(ctx context.Context, expected *AccountPayment, record string, amount float64, observation string) (returnErr error) {
	if record == "" || len(record) > 4096 || !(0 < amount) || providerPaymentReceiptLimit < len(observation) {
		return ErrProviderPaymentAttemptChanged
	}
	wallet, err := legacyProviderPaymentWallet(ctx, expected)
	if err != nil {
		return err
	}
	server.OwnedTx(ctx, providerPaymentAttributionKeys(expected.PaymentId, wallet.Blockchain, wallet.WalletAddress, record), func(tx server.PgTx) {
		returnErr = nil
		if !lockLegacyProviderPaymentAttributionInTx(ctx, tx, expected, wallet) {
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		if providerPaymentRecordClaimedInTx(ctx, tx, record, nil) {
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		retainProviderAttemptInTx(ctx, tx, expected, "LEGACY_RECONCILED_FOUND", observation)
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE account_payment SET token_type='USDC', token_amount=$2, payment_record=$3, payment_time=$4, payment_receipt=NULL WHERE payment_id=$1`,
			expected.PaymentId,
			amount,
			record,
			server.NowUtc(),
		))
	}, server.TxReadCommitted)
	return
}

// ReleaseLegacyProviderPaymentKey clears the key of a legacy attempt that
// Circle affirmatively shows created no transfer. The next AdvancePayment
// reserves a fresh key with a retained request. observation is journaled with
// the release.
func ReleaseLegacyProviderPaymentKey(ctx context.Context, expected *AccountPayment, observation string) (returnErr error) {
	if providerPaymentReceiptLimit < len(observation) {
		return ErrProviderPaymentAttemptChanged
	}
	wallet, err := legacyProviderPaymentWallet(ctx, expected)
	if err != nil {
		return err
	}
	server.OwnedTx(ctx, providerPaymentAttributionKeys(expected.PaymentId, wallet.Blockchain, wallet.WalletAddress, ""), func(tx server.PgTx) {
		returnErr = nil
		if !lockLegacyProviderPaymentAttributionInTx(ctx, tx, expected, wallet) {
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		retainProviderAttemptInTx(ctx, tx, expected, "LEGACY_RECONCILED_ABSENT", observation)
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE account_payment SET circle_idempotency_key=NULL WHERE payment_id=$1`,
			expected.PaymentId,
		))
	}, server.TxReadCommitted)
	return
}
