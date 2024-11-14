package model

import (
	"context"

	"github.com/urnetwork/server/bringyour"
)

type NetworkReferralCode struct {
	NetworkId    bringyour.Id
	ReferralCode bringyour.Id
}

func CreateNetworkReferralCode(ctx context.Context, networkId bringyour.Id) *NetworkReferralCode {

	var networkReferralCode *NetworkReferralCode

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {

		networkReferralCode = &NetworkReferralCode{
			NetworkId:    networkId,
			ReferralCode: bringyour.NewId(),
		}

		bringyour.RaisePgResult(tx.Exec(
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

func GetNetworkReferralCode(ctx context.Context, networkId bringyour.Id) *NetworkReferralCode {

	var networkReferralCode *NetworkReferralCode

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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

		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				networkReferralCode = &NetworkReferralCode{}
				bringyour.Raise(result.Scan(
					&networkReferralCode.NetworkId,
					&networkReferralCode.ReferralCode,
				))
			}
		})
	})

	return networkReferralCode

}
