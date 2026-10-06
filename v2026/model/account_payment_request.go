// Original Circle requests survive wallet retirement. Fresh admission writes
// the key and its exact request together; missing originals remain unknown.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/urnetwork/server/v2026"
)

// Allocate only a fresh attempt and retain its request in the same commit.
// A racing reservation must be reloaded, never rebuilt from current settings.
func ReserveProviderPaymentRequest(ctx context.Context, payment *AccountPayment, wallet *AccountWallet, amount float64, network string) (request *ProviderPaymentRequest, returnErr error) {
	if payment == nil || wallet == nil || payment.WalletId == nil || *payment.WalletId != wallet.WalletId || payment.NetworkId != wallet.NetworkId ||
		payment.CircleIdempotencyKey != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 || network == "" {
		return nil, ErrProviderPaymentBasisChanged
	}
	if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err != nil {
		return nil, err
	}
	defer func() {
		if value := recover(); value != nil {
			if err, ok := value.(error); ok && errors.Is(err, ErrProviderPaymentAttemptChanged) {
				request, returnErr = nil, err
			} else {
				panic(value)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		request, returnErr = nil, nil
		basis := reserveProviderPaymentBasisInTx(ctx, tx, payment, wallet, false)
		if basis == nil {
			returnErr = ErrProviderPaymentBasisChanged
			return
		}
		candidate := &ProviderPaymentRequest{Basis: *basis, Amount: amount, Network: network}
		details, err := json.Marshal(candidate)
		server.Raise(err)
		// Refusal must roll back the key as well as the request.
		server.Raise(retainProviderPaymentRequestInTx(ctx, tx, basis, details))
		request = candidate
	}, server.TxReadCommitted)
	return
}

// Load the original body before consulting current fees or wallet selection.
// Canonical bytes, the caller's original markers and current stored ownership
// must agree; a key without a request never authorizes a guessed retry.
func GetRetainedProviderPaymentRequest(ctx context.Context, payment *AccountPayment) (*ProviderPaymentRequest, error) {
	if payment == nil || payment.CircleIdempotencyKey == nil || payment.WalletId == nil {
		return nil, ErrProviderPaymentAttemptChanged
	}
	var details *string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT CASE WHEN octet_length(event_details)<=$3 THEN event_details END
			FROM audit_account_payment WHERE event_id=$1 AND payment_id=$2 AND event_type='circle_attempt_request'`,
			*payment.CircleIdempotencyKey, payment.PaymentId, providerPaymentReceiptLimit)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&details))
			}
		})
	})
	if details == nil {
		return nil, ErrProviderPaymentAttemptChanged
	}
	request := &ProviderPaymentRequest{}
	if err := json.Unmarshal([]byte(*details), request); err != nil {
		return nil, ErrProviderPaymentAttemptChanged
	}
	canonical, err := json.Marshal(request)
	if err != nil || !bytes.Equal(canonical, []byte(*details)) || request.Basis.PaymentId != payment.PaymentId ||
		request.Basis.IdempotencyKey != *payment.CircleIdempotencyKey || request.Basis.NetworkId != payment.NetworkId ||
		request.Basis.WalletId != *payment.WalletId || request.Basis.Payout != payment.Payout {
		return nil, ErrProviderPaymentAttemptChanged
	}
	if err := RequireProviderPaymentRequest(ctx, &request.Basis, request.Amount, request.Network); err != nil {
		return nil, err
	}
	return request, nil
}

// The final processor boundary rechecks the exact retained body and earning
// authority. Wallet ownership/address stay fixed, while active may change.
func RequireProviderPaymentRequest(ctx context.Context, basis *ProviderPaymentBasis, amount float64, network string) error {
	if basis == nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 || network == "" {
		return ErrProviderPaymentBasisChanged
	}
	if err := RequireProviderUsdcPayment(ctx, basis.PaymentId); err != nil {
		return err
	}
	details, err := json.Marshal(ProviderPaymentRequest{Basis: *basis, Amount: amount, Network: network})
	if err != nil || len(details) > providerPaymentReceiptLimit {
		return ErrProviderPaymentBasisChanged
	}
	var matches bool
	var queryErr error
	server.Db(ctx, func(conn server.PgConn) {
		queryErr = conn.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM account_payment p JOIN account_wallet w ON w.wallet_id=p.wallet_id AND w.network_id=p.network_id
		JOIN audit_account_payment a ON a.event_id=p.circle_idempotency_key AND a.payment_id=p.payment_id
		WHERE p.payment_id=$1 AND p.circle_idempotency_key=$2 AND p.network_id=$3 AND p.wallet_id=$4 AND p.payout_nano_cents=$5
		AND w.wallet_address=$6 AND w.blockchain=$7 AND a.event_type='circle_attempt_request' AND a.event_details=$8
		AND NOT p.completed AND NOT p.canceled AND p.payment_record IS NULL AND p.tx_hash IS NULL)`,
			basis.PaymentId, basis.IdempotencyKey, basis.NetworkId, basis.WalletId, basis.Payout, basis.WalletAddress, basis.Blockchain, string(details)).Scan(&matches)
	})
	if queryErr != nil {
		return queryErr
	}
	if !matches {
		return ErrProviderPaymentBasisChanged
	}
	return nil
}
