package model

import (
	"context"
	"crypto/rand"

	"github.com/urnetwork/server"
)

type NetworkReferralCode struct {
	NetworkId    server.Id
	ReferralCode string
}

func generateAlphanumericCode(length int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	code := make([]byte, length)

	randomBytes := make([]byte, length)
	if _, err := rand.Read(randomBytes); err != nil {
		panic(err)
	}

	for i := range randomBytes {
		code[i] = charset[randomBytes[i]%byte(len(charset))]
	}

	return string(code)
}

func CreateNetworkReferralCode(ctx context.Context, networkId server.Id) *NetworkReferralCode {

	var networkReferralCode *NetworkReferralCode

	// code := generateAlphanumericCode(6)
	code := server.NewId().String()

	server.Tx(ctx, func(tx server.PgTx) {

		networkReferralCode = &NetworkReferralCode{
			NetworkId:    networkId,
			ReferralCode: code,
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
						INSERT INTO network_referral_code (
								network_id,
								referral_code
						)
						VALUES ($1, $2)
				`,
			networkReferralCode.NetworkId,
			networkReferralCode.ReferralCode,
		))
	})

	return networkReferralCode

}

func GetNetworkReferralCode(ctx context.Context, networkId server.Id) *NetworkReferralCode {

	var networkReferralCode *NetworkReferralCode

	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
						SELECT
								network_id,
								referral_code
						FROM network_referral_code
						WHERE
								network_id = $1
				`,
			networkId,
		)

		server.WithPgResult(result, err, func() {
			if result.Next() {
				networkReferralCode = &NetworkReferralCode{}
				server.Raise(result.Scan(
					&networkReferralCode.NetworkId,
					&networkReferralCode.ReferralCode,
				))
			}
		})
	})

	return networkReferralCode

}

func GetNetworkIdByReferralCode(referralCode string) *server.Id {

	var networkId *server.Id = nil

	server.Tx(context.Background(), func(tx server.PgTx) {
		result, err := tx.Query(
			context.Background(),
			`
						SELECT
								network_id
						FROM network_referral_code
						WHERE
								referral_code = $1
				`,
			referralCode,
		)

		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
			}
		})
	})

	return networkId

}

func ValidateReferralCode(ctx context.Context, referralCode string) bool {

	var exists bool

	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
						SELECT
								1
						FROM network_referral_code
						WHERE
								referral_code = $1
				`,
			referralCode,
		)

		server.WithPgResult(result, err, func() {
			exists = result.Next()
		})
	})

	return exists

}
