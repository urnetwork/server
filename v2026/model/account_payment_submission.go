// The processor idempotency key freezes the exact amount and wallet read by
// this attempt. A stale caller must retry from storage without submitting.
package model

import (
	"context"
	"errors"

	"github.com/urnetwork/server/v2026"
)

var ErrProviderPaymentBasisChanged = errors.New("provider payment basis changed; retained for retry without submission")

// Public, immutable attempt basis; it contains no wallet secrets. The database
// financial guard and key-fenced wallet update preserve these values on retry.
type ProviderPaymentBasis struct {
	PaymentId      server.Id
	IdempotencyKey server.Id
	NetworkId      server.Id
	WalletId       server.Id
	Payout         NanoCents
	WalletAddress  string
	Blockchain     string
}

// Conditional reservation refuses a price or wallet update between the caller's
// read and key allocation. Nothing, including a new key, changes on refusal.
func ReserveProviderPaymentBasis(ctx context.Context, payment *AccountPayment, wallet *AccountWallet) (basis *ProviderPaymentBasis, returnErr error) {
	if payment == nil || wallet == nil || payment.WalletId == nil || *payment.WalletId != wallet.WalletId || payment.NetworkId != wallet.NetworkId {
		return nil, ErrProviderPaymentBasisChanged
	}
	if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err != nil {
		return nil, err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		basis = reserveProviderPaymentBasisInTx(ctx, tx, payment, wallet, true)
		if basis == nil {
			returnErr = ErrProviderPaymentBasisChanged
		}
	}, server.TxReadCommitted)
	return
}

// The fresh writer and the compatibility reservation share the same exact
// payment compare. Only the fresh writer requires an unreserved row.
func reserveProviderPaymentBasisInTx(ctx context.Context, tx server.PgTx, payment *AccountPayment, wallet *AccountWallet, allowReserved bool) (basis *ProviderPaymentBasis) {
	result, err := tx.Query(ctx, `UPDATE account_payment p SET circle_idempotency_key=COALESCE(circle_idempotency_key,$2)
			WHERE p.payment_id=$1 AND p.network_id=$3 AND p.wallet_id=$4 AND p.payout_nano_cents=$5
			AND NOT p.completed AND NOT p.canceled AND p.payment_record IS NULL AND p.tx_hash IS NULL
			AND ($8 OR p.circle_idempotency_key IS NULL)
			AND EXISTS (SELECT 1 FROM account_wallet w WHERE w.wallet_id=p.wallet_id AND w.network_id=p.network_id
			AND w.wallet_address=$6 AND w.blockchain=$7 AND w.active)
			RETURNING circle_idempotency_key`, payment.PaymentId, server.NewId(), payment.NetworkId, wallet.WalletId, payment.Payout, wallet.WalletAddress, wallet.Blockchain, allowReserved)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var key server.Id
			server.Raise(result.Scan(&key))
			basis = &ProviderPaymentBasis{PaymentId: payment.PaymentId, IdempotencyKey: key, NetworkId: payment.NetworkId, WalletId: wallet.WalletId, Payout: payment.Payout, WalletAddress: wallet.WalletAddress, Blockchain: wallet.Blockchain}
		}
	})
	return
}

// Recheck the key and exact basis after limiter admission. A different worker
// may already have recorded a result; the next retry then reconciles that record.
func RequireProviderPaymentBasis(ctx context.Context, basis *ProviderPaymentBasis) error {
	if basis == nil {
		return ErrProviderPaymentBasisChanged
	}
	if err := RequireProviderUsdcPayment(ctx, basis.PaymentId); err != nil {
		return err
	}
	var matches bool
	var queryErr error
	server.Db(ctx, func(conn server.PgConn) {
		queryErr = conn.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM account_payment p JOIN account_wallet w ON w.wallet_id=p.wallet_id AND w.network_id=p.network_id
		WHERE p.payment_id=$1 AND p.circle_idempotency_key=$2 AND p.network_id=$3 AND p.wallet_id=$4 AND p.payout_nano_cents=$5
		AND w.wallet_address=$6 AND w.blockchain=$7 AND w.active
		AND NOT p.completed AND NOT p.canceled AND p.payment_record IS NULL AND p.tx_hash IS NULL)`,
			basis.PaymentId, basis.IdempotencyKey, basis.NetworkId, basis.WalletId, basis.Payout, basis.WalletAddress, basis.Blockchain).Scan(&matches)
	})
	if queryErr != nil {
		return queryErr
	}
	if !matches {
		return ErrProviderPaymentBasisChanged
	}
	return nil
}
