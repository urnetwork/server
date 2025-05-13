package model

import (
	"context"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/session"
)

type SetPayoutWalletArgs struct {
	WalletId server.Id `json:"wallet_id"`
}

type SetPayoutWalletResult struct{}

func SetPayoutWallet(ctx context.Context, networkId server.Id, walletId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO payout_wallet (
						network_id,
						wallet_id
				)
				VALUES ($1, $2)
				ON CONFLICT (network_id) DO UPDATE
				SET
						wallet_id = $2
			`,
			networkId,
			walletId,
		))
	})
}

func GetPayoutWalletId(ctx context.Context, networkId server.Id) *server.Id {
	var walletId *server.Id
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
						wallet_id
				FROM payout_wallet
				WHERE
						network_id = $1
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

func deletePayoutWallet(walletId server.Id, session *session.ClientSession) {

	server.Tx(session.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
            DELETE FROM payout_wallet
            WHERE 
                wallet_id = $1 AND 
                network_id = $2
            `,
			walletId,
			session.ByJwt.NetworkId,
		))
	})

}
