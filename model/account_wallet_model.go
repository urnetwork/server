package model

import (
	"context"
	"time"

	"github.com/golang/glog"
	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

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

		// No existing wallet found, create a new one
		_, err = tx.Exec(
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
		)

		if err != nil {
			return
		}

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

		_, err := tx.Exec(
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
		)

		if err != nil {
			return
		}

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
	Success bool               `json:"success"`
	Error   *RemoveWalletError `json:"error,omitempty"`
}

type RemoveWalletArgs struct {
	WalletId string `json:"wallet_id"`
}

func RemoveWallet(id server.Id, session *session.ClientSession) *RemoveWalletResult {

	var result = &RemoveWalletResult{
		Success: false,
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {
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
			result = &RemoveWalletResult{
				Success: true,
			}

			deletePayoutWallet(id, session)

		}

	})

	return result

}

/**
 * If the wallet holds a Seeker NFT, we increase points earned
 */
func MarkWalletSeekerHolder(walletAddress string, session *session.ClientSession) (err error) {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
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
		)
		if err != nil {
			glog.Errorf("Error marking wallet as seeker holder: %v", err)
			return
		}

		if tag.RowsAffected() == 0 {
			/**
			 * No rows updated, create a new wallet
			 */
			id := server.NewId()
			active := true
			createTime := server.NowUtc()

			_, err = tx.Exec(
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
			)

		}
	})

	return err
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
