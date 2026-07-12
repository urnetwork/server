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

	var networkReferral *NetworkReferral

	referralCode = strings.ToUpper(referralCode)

	// find network id by associated referral code
	referralNetworkId := GetNetworkIdByReferralCode(referralCode)

	if referralNetworkId == nil {
		return nil
	}

	if referralNetworkId == &networkId {
		return nil
	}

	createTime := server.NowUtc()

	server.Tx(ctx, func(tx server.PgTx) {

		// count referral_network records for this network
		var count int
		result, err := tx.Query(
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
		})

		// if count >= max allowed, abort

		// Ask ReferralsCapped, never MaxReferrals directly. With no pro.yml the raw number
		// is 0, and `count >= 0` is always true -- every referral would be silently
		// refused and the feature would just stop working. No spec means no cap.
		if Pro().ReferralsCapped(count) {
			return
		}

		_, err = tx.Exec(
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
			networkId,
			referralNetworkId,
			createTime,
		)

		if err == nil {
			// create network referral
			networkReferral = &NetworkReferral{
				NetworkId:         &networkId,
				ReferralNetworkId: referralNetworkId,
			}
		}
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

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
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

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
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

// ReferralBonusCount is the number of referrals a referrer is paid for: the
// referral count capped at the configured maximum (pro.yml referral.max_referrals).
func ReferralBonusCount(referralCount int) int {
	// no pro.yml -> MaxReferrals is 0 -> no bonus to pay
	maxReferrals := Pro().MaxReferrals
	if maxReferrals <= 0 {
		return 0
	}
	if maxReferrals < referralCount {
		return maxReferrals
	}
	if referralCount < 0 {
		return 0
	}
	return referralCount
}

// AddReferralBonusesToAllNetworks grants every referrer its referral bonus for a
// single grant window: bonusPerReferral × min(referrals, pro.yml max_referrals).
// Referrals pay out every period for life, so this is called on the recurring
// refresh cadence alongside the tier data grant.
//
// The balance is added with AddBasicTransferBalance, i.e. net revenue 0, so it is
// an UNPAID balance: referral data can never by itself confer Pro (Pro keys off
// subscription_renewal — see IsPro).
//
// Returns the byte count granted per referrer network.
func AddReferralBonusesToAllNetworks(
	ctx context.Context,
	startTime time.Time,
	endTime time.Time,
	bonusPerReferral ByteCount,
) (addedTransferBalances map[server.Id]ByteCount) {
	addedTransferBalances = map[server.Id]ByteCount{}

	if bonusPerReferral <= 0 {
		return
	}

	// referralNetworkId -> the networks it referred
	referrals := GetNetworkReferralsMap(ctx)

	server.Tx(ctx, func(tx server.PgTx) {
		for referralNetworkId, referredNetworkIds := range referrals {
			bonusCount := ReferralBonusCount(len(referredNetworkIds))
			if bonusCount <= 0 {
				continue
			}

			balanceByteCount := bonusPerReferral * ByteCount(bonusCount)
			err := AddBasicTransferBalanceInTx(
				tx,
				ctx,
				referralNetworkId,
				balanceByteCount,
				startTime,
				endTime,
			)
			if err != nil {
				// do not fail the whole batch for one referrer
				continue
			}
			addedTransferBalances[referralNetworkId] = balanceByteCount
		}
	})

	return
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
