package model

import (
	"context"

	"github.com/urnetwork/server"
)

type NetworkReferralCode struct {
	NetworkId    server.Id
	ReferralCode server.Id
}

func CreateNetworkReferralCode(ctx context.Context, networkId server.Id) *NetworkReferralCode {

	var networkReferralCode *NetworkReferralCode

	server.Tx(ctx, func(tx server.PgTx) {

		networkReferralCode = &NetworkReferralCode{
			NetworkId:    networkId,
			ReferralCode: server.NewId(),
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

func GetNetworkIdByReferralCode(referralCode *server.Id) server.Id {

	var networkId server.Id

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
