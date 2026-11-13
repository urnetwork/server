// What operator block points (operator_block_points.go) keep of the payout
// plan, which ended with USDC provider payouts (owner decisions 2026-11-12 and
// 2026-11-13: "settle closed rows, let's close out usdc entirely"). An award
// computes a block's points, writing nothing until it commits:
//   - the reliability of the block's window, read in bounded statements and
//     added exactly (readPaymentPlanReliabilityPartials)
//   - the points rule (planPayoutPoints). The plan shared the payout points by
//     a computed payout of net revenue and a revenue-sized subsidy; since
//     2026-11-13 they are shared by bytes served
//     (operatorBlockPayoutPointShares), and no revenue is read.
//   - each payment's operator blocks (paymentPlanBlocksSql)
package model

import (
	"cmp"
	"context"
	"maps"
	"math/bits"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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

// Shares totalNanoPoints among the networks in proportion to their bytes,
// exactly: each takes the floor of its proportional share, and the nano points
// the floors leave, fewer than the networks, go one each to the largest
// remainders, a tie to the lower network id. So the shares always sum to
// totalNanoPoints, and each is within one nano point of its exact share. The
// bytes are positive and their sum fits a ByteCount (planOperatorBlockPoints
// checks it). Pure.
func operatorBlockPayoutPointShares(totalNanoPoints NanoPoints, networkByteCounts map[server.Id]ByteCount) map[server.Id]NanoPoints {
	shares := map[server.Id]NanoPoints{}
	networkIds := slices.SortedFunc(maps.Keys(networkByteCounts), server.Id.Cmp)
	var totalByteCount uint64
	for _, networkId := range networkIds {
		totalByteCount += uint64(networkByteCounts[networkId])
	}
	if totalNanoPoints <= 0 || totalByteCount == 0 {
		return shares
	}
	type remainder struct {
		networkId server.Id
		value     uint64
	}
	remainders := make([]remainder, 0, len(networkIds))
	allocated := uint64(0)
	for _, networkId := range networkIds {
		// totalNanoPoints * bytes / totalByteCount in 128 bits; the quotient is
		// at most totalNanoPoints, since bytes <= totalByteCount
		high, low := bits.Mul64(uint64(totalNanoPoints), uint64(networkByteCounts[networkId]))
		share, rest := bits.Div64(high, low, totalByteCount)
		shares[networkId] = NanoPoints(share)
		allocated += share
		remainders = append(remainders, remainder{networkId: networkId, value: rest})
	}
	// stable, so equal remainders keep the id order
	slices.SortStableFunc(remainders, func(a remainder, b remainder) int {
		return cmp.Compare(b.value, a.value)
	})
	for i := range uint64(totalNanoPoints) - allocated {
		shares[remainders[i].networkId] += 1
	}
	return shares
}

// The inputs of the points rule of one award: its payout points, its provider
// networks' bytes, and the bonuses' inputs.
type payoutPointsPlan struct {
	ctx           context.Context
	subsidyConfig *SubsidyConfig
	paymentPlanId server.Id
	// account_points_per_payout scaled to the window: the payout points the
	// networks share by bytes
	payoutNanoPoints NanoPoints
	// each provider network's bytes served in the window, all positive. A
	// network's id is also the placeholder id of its payment, until a commit
	// assigns the row's id.
	networkByteCounts map[server.Id]ByteCount
	// each network's reliability points; only a network with bytes takes them
	reliabilityPoints map[server.Id]NanoPoints
	seekerHolders     map[server.Id]bool
	// network -> the network that referred it
	referralParents map[server.Id]server.Id
	// network -> the networks it referred
	networkReferrals map[server.Id][]server.Id
}

// The points rule, without writing anything:
//   - payout points: the payout points shared exactly in proportion to bytes
//     (operatorBlockPayoutPointShares)
//   - reliability points of each network with bytes
//   - the seeker multiplier, which multiplies a seeker holder's payout points
//     and gives its reliability points again
//   - the referral parent's fraction and the referral children's halving
//     fractions of the payout points, after the multiplier
//
// Bytes, reliability evidence, seeker holdings and referrals are the only
// inputs: no revenue, and nothing about whether the traffic was paid. Networks
// are visited in id order, so the rows are the same for the same inputs, and a
// row of no points is left out.
func planPayoutPoints(plan *payoutPointsPlan) []ApplyAccountPointsArgs {
	queuedPoints := []ApplyAccountPointsArgs{}
	queuePoints := func(args ApplyAccountPointsArgs) {
		if 0 < args.PointValue {
			queuedPoints = append(queuedPoints, args)
		}
	}

	networkIds := slices.SortedFunc(maps.Keys(plan.networkByteCounts), server.Id.Cmp)
	payoutPoints := operatorBlockPayoutPointShares(plan.payoutNanoPoints, plan.networkByteCounts)

	for _, networkId := range networkIds {
		paymentId := networkId
		queuePoints(ApplyAccountPointsArgs{
			NetworkId:        networkId,
			Event:            AccountPointEventReliability,
			PointValue:       plan.reliabilityPoints[networkId],
			AccountPaymentId: &paymentId,
			PaymentPlanId:    &plan.paymentPlanId,
		})
	}

	for _, networkId := range networkIds {
		paymentId := networkId
		points := payoutPoints[networkId]
		queuePoints(ApplyAccountPointsArgs{
			NetworkId:        networkId,
			Event:            AccountPointEventPayout,
			PointValue:       points,
			PaymentPlanId:    &plan.paymentPlanId,
			AccountPaymentId: &paymentId,
		})

		if plan.seekerHolders[networkId] {
			multipliedPoints := NanoPoints(float64(points) * plan.subsidyConfig.SeekerHolderMultiplier)
			queuePoints(ApplyAccountPointsArgs{
				NetworkId:        networkId,
				Event:            AccountPointEventPayoutMultiplier,
				PointValue:       multipliedPoints - points + plan.reliabilityPoints[networkId],
				PaymentPlanId:    &plan.paymentPlanId,
				AccountPaymentId: &paymentId,
			})
			points = multipliedPoints
		}

		if parentNetworkId, ok := plan.referralParents[networkId]; ok && parentNetworkId != networkId {
			linkedNetworkId := networkId
			queuePoints(ApplyAccountPointsArgs{
				NetworkId:        parentNetworkId,
				Event:            AccountPointEventPayoutLinkedAccount,
				PointValue:       NanoPoints(float64(points) * plan.subsidyConfig.ReferralParentPayoutFraction),
				PaymentPlanId:    &plan.paymentPlanId,
				LinkedNetworkId:  &linkedNetworkId,
				AccountPaymentId: &paymentId,
			})
		}

		visited := map[server.Id]struct{}{}
		payoutChildrenReferralNetworksInTx(
			plan.ctx,
			queuePoints,
			points,
			plan.subsidyConfig.ReferralChildPayoutFraction,
			networkId,
			plan.networkReferrals,
			plan.paymentPlanId,
			&paymentId,
			nil,
			0,
			visited,
		)
	}

	return queuedPoints
}
