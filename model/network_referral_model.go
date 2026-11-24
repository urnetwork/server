package model

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

type NetworkReferral struct {
	NetworkId         *server.Id `json:"network_id"`
	ReferralNetworkId *server.Id `json:"referral_network_id"`
	CreateTime        time.Time  `json:"create_time"`
}

// Reports whether a referral code resolved to the network being referred. A
// network can never be its own referral network. Compare the ids: the looked-up
// pointer never equals the address of the local network id.
func isSelfReferral(networkId server.Id, referralNetworkId *server.Id) bool {
	return referralNetworkId != nil && *referralNetworkId == networkId
}

// Links the network to the referrer whose code this is, or re-points its link
// to that referrer. The referrer's cap counts only its links that have not
// ended (network_referral_terms.go). A link's claim window runs from the
// network's first link: a re-point keeps the link's create_time, and a link
// made again after an unlink takes the first link's time. A link to another
// referrer than the network's existing link is a new referral under the terms,
// so it pays only once the network claims it by its app use, and a link that
// is new, unclaimed and already past its window ends at once.
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

	if isSelfReferral(networkId, referralNetworkId) {
		return nil
	}

	createTime := server.NowUtc()

	server.Tx(ctx, func(tx server.PgTx) {
		// a retried attempt starts over
		networkReferral = nil

		// the referrer's links that count toward its cap
		count := activeReferralCountInTx(ctx, tx, *referralNetworkId, createTime)

		// if count >= max allowed, abort

		// Ask ReferralsCapped, never MaxReferrals directly. With no pro.yml the raw number
		// is 0, and `count >= 0` is always true -- every referral would be silently
		// refused and the feature would just stop working. No spec means no cap.
		if Pro().ReferralsCapped(count) {
			return
		}

		// a failed upsert raises: going on would commit a rollback. The
		// update's right-hand sides read the link as it was.
		var linkCreateTime time.Time
		server.Raise(tx.QueryRow(
			ctx,
			`
				INSERT INTO network_referral (
					network_id,
					referral_network_id,
					create_time
				)
				VALUES (
					$1,
					$2,
					LEAST($3, COALESCE(
						(SELECT first_link_time FROM network_referral_referee WHERE network_id = $1),
						$3
					))
				)
				ON CONFLICT (network_id)
				DO UPDATE SET
					referral_network_id = EXCLUDED.referral_network_id,
					create_time = LEAST(network_referral.create_time, EXCLUDED.create_time),
					existing_link = network_referral.existing_link AND
						network_referral.referral_network_id = EXCLUDED.referral_network_id
				RETURNING create_time
			`,
			networkId,
			referralNetworkId,
			createTime,
		).Scan(&linkCreateTime))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_referral_referee (
					network_id,
					first_link_time
				)
				VALUES ($1, $2)
				ON CONFLICT (network_id) DO UPDATE
				SET first_link_time = LEAST(network_referral_referee.first_link_time, EXCLUDED.first_link_time)
			`,
			networkId,
			linkCreateTime,
		))

		endExpiredReferralLinkInTx(ctx, tx, networkId, createTime)

		networkReferral = &NetworkReferral{
			NetworkId:         &networkId,
			ReferralNetworkId: referralNetworkId,
			CreateTime:        linkCreateTime,
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

// Removes the network's link to its referrer. The network keeps its first
// link's time (network_referral_referee), so a link it makes again pays only
// for what is left of its period.
func UnlinkReferralNetwork(
	ctx context.Context,
	networkId server.Id,
) {

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_referral_referee (
					network_id,
					first_link_time
				)
				SELECT
					network_id,
					create_time
				FROM network_referral
				WHERE network_id = $1
				ON CONFLICT (network_id) DO UPDATE
				SET first_link_time = LEAST(network_referral_referee.first_link_time, EXCLUDED.first_link_time)
			`,
			networkId,
		))
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

// The referrer's links that count toward its max_referrals now: an ended
// link, or a new one past its claim deadline unclaimed, is left out
// (network_referral_terms.go), so the count the apps show matches the cap.
func GetReferralsByReferralNetworkId(
	ctx context.Context,
	referralNetworkId server.Id,
) []*NetworkReferral {

	var networkReferrals []*NetworkReferral

	terms := Pro().ReferralTerms
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_referral.network_id,
					network_referral.referral_network_id,
					network_referral.create_time
				FROM network_referral
				WHERE
					network_referral.referral_network_id = $1 AND
			`+activeReferralLinkSql,
			referralNetworkId,
			terms.claimCutoff(server.NowUtc()),
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

// AddReferralBonusesToAllNetworks grants the referral bonus to BOTH sides for a single
// grant window, re-granted every period for life while the link pays under the referral
// terms (network_referral_terms.go, pro.yml referral): a link that existed when the terms
// began pays, and a link made under them pays once its referred network has claimed it by
// its app use within the claim window; an ended link never pays:
//   - the REFERRER earns bonusPerReferral × min(paying referrals, pro.yml max_referrals);
//   - each REFERRED (referee) network on a paying link earns a flat referredBonus.
//
// The run first settles the claims: the referred networks whose app use claimed their
// link since the last run earn from this one, with their referrers, and the links whose
// window passed unclaimed end. No grant already made is touched.
//
// Called on the recurring refresh cadence alongside the tier data grant. Both sides are
// granted in ONE transaction so a run is atomic.
//
// Balances are added as referral grants (AddGrantTransferBalanceInTx), i.e. net
// revenue 0, so they are unpaid: referral data can never by itself confer Pro (Pro
// keys off subscription_renewal — see IsPro).
//
// Returns the total byte count granted per network (a network that is both a referrer
// and a referee accumulates both grants).
// A supplied task run id commits a replay receipt with both sides' grants;
// omit it for an intentional independent refresh.
func AddReferralBonusesToAllNetworks(
	ctx context.Context,
	startTime time.Time,
	endTime time.Time,
	bonusPerReferral ByteCount,
	referredBonus ByteCount,
	runIds ...server.Id,
) (addedTransferBalances map[server.Id]ByteCount) {
	addedTransferBalances = map[server.Id]ByteCount{}

	if bonusPerReferral <= 0 && referredBonus <= 0 {
		return
	}

	terms := Pro().ReferralTerms
	claimedCount, endedCount, droppedCount := settleReferralClaims(ctx, startTime, &terms)

	// referralNetworkId -> the networks it referred, by the links that pay
	links := getReferralLinks(ctx)
	referrals := payingReferrals(links, &terms)
	payingCount := 0
	for _, referredNetworkIds := range referrals {
		payingCount += len(referredNetworkIds)
	}
	glog.Infof(
		"[referral]%d links, %d paying; %d referred networks newly claimed by app use, %d links ended unclaimed, %d app use days dropped\n",
		len(links),
		payingCount,
		claimedCount,
		endedCount,
		droppedCount,
	)

	// Seeker/Saga holders get their referral data scaled (pro.yml seeker.data_multiplier).
	seekers := GetAllSeekerHolders(ctx)
	seekerMultiplier := Pro().SeekerDataMultiplier()

	server.Tx(ctx, func(tx server.PgTx) {
		addedTransferBalances = map[server.Id]ByteCount{}
		if !claimTransferGrantRunInTx(ctx, tx, GrantKindReferral, startTime, endTime, runIds) {
			return
		}
		// a lag.dev guest gets no referral data and counts toward no
		// referrer's bonus (lag.dev/PLAN.md A1), and neither does a deleted
		// network: its referral links outlive it (a referee keeps its link to
		// a deleted referrer), and until 2026-11-16 each run granted the
		// deleted networks of those links a balance nothing could spend
		referralNetworkIds := referralNetworkIdsOf(referrals)
		lagGuests := lagGuestNetworkIdsInTx(ctx, tx, referralNetworkIds)
		goneNetworks := goneNetworkIdsInTx(ctx, tx, referralNetworkIds)
		excluded := func(networkId server.Id) bool {
			return lagGuests[networkId] || goneNetworks[networkId]
		}
		grant := func(networkId server.Id, byteCount ByteCount) {
			if excluded(networkId) {
				return
			}
			if seekerMultiplier != 1.0 && seekers[networkId] {
				byteCount = ByteCount(float64(byteCount) * seekerMultiplier)
			}
			if byteCount <= 0 {
				return
			}
			server.Raise(AddGrantTransferBalanceInTx(tx, ctx, networkId, GrantKindReferral, byteCount, startTime, endTime))
			addedTransferBalances[networkId] += byteCount
		}

		for referralNetworkId, referredNetworkIds := range referrals {
			referredNetworkIds = slices.DeleteFunc(slices.Clone(referredNetworkIds), excluded)
			// referrer: paid for up to max_referrals of its referrals
			if 0 < bonusPerReferral {
				if bonusCount := ReferralBonusCount(len(referredNetworkIds)); 0 < bonusCount {
					grant(referralNetworkId, bonusPerReferral*ByteCount(bonusCount))
				}
			}
			// referees: each referred network earns a flat bonus. Each referee has exactly
			// one referrer (network_referral PK is network_id), so it is granted once per
			// run; the link cap bounds the size of this list.
			if 0 < referredBonus {
				for _, referredNetworkId := range referredNetworkIds {
					grant(referredNetworkId, referredBonus)
				}
			}
		}
	})

	return
}

// referralNetworkId -> every network it referred, whatever the referral terms
// say about paying the link: the referral tree the operator block points read.
// The referral grant reads its links with getReferralLinks.
//
// A source query failure is not an empty referral set; propagate it so the
// caller retries without committing a receipt for missing work.
func GetNetworkReferralsMap(
	ctx context.Context,
) map[server.Id][]server.Id {

	networkReferrals := map[server.Id][]server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT network_id, referral_network_id FROM network_referral`,
		)
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
