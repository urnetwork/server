package model

import (
	"context"

	"github.com/urnetwork/server"
)

type SetPayoutWalletArgs struct {
	WalletId server.Id `json:"wallet_id"`
}

type SetPayoutWalletResult struct{}

// returns the payout wallet only when it is an active wallet owned by the
// network. A stale or corrupt `payout_wallet` row (e.g. pointing at a removed
// wallet or another network's wallet) reads as no payout wallet. USDC provider
// payouts ended (owner decisions 2026-11-12 and 2026-11-13): nothing selects a
// payout wallet any more, and removing the selected wallet removes it.
func GetPayoutWalletId(ctx context.Context, networkId server.Id) *server.Id {
	var walletId *server.Id
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
						payout_wallet.wallet_id
				FROM payout_wallet
				INNER JOIN account_wallet ON
						account_wallet.wallet_id = payout_wallet.wallet_id AND
						account_wallet.network_id = payout_wallet.network_id AND
						account_wallet.active = true
				WHERE
						payout_wallet.network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&walletId))
			}
		})
	})
	return walletId
}

// Wallet deactivation owns the same transaction as its payout selection. An
// independent checkout can exhaust the pool and commit only half the removal.
func deletePayoutWalletInTx(ctx context.Context, tx server.PgTx, walletId, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
            DELETE FROM payout_wallet
            WHERE 
                wallet_id = $1 AND 
                network_id = $2
            `,
		walletId,
		networkId,
	))
}
