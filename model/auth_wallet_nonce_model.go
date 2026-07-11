package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
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

// RemoveExpiredWalletNonces deletes used or expired nonces. Safe to run
// periodically as a cleanup task.
func RemoveExpiredWalletNonces(ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                DELETE FROM auth_wallet_nonce
                WHERE used = true OR expire_time <= $1
            `,
			server.NowUtc(),
		))
	})
}
