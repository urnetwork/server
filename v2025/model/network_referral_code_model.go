package model

import (
	"context"
	"crypto/rand"
	"strings"

	"github.com/urnetwork/server/v2025"
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

func CreateNetworkReferralCodeInTx(ctx context.Context, tx server.PgTx, networkId server.Id) *NetworkReferralCode {

	code := generateAlphanumericCode(6)

	networkReferralCode := &NetworkReferralCode{
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

	return networkReferralCode

}

func CreateNetworkReferralCode(ctx context.Context, networkId server.Id) *NetworkReferralCode {

	var networkReferralCode *NetworkReferralCode

	server.Tx(ctx, func(tx server.PgTx) {

		networkReferralCode = CreateNetworkReferralCodeInTx(ctx, tx, networkId)

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

	referralCode = strings.ToUpper(referralCode)

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

type ValidateReferralCodeResult struct {
	Valid    bool `json:"valid"`
	IsCapped bool `json:"is_capped"`
}

func ValidateReferralCode(
	ctx context.Context,
	referralCode string,
) ValidateReferralCodeResult {

	maxReferrals := 5

	validateResult := ValidateReferralCodeResult{
		Valid:    false,
		IsCapped: false,
	}

	referralCode = strings.ToUpper(referralCode)
	var referralNetworkId *server.Id

	server.Tx(ctx, func(tx server.PgTx) {

		/**
		 * check existence
		 */
		result, err := tx.Query(
			ctx,
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
				server.Raise(result.Scan(&referralNetworkId))
				validateResult.Valid = true
			}

		})

		if validateResult.Valid {

			/**
			 * Check cap
			 */

			var count int
			result, err = tx.Query(
				ctx,
				`
					SELECT COUNT(*) FROM network_referral
					WHERE referral_network_id = $1
				`,
				referralNetworkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count))
				}

				if count >= maxReferrals {
					validateResult.IsCapped = true
				}

			})

		}

	})

	return validateResult

}
