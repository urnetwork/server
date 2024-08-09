package model

import (
	"context"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	"github.com/jackc/pgx/v5"
)

type WalletType = string

const (
	WalletTypeCircleUserControlled = "circle_uc"
	WalletTypeXch                  = "xch"
	WalletTypeSol                  = "sol"
)

type AccountWallet struct {
	WalletId         bringyour.Id
	CircleWalletId   *string
	NetworkId        bringyour.Id
	WalletType       WalletType
	Blockchain       string
	WalletAddress    string
	Active           bool
	DefaultTokenType string
	CreateTime       time.Time
}

type CreateAccountWalletResult struct {
	WalletId bringyour.Id `json:"wallet_id"`
}

func CreateAccountWallet(
	ctx context.Context,
	wallet *AccountWallet,
) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		wallet.WalletId = bringyour.NewId()
		wallet.Active = true
		wallet.CreateTime = bringyour.NowUtc()

		bringyour.RaisePgResult(tx.Exec(
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
			wallet.WalletId,
			wallet.NetworkId,
			wallet.WalletType,
			wallet.Blockchain,
			wallet.WalletAddress,
			wallet.Active,
			wallet.DefaultTokenType,
			wallet.CreateTime,
			wallet.CircleWalletId,
		))
	})
}

func GetAccountWallet(ctx context.Context, walletId bringyour.Id) *AccountWallet {
	var wallet *AccountWallet
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		wallet = dbGetAccountWallet(ctx, conn, walletId)
	})
	return wallet
}

func dbGetAccountWallet(ctx context.Context, conn bringyour.PgConn, walletId bringyour.Id) *AccountWallet {
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
					circle_wallet_id
			FROM account_wallet
			WHERE
					wallet_id = $1
		`,
		walletId,
	)
	bringyour.WithPgResult(result, err, func() {
		if result.Next() {
			wallet = &AccountWallet{}
			scanAccountWallet(result, wallet)
		}
	})
	return wallet
}

func GetAccountWalletByCircleId(ctx context.Context, circleWalletId string) *AccountWallet {

	var wallet *AccountWallet
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
					circle_wallet_id
			FROM account_wallet
			WHERE
					circle_wallet_id = $1
		`,
			circleWalletId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				wallet = &AccountWallet{}
				scanAccountWallet(result, wallet)
			}
		})
	})
	return wallet
}

func scanAccountWallet(result pgx.Rows, wallet *AccountWallet) {
	bringyour.Raise(result.Scan(
		&wallet.WalletId,
		&wallet.NetworkId,
		&wallet.WalletType,
		&wallet.Blockchain,
		&wallet.WalletAddress,
		&wallet.Active,
		&wallet.DefaultTokenType,
		&wallet.CreateTime,
		&wallet.CircleWalletId,
	))
}

// this is unused
func FindActiveAccountWallets(
	ctx context.Context,
	networkId bringyour.Id,
	walletType WalletType,
	walletAddress string,
) []*AccountWallet {
	wallets := []*AccountWallet{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
		walletIds := []bringyour.Id{}
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var walletId bringyour.Id
				bringyour.Raise(result.Scan(&walletId))
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
	Wallets []*AccountWallet
}

// this is unused
func GetActiveAccountWallets(ctx context.Context, session *session.ClientSession) *GetAccountWalletsResult {
	wallets := []*AccountWallet{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    wallet_id
                FROM account_wallet
                WHERE
                    active = true AND
                    network_id = $1
            `,
			session.ByJwt.NetworkId,
		)
		walletIds := []bringyour.Id{}
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var walletId bringyour.Id
				bringyour.Raise(result.Scan(&walletId))
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

	return &GetAccountWalletsResult{
		Wallets: wallets,
	}
}
