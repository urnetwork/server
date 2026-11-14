package model

// Redeeming a Pro gift code (mmm/GIFTOFPRIVACY.md §5, §6, §10.3, §10.6).
//
// A gift code grants Pro for its pro_duration from max(now, the latest end of
// the network's paid cover in every market, gifts included: giftPaidCoverSql),
// so a recipient who already pays for Pro (card, App Store, Google Play,
// crypto) gets the gift year queued after the paid period, and a second gift
// queues after the first. A trial or a hold, which nobody paid for, queues
// nothing. When the cover a year waits behind ends early, the hourly
// RebaseQueuedGiftYears moves the network's queued years up to start then. The grant takes the path every paid Pro period takes: a
// supporter renewal (market gift, its paid end), a Pro marker of 0 bytes for
// the same window, and the cycle grant (GrantProCycleInTx), whose sweep grants
// the year's later months. Nothing bills anyone: a store subscription that
// renews later overlaps the gift.
//
// The year carries the code's net revenue, its share of what the giver paid,
// as every paid Pro year carries its payment: on the renewal, whose cycles take
// their subsidy revenue from it (proCycleSubsidyNetRevenue), and on the marker.
// So the recipient's traffic is paid traffic (PaidPriority) and the providers
// serving it are paid from it, as for a year bought with a card or coins (owner
// decision, 2026-11-13). The gift market stays out of SubscriptionMarkets and
// the winback's paid markets: the recipient did not pay.
//
// After the redeem's transaction commits, FinishGiftRedemption refreshes the
// network's Pro and, when the code carries its giver's network, links the
// redeemer as the giver's referral (pro.yml referral:), only when the redeemer
// has no referrer yet and is not the giver, under the referral cap.
//
// A refund of a redeemed gift (ClawbackBalanceCodesForPurchaseEventInTx) ends
// its renewal and marker, and the network's running cycle grant when no other
// subscription is left open, so the monthly sweep stops granting.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// RedeemBalanceCodePro is the Pro a redeemed gift code granted: from StartTime
// to EndTime. Queued is true when the year starts after the network's current
// paid period instead of now.
type RedeemBalanceCodePro struct {
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Queued    bool      `json:"queued"`
}

// GiftRedemption is a Pro gift a transaction redeemed (RedeemBalanceCodeInTx),
// for what its caller owes once the transaction commits
// (FinishGiftRedemption).
type GiftRedemption struct {
	NetworkId     server.Id
	BalanceCodeId server.Id
	// the giver's network, whose referral the redeemer becomes
	ReferrerNetworkId *server.Id
	StartTime         time.Time
	EndTime           time.Time
	Queued            bool
}

// redeemGiftCodeInTx redeems a pro_gift code into the network, in the
// caller's transaction, under the network's payment lock
// (RedeemBalanceCodeInTx). A code past its redeem-by, refunded or already
// redeemed is refused with a result error, before anything is written.
func redeemGiftCodeInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	code *balanceCodeState,
	now time.Time,
) (*RedeemBalanceCodeResult, error) {
	if status := balanceCodeStatusOf(code, now).Status; status != GiftCodeStatusOk {
		return &RedeemBalanceCodeResult{
			Error: &RedeemBalanceCodeError{
				Message: giftCodeStatusMessage(status),
			},
		}, nil
	}
	if code.proDuration <= 0 {
		return nil, fmt.Errorf("gift code %s grants no Pro", code.BalanceCodeId)
	}

	// One gift redemption of a network at a time, and none while one of its
	// cycles is granted: the second of two gifts must read the first's year to
	// queue after it. This is the cycle grant's own lock, which
	// GrantProCycleInTx takes again below.
	lockProGrantCycleInTx(tx, ctx, networkId)

	startTime := now
	if coverEnd := giftProCoverEndInTx(tx, ctx, networkId, now); now.Before(coverEnd) {
		startTime = coverEnd
	}
	endTime := startTime.Add(code.proDuration)
	queued := now.Before(startTime)

	// the code's grant, private until the code and grant commit together
	markerId := server.NewId()

	// As for a data code, the predicates re-check the read at write time, so
	// of two concurrent redeems exactly one writes, and a refund's void and a
	// redeem cannot both land. The code's window becomes the Pro year, as a data
	// code's becomes its balance's; its redeem-by stays on its order.
	redeemTag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE transfer_balance_code
			SET
				redeem_time = $2,
				redeem_balance_id = $3,
				start_time = $4,
				end_time = $5,
				network_id = $6
			WHERE
				balance_code_id = $1 AND
				redeem_balance_id IS NULL AND
				cancel_time IS NULL
		`,
		code.BalanceCodeId,
		now,
		markerId,
		startTime,
		endTime,
		networkId,
	))
	if redeemTag.RowsAffected() == 0 {
		return &RedeemBalanceCodeResult{
			Error: &RedeemBalanceCodeError{
				Message: balanceCodeRedeemedMessage,
			},
		}, nil
	}

	// a failure past the redeem raises: never commit a spent code without its
	// year
	// the year's payment: the code's share of what the giver paid, from which
	// each of its cycles takes its subsidy revenue
	server.Raise(AddSubscriptionRenewalInTx(tx, ctx, &SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   SubscriptionTypeSupporter,
		StartTime:          startTime,
		EndTime:            endTime,
		NetRevenue:         code.NetRevenue,
		SubscriptionMarket: SubscriptionMarketGift,
		// the clawback ends the renewal by its code
		TransactionId: code.BalanceCodeId.String(),
		PaidEndTime:   &endTime,
	}))

	// The year's Pro marker: no data, the renewal's window, its revenue as
	// every paid period's marker carries it (SubsidyNetRevenue). pro is set
	// explicitly (the column defaults to true).
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO transfer_balance (
				balance_id,
				network_id,
				start_time,
				end_time,
				start_balance_byte_count,
				balance_byte_count,
				net_revenue_nano_cents,
				subsidy_net_revenue_nano_cents,
				pro
			)
			VALUES ($1, $2, $3, $4, 0, 0, 0, $5, true)
		`,
		markerId,
		networkId,
		startTime,
		endTime,
		code.NetRevenue,
	))

	// The data of the cycle the year starts, when it starts now. A queued year's
	// first cycle, and every later one, is the hourly sweep's. A network create
	// redeems at RepeatableRead, where this grants nothing; its caller's
	// refresh (controller.AddRefreshTransferBalance) grants it.
	GrantProCycleInTx(tx, ctx, networkId)

	return &RedeemBalanceCodeResult{
		// older clients count a redeem with a transfer balance as a success
		TransferBalance: &RedeemBalanceCodeTransferBalance{
			TransferBalanceId: markerId,
			StartTime:         startTime,
			EndTime:           endTime,
			BalanceByteCount:  0,
		},
		Pro: &RedeemBalanceCodePro{
			StartTime: startTime,
			EndTime:   endTime,
			Queued:    queued,
		},
		gift: &GiftRedemption{
			NetworkId:         networkId,
			BalanceCodeId:     code.BalanceCodeId,
			ReferrerNetworkId: code.referrerNetworkId,
			StartTime:         startTime,
			EndTime:           endTime,
			Queued:            queued,
		},
	}, nil
}

// giftPaidCoverSql is whether the renewal `alias` is paid Pro cover, the Pro a
// gift year queues behind: a window with revenue, or another gift year (a code
// with no revenue still holds its year), that has not been ended. A trial or a
// billing hold carries no revenue: a gift starts now beside it rather than
// wait behind days nobody paid for. An end path (refund, revocation, reconcile
// cut, clawback) ends a renewal by setting its end_time to the moment it ended,
// so a window ended early covers nothing after that moment, and one ended
// before it started is inverted. $2 is SubscriptionTypeSupporter and $3
// SubscriptionMarketGift.
func giftPaidCoverSql(alias string) string {
	return fmt.Sprintf(
		`%[1]s.subscription_type = $2 AND %[1]s.start_time < %[1]s.end_time AND (0 < %[1]s.net_revenue_nano_cents OR %[1]s.market = $3)`,
		alias,
	)
}

// giftProCoverEndInTx is when the Pro the network already has ends: the latest
// end of its paid cover (giftPaidCoverSql) that ends after `after`, in every
// market, gifts included; zero when none does. A renewal's end carries its
// market's grace (a card's or crypto year's 30 days, a store's day), so a
// subscription still retrying a payment covers, and a queued renewal that has
// not started covers too (as GetGammaProCoverEnd reads it). A paid period that
// ends early after a gift queued behind it leaves the gift waiting; the hourly
// RebaseQueuedGiftYears moves it up.
func giftProCoverEndInTx(tx server.PgTx, ctx context.Context, networkId server.Id, after time.Time) (end time.Time) {
	var latest *time.Time
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT MAX(renewal.end_time)
			FROM subscription_renewal renewal
			WHERE
				renewal.network_id = $1 AND
				`+giftPaidCoverSql("renewal")+` AND
				$4 < renewal.end_time
		`,
		networkId,
		SubscriptionTypeSupporter,
		SubscriptionMarketGift,
		after,
	).Scan(&latest))
	if latest != nil {
		end = *latest
	}
	return
}

// FinishGiftRedemption is what a committed gift redemption owes its network:
// the Pro it now holds is made visible at once (UpdateProNetwork), and when the
// code carries its giver's network the redeemer becomes the giver's referral,
// unless it is the giver or already has a referrer (CreateNetworkReferral
// replaces a referrer, so this is checked first), and under the giver's
// referral cap. Call it after the redeem's transaction commits; nil does
// nothing. referred is true when the referral was linked.
func FinishGiftRedemption(ctx context.Context, redemption *GiftRedemption) (referred bool) {
	if redemption == nil {
		return false
	}
	UpdateProNetwork(ctx, redemption.NetworkId)
	return linkGiftReferral(ctx, redemption.NetworkId, redemption.ReferrerNetworkId)
}

func linkGiftReferral(ctx context.Context, networkId server.Id, referrerNetworkId *server.Id) bool {
	if referrerNetworkId == nil || isSelfReferral(networkId, referrerNetworkId) {
		return false
	}
	if networkHasReferrer(ctx, networkId) {
		return false
	}
	referralCode := GetNetworkReferralCode(ctx, *referrerNetworkId)
	if referralCode == nil || referralCode.ReferralCode == "" {
		// the giver's network was removed, or never had a code
		return false
	}
	// the cap is checked inside
	return CreateNetworkReferral(ctx, networkId, referralCode.ReferralCode) != nil
}

// Whether the network has a referrer, the network it is a referral of.
func networkHasReferrer(ctx context.Context, networkId server.Id) (referred bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM network_referral WHERE network_id = $1)`,
			networkId,
		).Scan(&referred))
	})
	return
}

// ----- the refund clawback of a redeemed gift -----

// The gift renewals the clawback ends: by their codes (a renewal's transaction
// id is its code's id), with the networks they are on and the networks the
// codes were redeemed into.
type giftClawback struct {
	codeIds    []string
	networkIds []server.Id
}

// giftClawbackOfInTx reads the networks of the redeemed gift codes' renewals,
// which the clawback must hold the cycle grants of before it admits its
// ownership (giftClawbackCycleBalanceIdsInTx).
func giftClawbackOfInTx(tx server.PgTx, ctx context.Context, codeIds []server.Id, codeNetworkIds []server.Id, now time.Time) *giftClawback {
	clawback := &giftClawback{}
	for _, codeId := range codeIds {
		clawback.codeIds = append(clawback.codeIds, codeId.String())
	}
	clawback.networkIds = append(clawback.networkIds, codeNetworkIds...)
	result, err := tx.Query(
		ctx,
		`
			SELECT DISTINCT network_id
			FROM subscription_renewal
			WHERE
				subscription_type = $1 AND
				market = $2 AND
				transaction_id = ANY($3) AND
				$4 < end_time
		`,
		SubscriptionTypeSupporter,
		SubscriptionMarketGift,
		clawback.codeIds,
		now,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			server.Raise(result.Scan(&networkId))
			clawback.networkIds = append(clawback.networkIds, networkId)
		}
	})
	slices.SortFunc(clawback.networkIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	clawback.networkIds = slices.Compact(clawback.networkIds)
	return clawback
}

// The grants of the running cycles of the clawback's networks: every one the
// clawback may end, for its single ownership admission. The end step re-reads
// them and refuses any it was not admitted for.
func giftClawbackCycleBalanceIdsInTx(tx server.PgTx, ctx context.Context, clawback *giftClawback, now time.Time) (balanceIds []server.Id) {
	if len(clawback.networkIds) == 0 {
		return nil
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT cycle.balance_id
			FROM pro_grant_cycle cycle
			INNER JOIN transfer_balance balance ON balance.balance_id = cycle.balance_id
			WHERE
				cycle.network_id = ANY($1) AND
				cycle.ended_time IS NULL AND
				$2 < balance.end_time
		`,
		clawback.networkIds,
		now,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			server.Raise(result.Scan(&balanceId))
			balanceIds = append(balanceIds, balanceId)
		}
	})
	return
}

// endGiftRenewalsInTx ends, at now, the renewals of the clawback's redeemed
// gifts, then the running cycle grant of each of their networks left with no
// supporter renewal open (as endReconciledEntitlementInTx ends a refunded
// period's), so the cycle sweep stops granting the year. The cycle grants
// ended must all be in admitted, the clawback's ownership; one that appeared
// since raises errTransferBalanceOwnershipBusy, rolling the refund back for
// redelivery. Returns the networks whose Pro changed. Idempotent: every
// predicate requires an end after now.
func endGiftRenewalsInTx(tx server.PgTx, ctx context.Context, clawback *giftClawback, admitted []server.Id, now time.Time) (endedNetworkIds []server.Id) {
	if len(clawback.codeIds) == 0 {
		return nil
	}
	result, err := tx.Query(
		ctx,
		`
			UPDATE subscription_renewal
			SET end_time = $4
			WHERE
				subscription_type = $1 AND
				market = $2 AND
				transaction_id = ANY($3) AND
				$4 < end_time
			RETURNING network_id
		`,
		SubscriptionTypeSupporter,
		SubscriptionMarketGift,
		clawback.codeIds,
		now,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			server.Raise(result.Scan(&networkId))
			endedNetworkIds = append(endedNetworkIds, networkId)
		}
	})
	if len(clawback.networkIds) == 0 {
		return
	}

	// the renewals ended above are no longer open here
	var ownershipAdmitted bool
	var cycleNetworkIds []server.Id
	server.Raise(tx.QueryRow(
		ctx,
		`
			WITH cycle_grants AS MATERIALIZED (
				SELECT cycle.network_id, cycle.cycle_start_time, cycle.balance_id
				FROM pro_grant_cycle cycle
				INNER JOIN transfer_balance balance ON balance.balance_id = cycle.balance_id
				WHERE
					cycle.network_id = ANY($1) AND
					cycle.ended_time IS NULL AND
					$2 < balance.end_time AND
					NOT EXISTS (
						SELECT 1
						FROM subscription_renewal other
						WHERE
							other.network_id = cycle.network_id AND
							other.subscription_type = $3 AND
							other.start_time <= $2 AND
							$2 < `+proRenewalOpenEndSql("other")+`
					)
			), ownership AS MATERIALIZED (
				SELECT NOT EXISTS (
					SELECT 1 FROM cycle_grants WHERE NOT (balance_id = ANY($4::uuid[]))
				) AS admitted
			), ended_balances AS (
				UPDATE transfer_balance balance
				SET end_time = $2
				FROM cycle_grants
				WHERE
					balance.balance_id = cycle_grants.balance_id AND
					$2 < balance.end_time AND
					(SELECT admitted FROM ownership)
				RETURNING balance.balance_id
			), ended_cycles AS (
				-- a cycle already past its end, in its grant's grace, keeps its end
				UPDATE pro_grant_cycle cycle
				SET ended_time = LEAST($2, cycle.cycle_end_time)
				FROM cycle_grants
				WHERE
					cycle.network_id = cycle_grants.network_id AND
					cycle.cycle_start_time = cycle_grants.cycle_start_time AND
					cycle.ended_time IS NULL AND
					(SELECT admitted FROM ownership)
				RETURNING cycle.network_id
			)
			SELECT admitted, ARRAY(SELECT DISTINCT network_id FROM ended_cycles)
			FROM ownership
		`,
		clawback.networkIds,
		now,
		SubscriptionTypeSupporter,
		admitted,
	).Scan(&ownershipAdmitted, &cycleNetworkIds))
	if !ownershipAdmitted {
		server.Raise(errTransferBalanceOwnershipBusy)
	}
	endedNetworkIds = append(endedNetworkIds, cycleNetworkIds...)
	return
}

// NetworkLastGiftWindowEnd is the end of the network's latest redeemed gift
// year, ok false when it never redeemed one. A gift year is Pro the network
// had but did not buy: the paid-window facts (NetworkLastPaidWindow) leave it
// out, and what asks when Pro stopped reads it beside them.
func NetworkLastGiftWindowEnd(ctx context.Context, networkId server.Id) (end time.Time, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		var latest *time.Time
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT MAX(end_time) FROM subscription_renewal
				WHERE network_id = $1 AND subscription_type = $2 AND market = $3
			`,
			networkId,
			SubscriptionTypeSupporter,
			SubscriptionMarketGift,
		).Scan(&latest))
		if latest != nil {
			end, ok = *latest, true
		}
	})
	return
}

// ----- the gift year queue's reconciliation -----

// The networks one reconciliation run lists.
const rebaseQueuedGiftYearsBatchSize = 1000

// RebaseQueuedGiftYears is the hourly reconciliation of the gift year queue. A
// gift year redeemed while the network had paid Pro is queued after that cover
// (giftProCoverEndInTx). When the cover then ends early (a store refund or
// revocation, a card period cut, a refunded gift year ahead of it), the queued
// years would leave the network without Pro until their start. So for every
// network with a gift year queued after `now` and no paid cover at `now`
// (giftPaidCoverSql), its queued gift years are re-based to start at `now`,
// back to back in their order, each keeping its length: the renewal, its Pro
// marker and the code's window move together, and the cycle the first year
// starts is granted. A network re-based has cover at `now`, so a second run
// changes nothing; a network still covered keeps its queue. Returns the
// networks re-based with the number of years each moved; their Pro cache is
// refreshed.
func RebaseQueuedGiftYears(ctx context.Context, now time.Time) (rebased map[server.Id]int) {
	rebased = map[server.Id]int{}
	for _, networkId := range rebaseQueuedGiftYearCandidates(ctx, now) {
		count := 0
		server.HandleError(func() {
			count = rebaseQueuedGiftYearsOfNetwork(ctx, networkId, now)
		}, func(err error) {
			// the next run tries again
			glog.Errorf("[gift]could not re-base the queued gift years of network %s: %s\n", networkId, err)
		})
		if 0 < count {
			rebased[networkId] = count
			UpdateProNetwork(ctx, networkId)
		}
	}
	return
}

// The networks with a gift year queued after now and no paid cover at now.
func rebaseQueuedGiftYearCandidates(ctx context.Context, now time.Time) (networkIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT queued.network_id
				FROM subscription_renewal queued
				WHERE
					queued.subscription_type = 'supporter' AND
					$1 < queued.end_time AND
					$1 < queued.start_time AND
					queued.start_time < queued.end_time AND
					queued.market = $3 AND
					NOT EXISTS (
						SELECT 1
						FROM subscription_renewal cover
						WHERE
							cover.network_id = queued.network_id AND
							`+giftPaidCoverSql("cover")+` AND
							cover.start_time <= $1 AND
							$1 < cover.end_time
					)
				LIMIT $4
			`,
			now,
			SubscriptionTypeSupporter,
			SubscriptionMarketGift,
			rebaseQueuedGiftYearsBatchSize,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	return
}

// A queued gift year as the re-base reads it.
type queuedGiftYear struct {
	balanceCodeId server.Id
	markerId      server.Id
	startTime     time.Time
	endTime       time.Time
}

// Re-bases one network's queued gift years to start at now, under its payment
// lock and its cycle lock (the redeem path's), with the ownership every writer
// of a granted balance takes, in one transaction: all of the network's years
// move, or none. Returns the years moved; 0 when the network is gone, is
// covered again, or another writer holds a marker (the next run tries again).
func rebaseQueuedGiftYearsOfNetwork(ctx context.Context, networkId server.Id, now time.Time) (count int) {
	server.Tx(ctx, func(tx server.PgTx) {
		count = 0
		if err := LockPaymentNetworkInTx(tx, ctx, networkId); err != nil {
			// the network was removed
			return
		}
		lockProGrantCycleInTx(tx, ctx, networkId)

		var covered bool
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM subscription_renewal cover
					WHERE
						cover.network_id = $1 AND
						`+giftPaidCoverSql("cover")+` AND
						cover.start_time <= $4 AND
						$4 < cover.end_time
				)
			`,
			networkId,
			SubscriptionTypeSupporter,
			SubscriptionMarketGift,
			now,
		).Scan(&covered))
		if covered {
			return
		}

		years := []*queuedGiftYear{}
		codeIds := []server.Id{}
		result, err := tx.Query(
			ctx,
			`
				SELECT transaction_id, start_time, end_time
				FROM subscription_renewal
				WHERE
					network_id = $1 AND
					subscription_type = $2 AND
					market = $3 AND
					$4 < start_time AND
					start_time < end_time
				ORDER BY start_time, end_time
			`,
			networkId,
			SubscriptionTypeSupporter,
			SubscriptionMarketGift,
			now,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var transactionId string
				year := &queuedGiftYear{}
				server.Raise(result.Scan(&transactionId, &year.startTime, &year.endTime))
				balanceCodeId, parseErr := server.ParseId(transactionId)
				server.Raise(parseErr)
				year.balanceCodeId = balanceCodeId
				years = append(years, year)
				codeIds = append(codeIds, balanceCodeId)
			}
		})
		if len(years) == 0 {
			return
		}

		markers := map[server.Id]server.Id{}
		result, err = tx.Query(
			ctx,
			`
				SELECT balance_code_id, redeem_balance_id
				FROM transfer_balance_code
				WHERE
					balance_code_id = ANY($1) AND
					kind = $2 AND
					redeem_balance_id IS NOT NULL
			`,
			codeIds,
			BalanceCodeKindProGift,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceCodeId, markerId server.Id
				server.Raise(result.Scan(&balanceCodeId, &markerId))
				markers[balanceCodeId] = markerId
			}
		})
		markerIds := []server.Id{}
		for _, year := range years {
			markerId, ok := markers[year.balanceCodeId]
			if !ok {
				server.Raise(fmt.Errorf("the queued gift year of code %s has no redeemed code", year.balanceCodeId))
			}
			year.markerId = markerId
			markerIds = append(markerIds, markerId)
		}
		admitted, ownershipErr := tryTransferBalanceOwnershipInTx(ctx, tx, markerIds)
		server.Raise(ownershipErr)
		if !admitted {
			return
		}

		startTime := now
		for _, year := range years {
			endTime := startTime.Add(year.endTime.Sub(year.startTime))
			// in start order, so a moved year's window is never another's yet
			renewalTag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE subscription_renewal
					SET
						start_time = $5,
						end_time = $6,
						paid_end_time = $6
					WHERE
						network_id = $1 AND
						subscription_type = $2 AND
						market = $3 AND
						transaction_id = $4 AND
						start_time = $7 AND
						end_time = $8
				`,
				networkId,
				SubscriptionTypeSupporter,
				SubscriptionMarketGift,
				year.balanceCodeId.String(),
				startTime,
				endTime,
				year.startTime,
				year.endTime,
			))
			markerTag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE transfer_balance
					SET
						start_time = $3,
						end_time = $4
					WHERE
						balance_id = $1 AND
						network_id = $2 AND
						pro AND
						start_time = $5 AND
						end_time = $6
				`,
				year.markerId,
				networkId,
				startTime,
				endTime,
				year.startTime,
				year.endTime,
			))
			if renewalTag.RowsAffected() != 1 || markerTag.RowsAffected() != 1 {
				server.Raise(fmt.Errorf("the queued gift year of code %s changed while it was re-based", year.balanceCodeId))
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE transfer_balance_code
					SET
						start_time = $2,
						end_time = $3
					WHERE balance_code_id = $1 AND redeem_balance_id = $4
				`,
				year.balanceCodeId,
				startTime,
				endTime,
				year.markerId,
			))
			startTime = endTime
		}

		// the data of the cycle the first year now starts
		GrantProCycleInTx(tx, ctx, networkId)
		count = len(years)
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}
