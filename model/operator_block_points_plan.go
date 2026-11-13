// What operator block points (operator_block_points.go) keep of the payout
// plan, which ended with USDC provider payouts (owner decisions 2026-11-12 and
// 2026-11-13: "settle closed rows, let's close out usdc entirely"). An award
// computes a block's points by the plan's rules, writing nothing until it
// commits:
//   - the reliability of the block's window, read in bounded statements and
//     added exactly (readPaymentPlanReliabilityPartials)
//   - the subscription revenue that weighs the subsidy
//     (subsidyNetRevenuePerPeriod)
//   - the points rule (planPayoutPoints)
//   - each payment's operator blocks (paymentPlanBlocksSql)
package model

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Blocks per chunk of an award's reliability read: six hours of 60-second
// blocks, about 28M client_reliability rows on Main. The award reads the
// locations of a chunk's new clients after each chunk.
var paymentPlanReliabilityChunkBlockCount int64 = 360

// Blocks per read-only aggregate statement: a quarter hour of blocks, about
// 1.2M client_reliability rows on Main, read in about 5-17 s at the measured
// 0.33-1.1 s per block (about 32 s at the worst 2.1 s).
var paymentPlanReliabilityReadBlockCount int64 = 15

// The unshifted block window of a subsidy time window, as
// UpdateNetworkReliabilityScoresInTx computes it.
func paymentPlanReliabilityBlockWindow(minTime time.Time, maxTime time.Time) (minBlockNumber int64, maxBlockNumber int64) {
	return reliabilityBlockNumber(minTime), reliabilityBlockNumber(maxTime) + 1
}

// The per-client partials of a shifted block range: the rows and per-row terms
// of the single statement, with the count as bigint and the sum as numeric.
const paymentPlanReliabilityReadSql = `
	SELECT
		valid_counts.network_id,
		valid_counts.client_id,
		COUNT(*),
		SUM(1.0/valid_counts.valid_client_count)
	FROM (
		SELECT
			network_id,
			client_id,
			COUNT(*) OVER (PARTITION BY block_number, client_address_hash) AS valid_client_count
		FROM client_reliability
		WHERE
			valid = true AND
			$1 <= block_number AND
			block_number < $2 AND
			NOT (block_number = ANY($3::bigint[]))
	) valid_counts
	GROUP BY
		valid_counts.network_id,
		valid_counts.client_id
`

// A partial's client under the network its rows carry, as the partials key it.
type paymentPlanReliabilityPartialKey struct {
	networkId server.Id
	clientId  server.Id
}

// A client's exact count and sum over the blocks read so far.
type paymentPlanReliabilityPartial struct {
	validBlockCount int64
	reliabilitySum  reliabilityExactSum
}

// Tests only: runs on a bounded read's connection after the read has consumed
// its rows, before the read returns the connection to its pool, where a slow
// client idles. Production leaves it nil.
var testingBoundedReadDone func(read string)

func boundedReadDone(read string) {
	if testingBoundedReadDone != nil {
		testingBoundedReadDone(read)
	}
}

// Reads the partials of shifted blocks [minBlockNumber, maxBlockNumber) in
// read-only statements of at most paymentPlanReliabilityReadBlockCount blocks,
// and adds them exactly. A read joins the totals only after its statement
// returns, so a retried read is counted once.
//
// Each read is a statement of its own, outside any transaction. A read
// returns about 90k rows on Main, which wait in socket buffers after the
// server has sent them, and a slow client (a loaded host, a tunnel) takes a
// while to drain them. In a transaction the server sits idle in it all that
// time, and Main ends such a session after idle_in_transaction_session_timeout
// (60 s): the operator block points dry run failed that way on 2026-10-29,
// 2.5 h into a block. Outside a transaction no timeout applies.
func readPaymentPlanReliabilityPartials(
	ctx context.Context,
	minBlockNumber int64,
	maxBlockNumber int64,
	degradedBlockNumbers []int64,
) map[paymentPlanReliabilityPartialKey]*paymentPlanReliabilityPartial {
	partials := map[paymentPlanReliabilityPartialKey]*paymentPlanReliabilityPartial{}
	for readMinBlockNumber := minBlockNumber; readMinBlockNumber < maxBlockNumber; {
		readMaxBlockNumber := min(readMinBlockNumber+paymentPlanReliabilityReadBlockCount, maxBlockNumber)
		type read struct {
			key             paymentPlanReliabilityPartialKey
			validBlockCount int64
			reliabilitySum  pgtype.Numeric
		}
		var reads []read
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			reads = nil
			result, err := conn.Query(
				ctx,
				paymentPlanReliabilityReadSql,
				readMinBlockNumber,
				readMaxBlockNumber,
				degradedBlockNumbers,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var r read
					server.Raise(result.Scan(&r.key.networkId, &r.key.clientId, &r.validBlockCount, &r.reliabilitySum))
					reads = append(reads, r)
				}
			})
			boundedReadDone("reliability_partials")
		})
		for _, r := range reads {
			partial, ok := partials[r.key]
			if !ok {
				partial = &paymentPlanReliabilityPartial{}
				partials[r.key] = partial
			}
			partial.validBlockCount += r.validBlockCount
			partial.reliabilitySum.add(r.reliabilitySum)
		}
		readMinBlockNumber = readMaxBlockNumber
	}
	return partials
}

// No product nets more than this in one purchase; a year of Pro nets about $40.
// A balance claiming more holds a misrecorded price, such as an App Store price
// from a non-USD storefront read as USD, and adds nothing to the subsidy revenue.
const maxPlausibleBalanceSubsidyNetRevenueUsd = 100.0

// A balance that funded a selected sweep, with the subscription revenue it carries.
type subsidyRevenueBalance struct {
	startTime         time.Time
	endTime           time.Time
	subsidyNetRevenue NanoCents
}

// The subscription revenue rate per subsidy period (`days`) over the subsidy
// window, so that `subscription_net_revenue_fraction` of it compares with
// `min_payout_usd`, which is also per period, before both scale to the window.
// Each balance adds its revenue pro-rated to the part of the window it covers:
// a balance spans many plan windows (a month of Pro, a year's purchase), and
// adding its full revenue to each plan that drew from it counted it many times
// over. A balance above a plausible price adds nothing.
func subsidyNetRevenuePerPeriod(
	subsidyRevenueBalances []*subsidyRevenueBalance,
	windowStartTime time.Time,
	windowEndTime time.Time,
	period time.Duration,
) NanoCents {
	window := windowEndTime.Sub(windowStartTime)
	if window <= 0 {
		return 0
	}
	maxBalanceSubsidyNetRevenue := UsdToNanoCents(maxPlausibleBalanceSubsidyNetRevenueUsd)
	windowSubsidyNetRevenue := 0.0
	for _, revenueBalance := range subsidyRevenueBalances {
		if revenueBalance.subsidyNetRevenue <= 0 || maxBalanceSubsidyNetRevenue < revenueBalance.subsidyNetRevenue {
			continue
		}
		balanceDuration := revenueBalance.endTime.Sub(revenueBalance.startTime)
		overlap := server.MinTime(revenueBalance.endTime, windowEndTime).Sub(server.MaxTime(revenueBalance.startTime, windowStartTime))
		if balanceDuration <= 0 || overlap <= 0 {
			continue
		}
		windowSubsidyNetRevenue += float64(revenueBalance.subsidyNetRevenue) * float64(overlap) / float64(balanceDuration)
	}
	return NanoCents(windowSubsidyNetRevenue * float64(period) / float64(window))
}

// Each payment's operator blocks, from the paid sweeps it selected and by the
// history rollup's definition (pointsPaidSweepSql). The selection carries each
// sweep's bytes and time, so this reads neither the sweep nor the contract
// table.
var paymentPlanBlocksSql = `
	INSERT INTO account_payment_block (payment_id, block_number)
	SELECT DISTINCT
		payment_network_ids.payment_id,
		` + pointsSweepBlockSql("selected_escrow", "$1", "$2") + `
	FROM temp_account_payment AS selected_escrow
	INNER JOIN temp_payment_network_ids AS payment_network_ids ON
		payment_network_ids.network_id = selected_escrow.network_id
	WHERE
		` + pointsPaidSweepSql("selected_escrow") + ` AND
		` + pointsSweepInBlocksSql("selected_escrow", "$1")

// The inputs of the points rule of one plan: its points, its payments by
// network, and the bonuses' inputs. Operator block points
// (operator_block_points.go) apply the same rule to a block's computed
// payments.
type payoutPointsPlan struct {
	ctx           context.Context
	subsidyConfig *SubsidyConfig
	paymentPlanId server.Id
	// account_points_per_payout scaled to the plan's subsidy window
	totalPoints float64
	// the sum of the payments' payouts, net of carried bonuses
	totalPayout          NanoCents
	payments             map[server.Id]*AccountPayment
	reliabilitySubsidies map[server.Id]NetworkReliabilitySubsidy
	seekerHolders        map[server.Id]bool
	// network -> the network that referred it
	referralParents map[server.Id]server.Id
	// network -> the networks it referred
	networkReferrals map[server.Id][]server.Id
}

// The points rule of a plan, without writing anything:
//   - reliability points of each network with a payment
//   - payout points in proportion to each payment's payout, net of carried
//     bonuses
//   - the seeker multiplier, which also doubles the reliability points
//   - the referral parent's fraction and the referral children's halving
//     fractions of the payout points
//
// Networks are visited in id order, so the rows are the same for the same
// inputs. The order changes nothing else: every row depends only on its own
// payment and the plan's totals.
func planPayoutPoints(plan *payoutPointsPlan) []ApplyAccountPointsArgs {
	queuedPoints := []ApplyAccountPointsArgs{}
	queuePoints := func(args ApplyAccountPointsArgs) {
		queuedPoints = append(queuedPoints, args)
	}

	/**
	 * Apply reliability points
	 */
	for _, networkId := range slices.SortedFunc(maps.Keys(plan.reliabilitySubsidies), server.Id.Cmp) {
		reliabilitySubsidy := plan.reliabilitySubsidies[networkId]

		payment, ok := plan.payments[networkId]
		if !ok {
			glog.Errorf("[plan]no payment found for network %s, but reliability subsidy is %d nano cents\n", networkId, reliabilitySubsidy)
			continue
		}

		if reliabilitySubsidy.Points > 0 {
			reliabilityPointsArgs := ApplyAccountPointsArgs{
				NetworkId:        payment.NetworkId,
				Event:            AccountPointEventReliability,
				PointValue:       reliabilitySubsidy.Points,
				AccountPaymentId: &payment.PaymentId,
				PaymentPlanId:    &plan.paymentPlanId,
			}
			queuePoints(reliabilityPointsArgs)
		}
	}

	for _, networkId := range slices.SortedFunc(maps.Keys(plan.payments), server.Id.Cmp) {
		payment := plan.payments[networkId]

		/**
		 * Payout account points
		 */
		scaledAccountPoints := PointsToNanoPoints(plan.totalPoints * (float64(payment.Payout-payment.BonusPayout) / float64(plan.totalPayout)))
		glog.Infof("[plan]payout %s with %d nano points (%d nano cents)\n",
			payment.NetworkId,
			scaledAccountPoints,
			payment.Payout,
		)

		if scaledAccountPoints > 0 {

			accountPointsArgs := ApplyAccountPointsArgs{
				NetworkId:        payment.NetworkId,
				Event:            AccountPointEventPayout,
				PointValue:       scaledAccountPoints,
				PaymentPlanId:    &payment.PaymentPlanId,
				AccountPaymentId: &payment.PaymentId,
			}

			queuePoints(accountPointsArgs)

		}

		/**
		 * Seeker bonus
		 */
		if plan.seekerHolders[payment.NetworkId] {
			seekerBonus := NanoPoints(float64(scaledAccountPoints)*plan.subsidyConfig.SeekerHolderMultiplier) - scaledAccountPoints
			scaledAccountPoints = NanoPoints(float64(scaledAccountPoints) * plan.subsidyConfig.SeekerHolderMultiplier) // apply the multiplier to the account points

			// double reliability points for seeker holders
			seekerBonus += plan.reliabilitySubsidies[payment.NetworkId].Points
			glog.Infof("[plan]payout seeker holder %s with %d bonus points\n", payment.NetworkId, seekerBonus)

			accountPointsArgs := ApplyAccountPointsArgs{
				NetworkId:        payment.NetworkId,
				Event:            AccountPointEventPayoutMultiplier,
				PointValue:       seekerBonus,
				PaymentPlanId:    &payment.PaymentPlanId,
				AccountPaymentId: &payment.PaymentId,
			}

			queuePoints(accountPointsArgs)
		}

		/**
		 * Apply bonus points to parent referral network
		 */
		parentReferralNetworkId, ok := plan.referralParents[payment.NetworkId]

		if ok {
			if parentReferralNetworkId != payment.NetworkId {
				parentReferralPoints := NanoPoints(float64(scaledAccountPoints) * plan.subsidyConfig.ReferralParentPayoutFraction)

				glog.Infof("[plan]payout applying referral parent network %s with %d points\n", parentReferralNetworkId, scaledAccountPoints)

				accountPointsArgs := ApplyAccountPointsArgs{
					NetworkId:        parentReferralNetworkId,
					Event:            AccountPointEventPayoutLinkedAccount,
					PointValue:       parentReferralPoints,
					PaymentPlanId:    &payment.PaymentPlanId,
					LinkedNetworkId:  &payment.NetworkId,
					AccountPaymentId: &payment.PaymentId,
				}

				queuePoints(accountPointsArgs)
			}
		}

		/**
		 * Apply bonus points to child referral networks
		 */
		visited := make(map[server.Id]struct{})
		payoutChildrenReferralNetworksInTx(
			plan.ctx,
			queuePoints,
			scaledAccountPoints,
			plan.subsidyConfig.ReferralChildPayoutFraction,
			payment.NetworkId,
			plan.networkReferrals,
			payment.PaymentPlanId,
			&payment.PaymentId,
			nil,
			0,
			visited,
		)
	}

	return queuedPoints
}
