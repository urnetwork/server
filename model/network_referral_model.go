package model

import (
	"context"

	"github.com/urnetwork/server"
)

type NetworkReferral struct {
	NetworkId         *server.Id `json:"network_id"`
	ReferralNetworkId *server.Id `json:"referral_network_id"`
}

func CreateNetworkReferral(
	ctx context.Context,
	networkId server.Id,
	referralCode *server.Id,
) *NetworkReferral {

	// find network id by associated referral code
	referralNetworkId := GetNetworkIdByReferralCode(referralCode)

	// create network referral
	networkReferral := &NetworkReferral{
		NetworkId:         &networkId,
		ReferralNetworkId: &referralNetworkId,
	}

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_referral (
					network_id,
					referral_network_id
				)
				VALUES ($1, $2)
			`,
			networkReferral.NetworkId,
			networkReferral.ReferralNetworkId,
		))
	})
	return networkReferral

}

func GetNetworkReferralByNetworkId(
	ctx context.Context,
	networkId server.Id,
) *NetworkReferral {

	var networkReferral *NetworkReferral

	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
				SELECT
					network_id,
					referral_network_id
				FROM network_referral
				WHERE
					network_id = $1
			`,
			networkId,
		)

		server.WithPgResult(result, err, func() {
			if result.Next() {
				networkReferral = &NetworkReferral{}
				server.Raise(result.Scan(
					&networkReferral.NetworkId,
					&networkReferral.ReferralNetworkId,
				))
			}
		})

	})

	return networkReferral

}

func GetReferralsByReferralNetworkId(
	ctx context.Context,
	referralNetworkId server.Id,
) []*NetworkReferral {

	var networkReferrals []*NetworkReferral

	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
				SELECT
					network_id,
					referral_network_id
				FROM network_referral
				WHERE
					referral_network_id = $1
			`,
			referralNetworkId,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				networkReferral := &NetworkReferral{}
				server.Raise(result.Scan(
					&networkReferral.NetworkId,
					&networkReferral.ReferralNetworkId,
				))
				networkReferrals = append(networkReferrals, networkReferral)
			}
		})

	})

	return networkReferrals

}
