package model

import (
	"context"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

type NetworkReferral struct {
	NetworkId         *server.Id `json:"network_id"`
	ReferralNetworkId *server.Id `json:"referral_network_id"`
	CreateTime        time.Time  `json:"create_time"`
}

func CreateNetworkReferral(
	ctx context.Context,
	networkId server.Id,
	referralCode string,
) *NetworkReferral {

	referralCode = strings.ToUpper(referralCode)

	// find network id by associated referral code
	referralNetworkId := GetNetworkIdByReferralCode(referralCode)

	if referralNetworkId == nil {
		return nil
	}

	if referralNetworkId == &networkId {
		return nil
	}

	// create network referral
	networkReferral := &NetworkReferral{
		NetworkId:         &networkId,
		ReferralNetworkId: referralNetworkId,
	}

	createTime := server.NowUtc()

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_referral (
					network_id,
					referral_network_id,
					create_time
				)
				VALUES ($1, $2, $3)
				ON CONFLICT (network_id)
				DO UPDATE SET
				    referral_network_id = EXCLUDED.referral_network_id,
				    create_time = EXCLUDED.create_time;
			`,
			networkReferral.NetworkId,
			networkReferral.ReferralNetworkId,
			createTime,
		))
	})
	return networkReferral

}

type ReferralNetwork struct {
	Id   server.Id `json:"id"`
	Name string    `json:"name"`
}

// func GetNetworkReferralByNetworkId(
func GetReferralNetworkByChildNetworkId(
	ctx context.Context,
	networkId server.Id,
) *ReferralNetwork {

	var referralNetwork *ReferralNetwork

	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
				SELECT
					network_referral.referral_network_id,
					network.network_name
				FROM network_referral

				INNER JOIN network ON network.network_id = network_referral.referral_network_id

				WHERE
					network_referral.network_id = $1
			`,
			networkId,
		)

		server.WithPgResult(result, err, func() {

			if result.Next() {
				referralNetwork = &ReferralNetwork{}
				server.Raise(result.Scan(
					&referralNetwork.Id,
					&referralNetwork.Name,
					// &referralNetwork.CreateTime,
				))
			}
		})

	})

	return referralNetwork

}

/**
 * Removes parent network referral
 */
func UnlinkReferralNetwork(
	ctx context.Context,
	networkId server.Id,
) {

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_referral
				WHERE
					network_id = $1
			`,
			networkId,
		))
	})

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
					referral_network_id,
					create_time
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
					&networkReferral.CreateTime,
				))
				networkReferrals = append(networkReferrals, networkReferral)
			}
		})

	})

	return networkReferrals

}

// todo - testme
func GetNetworkReferralsMap(
	ctx context.Context,
) map[server.Id][]server.Id {

	networkReferrals := map[server.Id][]server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT network_id, referral_network_id FROM network_referral`,
		)
		if err != nil {
			return
		}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId, referralNetworkId server.Id
				server.Raise(result.Scan(&networkId, &referralNetworkId))
				networkReferrals[referralNetworkId] = append(networkReferrals[referralNetworkId], networkId)
			}
		})
	})

	return networkReferrals

}
