package model

import (
	"context"

	"bringyour.com/bringyour"
)

type SetPayoutWalletArgs struct {
	WalletId bringyour.Id `json:"wallet_id"`
}

type SetPayoutWalletResult struct{}

func SetPayoutWallet(ctx context.Context, networkId bringyour.Id, walletId bringyour.Id) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
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

func GetPayoutWalletId(ctx context.Context, networkId bringyour.Id) *bringyour.Id {
	var walletId *bringyour.Id
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&walletId))
			}
		})
	})
	return walletId
}
