package model

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The account_wallet.default_token_type column is varchar(16)
// (db_migrations.go), and varchar counts characters, not bytes.
const MaxWalletDefaultTokenTypeLength = 16

type WalletType = string

const (
	WalletTypeCircleUserControlled WalletType = "circle_uc"
	WalletTypeExternal             WalletType = "external"
	// WalletTypeXch                  WalletType = "xch"
	// WalletTypeSol                  WalletType = "sol"
	// WalletTypeMatic                WalletType = "matic"
)

type AccountWallet struct {
	WalletId         server.Id  `json:"wallet_id"`
	CircleWalletId   *string    `json:"circle_wallet_id,omitempty"`
	NetworkId        server.Id  `json:"network_id"`
	WalletType       WalletType `json:"wallet_type"`
	Blockchain       string     `json:"blockchain"`
	WalletAddress    string     `json:"wallet_address"`
	Active           bool       `json:"active"`
	DefaultTokenType string     `json:"default_token_type"`
	CreateTime       time.Time  `json:"create_time"`
	HasSeekerToken   bool       `json:"has_seeker_token"`
}

type CreateAccountWalletExternalArgs struct {
	NetworkId        server.Id `json:"network_id"`
	Blockchain       string    `json:"blockchain"`
	WalletAddress    string    `json:"wallet_address"`
	DefaultTokenType string    `json:"default_token_type"`
}

type CreateAccountWalletCircleArgs struct {
	NetworkId        server.Id
	Blockchain       string
	WalletAddress    string
	DefaultTokenType string
	CircleWalletId   string
}

type CreateAccountWalletResult struct {
	WalletId server.Id `json:"wallet_id"`
}

func CreateAccountWalletExternal(
	session *session.ClientSession,
	createAccountWallet *CreateAccountWalletExternalArgs,
) *server.Id {
	var walletId *server.Id

	server.Tx(session.Ctx, func(tx server.PgTx) {
		id := server.NewId()
		active := true
		createTime := server.NowUtc()

		// First try to find an existing wallet with the same network_id and wallet_address
		var existingWalletId server.Id
		existingRow := tx.QueryRow(
			session.Ctx,
			`
					SELECT wallet_id
					FROM account_wallet
					WHERE network_id = $1 AND wallet_address = $2
			`,
			createAccountWallet.NetworkId,
			createAccountWallet.WalletAddress,
		)

		err := existingRow.Scan(&existingWalletId)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			// a failed statement ends the transaction at once. It used to be
			// taken for "no wallet", and the insert below then failed too,
			// after which the transaction went on to a commit that server.Tx
			// retried for a minute.
			server.Raise(err)
		}
		if err == nil {

			glog.Infof("[wm][%s]found existing wallet %s for address %s", createAccountWallet.NetworkId, existingWalletId, createAccountWallet.WalletAddress)

			// Found an existing wallet - update active = true
			_ = server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
									UPDATE account_wallet
									SET active = true
									WHERE wallet_id = $1 AND NOT active
							`,
				existingWalletId,
			))

			walletId = &existingWalletId
			return
		}

		// No existing wallet found, create a new one. A failed insert raises,
		// which ends the transaction at once.
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				INSERT INTO account_wallet (
						wallet_id,
						network_id,
						wallet_type,
						blockchain,
						wallet_address,
						active,
						default_token_type,
						create_time
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`,
			id,
			createAccountWallet.NetworkId,
			WalletTypeExternal,
			createAccountWallet.Blockchain,
			createAccountWallet.WalletAddress,
			active,
			createAccountWallet.DefaultTokenType,
			createTime,
		))

		walletId = &id
	})

	return walletId
}

func CreateAccountWalletCircle(
	ctx context.Context,
	createAccountWallet *CreateAccountWalletCircleArgs,
) *server.Id {

	var walletId *server.Id

	server.Tx(ctx, func(tx server.PgTx) {

		id := server.NewId()
		active := true
		createTime := server.NowUtc()

		// a failed insert raises, which ends the transaction at once
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO account_wallet (
						wallet_id,
						network_id,
						wallet_type,
						blockchain,
						wallet_address,
						active,
						default_token_type,
						create_time,
						circle_wallet_id
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			`,
			id,
			createAccountWallet.NetworkId,
			WalletTypeCircleUserControlled,
			createAccountWallet.Blockchain,
			createAccountWallet.WalletAddress,
			active,
			createAccountWallet.DefaultTokenType,
			createTime,
			createAccountWallet.CircleWalletId,
		))

		walletId = &id
	})

	return walletId
}

func GetAccountWallet(ctx context.Context, walletId server.Id) *AccountWallet {
	var wallet *AccountWallet
	server.Db(ctx, func(conn server.PgConn) {
		wallet = dbGetAccountWallet(ctx, conn, walletId)
	})
	return wallet
}

func dbGetAccountWallet(ctx context.Context, conn server.PgConn, walletId server.Id) *AccountWallet {
	var wallet *AccountWallet
	result, err := conn.Query(
		ctx,
		`
			SELECT
					wallet_id,
					network_id,
					wallet_type,
					blockchain,
					wallet_address,
					active,
					default_token_type,
					create_time,
					circle_wallet_id,
					has_seeker_token
			FROM account_wallet
			WHERE
					wallet_id = $1
		`,
		walletId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			wallet = &AccountWallet{}
			scanAccountWallet(result, wallet)
		}
	})
	return wallet
}

func GetAccountWalletByCircleId(ctx context.Context, circleWalletId string) *AccountWallet {

	var wallet *AccountWallet
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
					wallet_id,
					network_id,
					wallet_type,
					blockchain,
					wallet_address,
					active,
					default_token_type,
					create_time,
					circle_wallet_id,
					has_seeker_token
			FROM account_wallet
			WHERE
					circle_wallet_id = $1
		`,
			circleWalletId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				wallet = &AccountWallet{}
				scanAccountWallet(result, wallet)
			}
		})
	})
	return wallet
}

func scanAccountWallet(result pgx.Rows, wallet *AccountWallet) {
	server.Raise(result.Scan(
		&wallet.WalletId,
		&wallet.NetworkId,
		&wallet.WalletType,
		&wallet.Blockchain,
		&wallet.WalletAddress,
		&wallet.Active,
		&wallet.DefaultTokenType,
		&wallet.CreateTime,
		&wallet.CircleWalletId,
		&wallet.HasSeekerToken,
	))
}

// this is unused
func FindActiveAccountWallets(
	ctx context.Context,
	networkId server.Id,
	walletType WalletType,
	walletAddress string,
) []*AccountWallet {
	wallets := []*AccountWallet{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    wallet_id
                FROM account_wallet
                WHERE
                    active = true AND
                    network_id = $1 AND
                    wallet_type = $2 AND
                    wallet_address = $3
            `,
			networkId,
			walletType,
			walletAddress,
		)
		walletIds := []server.Id{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var walletId server.Id
				server.Raise(result.Scan(&walletId))
				walletIds = append(walletIds, walletId)
			}
		})

		for _, walletId := range walletIds {
			wallet := dbGetAccountWallet(ctx, conn, walletId)
			if wallet != nil && wallet.Active {
				wallets = append(wallets, wallet)
			}
		}
	})

	return wallets
}

type GetAccountWalletsResult struct {
	Wallets []*AccountWallet `json:"wallets"`
}

func GetActiveAccountWallets(session *session.ClientSession) *GetAccountWalletsResult {
	wallets := []*AccountWallet{}

	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
						wallet_id,
						network_id,
						wallet_type,
						blockchain,
						wallet_address,
						active,
						default_token_type,
						create_time,
						circle_wallet_id,
						has_seeker_token
					FROM account_wallet
					WHERE
							active = true AND
							network_id = $1
			`,
			session.ByJwt.NetworkId,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {

				var wallet = &AccountWallet{}

				server.Raise(
					result.Scan(
						&wallet.WalletId,
						&wallet.NetworkId,
						&wallet.WalletType,
						&wallet.Blockchain,
						&wallet.WalletAddress,
						&wallet.Active,
						&wallet.DefaultTokenType,
						&wallet.CreateTime,
						&wallet.CircleWalletId,
						&wallet.HasSeekerToken,
					),
				)

				wallets = append(wallets, wallet)
			}
		})
	})

	return &GetAccountWalletsResult{
		Wallets: wallets,
	}
}

type RemoveWalletError struct {
	Message string `json:"message"`
}

type RemoveWalletResult struct {
	Success bool `json:"success"`
	// set when the removed wallet was the payout wallet and another of the
	// network's wallets took over. Added later; older clients ignore it.
	PayoutWalletId *server.Id         `json:"payout_wallet_id,omitempty"`
	Error          *RemoveWalletError `json:"error,omitempty"`
}

type RemoveWalletArgs struct {
	WalletId string `json:"wallet_id"`
}

func RemoveWallet(id server.Id, session *session.ClientSession) *RemoveWalletResult {

	var result = &RemoveWalletResult{
		Success: false,
	}

	// runs after id, the payout wallet, was removed in tx. Payments planned
	// while a network has no payout wallet are held until the user picks one,
	// so another active wallet the network owns takes over instead. Returns the
	// promoted wallet, or nil when no active Solana or Polygon wallet is left.
	promotePayoutWalletInTx := func(tx server.PgTx) *server.Id {
		var removedBlockchain string
		removedResult, err := tx.Query(
			session.Ctx,
			`
				SELECT blockchain
				FROM account_wallet
				WHERE
						wallet_id = $2 AND
						network_id = $1
			`,
			session.ByJwt.NetworkId,
			id,
		)
		server.WithPgResult(removedResult, err, func() {
			if removedResult.Next() {
				server.Raise(removedResult.Scan(&removedBlockchain))
			}
		})

		candidates := []*payoutWalletCandidate{}
		candidateResult, err := tx.Query(
			session.Ctx,
			`
				SELECT
						wallet_id,
						blockchain
				FROM account_wallet
				WHERE
						network_id = $1 AND
						active = true AND
						wallet_id <> $2
				ORDER BY create_time DESC, wallet_id
			`,
			session.ByJwt.NetworkId,
			id,
		)
		server.WithPgResult(candidateResult, err, func() {
			for candidateResult.Next() {
				candidate := &payoutWalletCandidate{}
				server.Raise(candidateResult.Scan(&candidate.walletId, &candidate.blockchain))
				candidates = append(candidates, candidate)
			}
		})

		promotedWalletId := choosePromotedPayoutWallet(removedBlockchain, candidates)
		if promotedWalletId == nil {
			return nil
		}
		if err := setPayoutWalletInTx(session.Ctx, tx, session.ByJwt.NetworkId, *promotedWalletId); err != nil {
			return nil
		}
		return promotedWalletId
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {
		result.Success = false
		result.PayoutWalletId = nil
		tag := server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE account_wallet
				SET
						active = $1
				WHERE
						wallet_id = $2 AND
						network_id = $3
			`,
			false,
			id,
			session.ByJwt.NetworkId,
		))

		if tag.RowsAffected() == 1 {
			if deletePayoutWalletInTx(session.Ctx, tx, id, session.ByJwt.NetworkId) {
				result.PayoutWalletId = promotePayoutWalletInTx(tx)
			}
			result.Success = true
		}

	})

	return result

}

/**
 * If the wallet holds a Seeker NFT, we increase points earned
 *
 * A failed write raises, as every model write does, so the transaction rolls
 * back with its cause; the error result is always nil.
 */
func MarkWalletSeekerHolder(walletAddress string, session *session.ClientSession) error {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE account_wallet
				SET
					has_seeker_token = true
				WHERE
					wallet_address = $1 AND network_id = $2
			`,
			walletAddress,
			session.ByJwt.NetworkId,
		))

		if tag.RowsAffected() == 0 {
			/**
			 * No rows updated, create a new wallet
			 */
			id := server.NewId()
			active := true
			createTime := server.NowUtc()

			server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
					 INSERT INTO account_wallet (
							 wallet_id,
							 network_id,
							 wallet_type,
							 blockchain,
							 wallet_address,
							 active,
							 default_token_type,
							 create_time,
							 has_seeker_token
					 )
					 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				 `,
				id,
				session.ByJwt.NetworkId,
				WalletTypeExternal,
				SOL.String(),
				walletAddress,
				active,
				"USDC",
				createTime,
				true,
			))

		}
	})

	return nil
}

func GetAllSeekerHolders(ctx context.Context) map[server.Id]bool {
	seekerHolders := map[server.Id]bool{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_id
				FROM account_wallet
				WHERE has_seeker_token = true
			`,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				if !seekerHolders[networkId] {
					seekerHolders[networkId] = true
				}
			}
		})
	})

	return seekerHolders
}
