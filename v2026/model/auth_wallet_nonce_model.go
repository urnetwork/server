package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

const WalletNonceExpireTimeout = 5 * time.Minute

type AuthWalletNonceResult struct {
	Nonce string `json:"nonce"`
}

// AuthWalletNonceCreate issues a fresh single-use, short-lived nonce for wallet
// login. The client must include the returned nonce in the message it signs; the
// server validates and consumes it in handleLoginWallet so that a captured
// (message, signature) pair cannot be replayed.
func AuthWalletNonceCreate(clientSession *session.ClientSession) (*AuthWalletNonceResult, error) {
	nonce, err := newCodeBase32()
	if err != nil {
		return nil, err
	}

	createTime := server.NowUtc()
	expireTime := createTime.Add(WalletNonceExpireTimeout)

	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                INSERT INTO auth_wallet_nonce (nonce, create_time, expire_time)
                VALUES ($1, $2, $3)
            `,
			nonce,
			createTime,
			expireTime,
		))
	})

	return &AuthWalletNonceResult{Nonce: nonce}, nil
}

// consumeWalletAuthNonce atomically validates and consumes a wallet-login nonce.
// Returns true only if the nonce existed, was unexpired, and had not already been
// used. The single-use UPDATE (guarded on used = false) means the same nonce cannot
// be consumed twice even under concurrent requests, which is what prevents replay.
func consumeWalletAuthNonce(ctx context.Context, tx server.PgTx, nonce string) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
            UPDATE auth_wallet_nonce
            SET used = true
            WHERE
                nonce = $1 AND
                used = false AND
                $2 < expire_time
        `,
		nonce,
		server.NowUtc(),
	))
	return tag.RowsAffected() == 1
}

// RemoveExpiredWalletNonces deletes expired nonces in bounded batches, driven by
// the auth_wallet_nonce_expire_time index. Used-but-unexpired nonces are left to
// age out at their (short) expiry, so the delete is one indexable predicate
// rather than the old unindexable `used = true OR expire_time <= $1`. Wired to
// the RemoveExpiredWalletNonces task -- previously unwired, so this live-written
// table grew without bound.
func RemoveExpiredWalletNonces(ctx context.Context, maxTime time.Time, limit int) (removedCount int64) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
                DELETE FROM auth_wallet_nonce
                USING (
                    SELECT nonce FROM auth_wallet_nonce WHERE expire_time <= $1 ORDER BY expire_time LIMIT $2
                ) t
                WHERE auth_wallet_nonce.nonce = t.nonce
            `,
			maxTime,
			limit,
		)
		server.Raise(err)
		removedCount = tag.RowsAffected()
	})
	return
}
