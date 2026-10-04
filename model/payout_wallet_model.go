package model

import (
	"context"
	"fmt"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type SetPayoutWalletArgs struct {
	WalletId server.Id `json:"wallet_id"`
}

type SetPayoutWalletResult struct{}

// the wallet must be an active wallet owned by the network,
// so that a payout can never be directed to another network's wallet
func SetPayoutWallet(ctx context.Context, networkId server.Id, walletId server.Id) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		// bittensor wallets are recorded for future use only; payouts run
		// USDC on Solana/Polygon, so a TAO payout wallet would silently
		// break payouts
		var blockchain string
		blockchainResult, err := tx.Query(
			ctx,
			`
				SELECT blockchain
				FROM account_wallet
				WHERE
						wallet_id = $2 AND
						network_id = $1 AND
						active = true
			`,
			networkId,
			walletId,
		)
		server.WithPgResult(blockchainResult, err, func() {
			if blockchainResult.Next() {
				server.Raise(blockchainResult.Scan(&blockchain))
			}
		})
		if parsedBlockchain, err := ParseBlockchain(blockchain); err == nil && parsedBlockchain == TAO {
			returnErr = fmt.Errorf("Bittensor wallets cannot be the payout wallet.")
			return
		}

		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO payout_wallet (
						network_id,
						wallet_id
				)
				SELECT
						account_wallet.network_id,
						account_wallet.wallet_id
				FROM account_wallet
				WHERE
						account_wallet.wallet_id = $2 AND
						account_wallet.network_id = $1 AND
						account_wallet.active = true
				ON CONFLICT (network_id) DO UPDATE
				SET
						wallet_id = $2
			`,
			networkId,
			walletId,
		))
		if tag.RowsAffected() != 1 {
			returnErr = fmt.Errorf("Wallet must be an active wallet owned by the network.")
			return
		}
	})
	return
}

// returns the payout wallet only when it is an active wallet owned by the
// network, matching the wallet the payout planner will use. A stale or corrupt
// `payout_wallet` row (e.g. pointing at a removed wallet or another network's
// wallet) reads as no payout wallet, so connecting a wallet replaces it
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

func deletePayoutWallet(walletId server.Id, session *session.ClientSession) {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		deletePayoutWalletInTx(session.Ctx, tx, walletId, session.ByJwt.NetworkId)
	})
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
