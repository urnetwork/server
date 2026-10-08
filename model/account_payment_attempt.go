// Processor observations belong to one retained attempt. A delayed response
// cannot mutate a later retry, and resetting markers must retain their history
// in the same database transaction as the reset.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/urnetwork/server"
)

var ErrProviderPaymentAttemptChanged = errors.New("provider payment attempt changed; current obligation retained for reconciliation")
var ErrProviderPaymentOutcomeContradictory = errors.New("provider processor outcome contradicts retained chain evidence")

const providerPaymentReceiptLimit = 1024 * 1024

// The retained processor request is the authority for an ambiguous retry,
// including after its original wallet has been deactivated.
type ProviderPaymentRequest struct {
	Basis   ProviderPaymentBasis `json:"basis"`
	Amount  float64              `json:"amount_usdc"`
	Network string               `json:"processor_network"`
}

// RetainProviderPaymentRequest records the exact first request before calling
// Circle. The stable key also identifies its immutable audit row; a retry must
// match that row byte-for-byte. No external operation runs in this transaction.
func RetainProviderPaymentRequest(ctx context.Context, basis *ProviderPaymentBasis, amount float64, network string) (returnErr error) {
	if basis == nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 || network == "" {
		return ErrProviderPaymentBasisChanged
	}
	details, err := json.Marshal(ProviderPaymentRequest{Basis: *basis, Amount: amount, Network: network})
	if err != nil {
		return err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = retainProviderPaymentRequestInTx(ctx, tx, basis, details)
	}, server.TxReadCommitted)
	return
}

// A first request requires a currently active wallet. An already retained
// request can only compare equal; retirement cannot manufacture new authority.
func retainProviderPaymentRequestInTx(ctx context.Context, tx server.PgTx, basis *ProviderPaymentBasis, details []byte) error {
	if len(details) > providerPaymentReceiptLimit || !lockProviderPaymentBasis(ctx, tx, basis, true) {
		return ErrProviderPaymentAttemptChanged
	}
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details)
		SELECT $1,$2,'circle_attempt_request',$3 WHERE EXISTS(SELECT 1 FROM account_wallet WHERE wallet_id=$4 AND active)
		ON CONFLICT(event_id) DO NOTHING`, basis.IdempotencyKey, basis.PaymentId, string(details), basis.WalletId))
	var matches bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_account_payment WHERE event_id=$1
		AND payment_id=$2 AND event_type='circle_attempt_request' AND event_details=$3)`, basis.IdempotencyKey, basis.PaymentId, string(details)).Scan(&matches))
	if !matches {
		return ErrProviderPaymentAttemptChanged
	}
	return nil
}

func lockProviderPaymentBasis(ctx context.Context, tx server.PgTx, basis *ProviderPaymentBasis, requireNoRecord bool) bool {
	result, err := tx.Query(ctx, `SELECT p.payment_id FROM account_payment p JOIN account_wallet w ON w.wallet_id=p.wallet_id
		WHERE p.payment_id=$1 AND p.circle_idempotency_key=$2 AND p.network_id=$3 AND p.wallet_id=$4 AND p.payout_nano_cents=$5
		AND w.wallet_address=$6 AND w.blockchain=$7 AND w.network_id=p.network_id
		AND NOT p.completed AND NOT p.canceled AND p.tx_hash IS NULL AND (NOT $8 OR p.payment_record IS NULL)
		FOR UPDATE OF p`, basis.PaymentId, basis.IdempotencyKey, basis.NetworkId, basis.WalletId, basis.Payout, basis.WalletAddress, basis.Blockchain, requireNoRecord)
	matched := false
	server.WithPgResult(result, err, func() { matched = result.Next() })
	return matched
}

func lockProviderPaymentAttempt(ctx context.Context, tx server.PgTx, expected *AccountPayment) bool {
	if expected == nil {
		return false
	}
	result, err := tx.Query(ctx, `SELECT payment_id FROM account_payment WHERE payment_id=$1 AND NOT completed AND NOT canceled
		AND circle_idempotency_key IS NOT DISTINCT FROM $2::uuid AND payment_record IS NOT DISTINCT FROM $3::text
		AND tx_hash IS NOT DISTINCT FROM $4::text AND payout_nano_cents=$5 AND wallet_id IS NOT DISTINCT FROM $6::uuid
		AND token_amount IS NOT DISTINCT FROM $7::double precision AND network_id=$8 FOR UPDATE`,
		expected.PaymentId, expected.CircleIdempotencyKey, expected.PaymentRecord, expected.TxHash, expected.Payout, expected.WalletId, expected.TokenAmount, expected.NetworkId)
	matched := false
	server.WithPgResult(result, err, func() { matched = result.Next() })
	return matched
}

func retainProviderAttemptInTx(ctx context.Context, tx server.PgTx, expected *AccountPayment, status, receipt string) {
	// AccountPayment intentionally hides the idempotency key from public APIs;
	// retain it explicitly here, alongside the old response and actual amount.
	details, err := json.Marshal(struct {
		Attempt        *AccountPayment `json:"attempt"`
		IdempotencyKey *server.Id      `json:"idempotency_key"`
		Status         string          `json:"status"`
		Response       string          `json:"response"`
	}{Attempt: expected, IdempotencyKey: expected.CircleIdempotencyKey, Status: status, Response: receipt})
	server.Raise(err)
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details)
		VALUES($1,$2,'circle_attempt_outcome',$3)`, server.NewId(), expected.PaymentId, string(details)))
}

// SetProviderPaymentRecord binds the accepted result to the exact submitted
// basis. A lost acknowledgement may safely repeat the same key and record; an
// old response cannot overwrite a different attempt or processor record.
func SetProviderPaymentRecord(ctx context.Context, basis *ProviderPaymentBasis, amount float64, record string) (returnErr error) {
	if basis == nil || record == "" || len(record) > 4096 || math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return ErrProviderPaymentAttemptChanged
	}
	server.Tx(ctx, func(tx server.PgTx) {
		if !lockProviderPaymentBasis(ctx, tx, basis, false) {
			// The accepted response still matters even after another worker
			// advanced the attempt. Retain it without overwriting current markers.
			details, err := json.Marshal(struct {
				Basis  *ProviderPaymentBasis `json:"basis"`
				Amount float64               `json:"amount_usdc"`
				Record string                `json:"record"`
			}{Basis: basis, Amount: amount, Record: record})
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_late_acceptance',$3)`, server.NewId(), basis.PaymentId, string(details)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET attribution_review_required=true WHERE payment_id=$1 AND
				(circle_idempotency_key IS DISTINCT FROM $2::uuid OR (payment_record IS NOT NULL AND payment_record<>$3))`, basis.PaymentId, basis.IdempotencyKey, record))
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		var requested bool
		server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_account_payment WHERE event_id=$1 AND payment_id=$2 AND event_type='circle_attempt_request'
			AND (event_details::jsonb->>'amount_usdc')::double precision=$3)`, basis.IdempotencyKey, basis.PaymentId, amount).Scan(&requested))
		if !requested {
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		current, err := dbGetPayment(ctx, tx, basis.PaymentId)
		server.Raise(err)
		if current == nil || (current.PaymentRecord != nil && (*current.PaymentRecord != record || current.TokenAmount == nil || *current.TokenAmount != amount)) {
			if current != nil {
				conflict, err := json.Marshal(struct {
					Record string  `json:"record"`
					Amount float64 `json:"amount_usdc"`
				}{Record: record, Amount: amount})
				server.Raise(err)
				retainProviderAttemptInTx(ctx, tx, current, "CONTRADICTORY_ACCEPTANCE", string(conflict))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET attribution_review_required=true WHERE payment_id=$1`, basis.PaymentId))
			}
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		if current.PaymentRecord != nil {
			return
		}
		response, err := json.Marshal(struct {
			Record string  `json:"record"`
			Amount float64 `json:"amount_usdc"`
		}{Record: record, Amount: amount})
		server.Raise(err)
		retainProviderAttemptInTx(ctx, tx, current, "ACCEPTED", string(response))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET token_type='USDC',token_amount=$2,payment_record=$3,payment_time=$4,payment_receipt=NULL WHERE payment_id=$1`, basis.PaymentId, amount, record, server.NowUtc()))
	}, server.TxReadCommitted)
	return
}

// ResetProviderPaymentSubmission is only for a definitive invalid-destination
// rejection. If another worker already retained an accepted record, the reset
// refuses without clearing that result or allocating a new key.
func ResetProviderPaymentSubmission(ctx context.Context, basis *ProviderPaymentBasis, receipt string) (returnErr error) {
	if basis == nil || len(receipt) > providerPaymentReceiptLimit {
		return ErrProviderPaymentAttemptChanged
	}
	server.Tx(ctx, func(tx server.PgTx) {
		if !lockProviderPaymentBasis(ctx, tx, basis, true) {
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		current, err := dbGetPayment(ctx, tx, basis.PaymentId)
		server.Raise(err)
		retainProviderAttemptInTx(ctx, tx, current, "INVALID_DESTINATION", receipt)
		server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET circle_idempotency_key=NULL,payment_record=NULL,token_type=NULL,token_amount=NULL,payment_time=NULL,payment_receipt=$2 WHERE payment_id=$1`, basis.PaymentId, receipt))
	}, server.TxReadCommitted)
	return
}

// ApplyProviderPaymentOutcome atomically journals and applies a response to
// exactly the requested attempt. Known failed component-bearing payments keep
// their original row, amounts, points and subsidy window for a later retry.
// A completed but unexplained historical gross is reported for review; only its
// observed processor transfer is complete, with no inferred paid-gross credit.
func ApplyProviderPaymentOutcome(ctx context.Context, expected *AccountPayment, status, receipt, txHash string, reviewRequired bool) (complete, canceled bool, returnErr error) {
	if expected == nil || expected.PaymentRecord == nil || len(receipt) > providerPaymentReceiptLimit || len(txHash) > 4096 {
		return false, false, ErrProviderPaymentAttemptChanged
	}
	status = strings.ToUpper(status)
	policy, policyErr := server.LoadProviderPayoutEarningPolicy(ctx)
	// A missing policy observation cannot authorize releasing an allocated
	// component. Completion/reconciliation still retains the original attempt.
	retainComponents := policy != nil || policyErr != nil
	apply := func(tx server.PgTx) {
		complete, canceled, returnErr = false, false, nil
		if !lockProviderPaymentAttempt(ctx, tx, expected) {
			retainProviderAttemptInTx(ctx, tx, expected, "STALE_"+status, receipt)
			if txHash != "" || status == "COMPLETE" {
				// A late chain observation from a different attempt cannot be
				// treated as a harmless failed retry. Stop new submissions only
				// for this payment; any already retained record still reconciles.
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET attribution_review_required=true WHERE payment_id=$1 AND
					(circle_idempotency_key IS DISTINCT FROM $2::uuid OR payment_record IS DISTINCT FROM $3::text)`, expected.PaymentId, expected.CircleIdempotencyKey, expected.PaymentRecord))
			}
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		retainProviderAttemptInTx(ctx, tx, expected, status, receipt)
		if expected.TxHash != nil && txHash != "" && *expected.TxHash != txHash {
			returnErr = ErrProviderPaymentOutcomeContradictory
			return
		}
		if txHash == "" && expected.TxHash != nil {
			txHash = *expected.TxHash
		}
		switch status {
		case "DENIED", "FAILED", "CANCELLED":
			if txHash != "" {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET payment_receipt=$2,tx_hash=$3 WHERE payment_id=$1`, expected.PaymentId, receipt, txHash))
				returnErr = ErrProviderPaymentOutcomeContradictory
				return
			}
			canceled = status == "CANCELLED" && !(retainComponents && (expected.SubsidyPayout > 0 || expected.ReliabilitySubsidy > 0))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET payment_record=NULL,circle_idempotency_key=NULL,token_type=NULL,token_amount=NULL,payment_time=NULL,payment_receipt=$2,
				canceled=$3,cancel_time=CASE WHEN $3 THEN $4 ELSE cancel_time END WHERE payment_id=$1`, expected.PaymentId, receipt, canceled, server.NowUtc()))
		case "SENT", "STUCK", "CONFIRMED":
			if txHash != "" {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET payment_receipt=$2,tx_hash=$3 WHERE payment_id=$1`, expected.PaymentId, receipt, txHash))
			}
		case "COMPLETE":
			if txHash == "" {
				returnErr = ErrProviderPaymentOutcomeContradictory
				return
			}
			if reviewRequired {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET attribution_review_required=true WHERE payment_id=$1`, expected.PaymentId))
			}
			server.Raise(completePaymentInTx(ctx, tx, expected.PaymentId, &expected.NetworkId, receipt, txHash))
			complete = true
		default:
			returnErr = fmt.Errorf("processor status %q is not an admitted outcome; attempt retained", status)
		}
	}
	if status == "COMPLETE" {
		if expected.NetworkId == (server.Id{}) {
			return false, false, ErrProviderPaymentAttemptChanged
		}
		// The locked attempt predicate includes this exact network identity.
		// Stale attempts still retain their audit event inside the same body.
		keys := append(accountBalanceOwnershipKeys([]server.Id{expected.NetworkId}), server.NewPgOwnershipKey("account_payment", expected.PaymentId))
		server.OwnedTx(ctx, keys, apply, server.TxReadCommitted)
	} else {
		server.Tx(ctx, apply, server.TxReadCommitted)
	}
	return
}
