package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// The payout-week fallback of the points leaderboard (owner decision
// 2026-10-28). Blocks and Streak are published as soon as completed operator
// blocks exist; they no longer wait for the history rollup
// (AdvancePointsBlockRollup) to measure every payout's paid sweeps.
//
// A point earns in the operator blocks of its payment:
//   - the payment's measured account_payment_block rows, when the payout plan
//     or the rollup wrote them;
//   - its payout weeks (pointsPayoutBlocks) when nothing was measured: the
//     rollup has not reached the payment yet, or it measured a completed
//     payment after contract reaping had deleted the payment's sweeps. A
//     completed payment that paid bytes from sweeps after the genesis cannot
//     have earned in no block, so its missing rows are unknown, not empty;
//   - the last block that closed before the point posted, for a positive
//     point without a payment row.
//
// A canceled payment counts as the rollup counts it: with the paid sweeps it
// still owns when measured. A legacy correction that moved all of a canceled
// original's sweeps to its replacement (no anchor journaled) leaves the
// original none, so the fallback gives it no blocks either. A pending payment
// keeps its sweeps (contract retention holds them), so the rollup measures it
// later and its rows replace its payout weeks on the next rebuild.
//
// A network's Blocks are a set: a block counts once however many payments or
// sources earn in it, so measured rows and payout weeks never double count.

// pointsPayoutBlocks is the payout-week rule: the blocks [first, last] that a
// payout's points earn in while its paid sweeps are unmeasured. ok is false
// for a payout planned before the genesis, which paid no block's traffic.
//
// A payout's oldest selected sweep (min_sweep_time) starts its traffic, so the
// block of that sweep earns. A payout planned on time pays the slice that
// followed the network's previous payout: when its oldest sweep is in the last
// block that closed before it was planned (create_time), or in the block
// before that one, it earns in every block from there through that last closed
// block. An older oldest sweep marks a backlog payout (withheld dust,
// re-planned or legacy sweeps), whose later blocks stay unknown until the
// rollup measures them; only its first block earns. The block the payout was
// planned in earns only when its oldest sweep is in it: crediting the plan's
// block shifts every payout a week late. An oldest sweep before the genesis
// starts at block 1.
//
// pointsPayoutFirstBlockSql and pointsPayoutLastBlockSql are the same rule in
// SQL; TestPointsPayoutBlocksSqlMatchesRule keeps them equal.
func pointsPayoutBlocks(minSweepTime time.Time, createTime time.Time) (first int64, last int64, ok bool) {
	if createTime.Before(SubnetBlockGenesis) {
		return 0, 0, false
	}
	first = max(1, pointsBlockOf(minSweepTime))
	closed := pointsBlockOf(createTime) - 1
	last = first
	if closed-first <= 1 {
		last = max(first, closed)
	}
	return first, last, true
}

// pointsBlockOf is the 1-based operator block containing t: zero or negative
// before the genesis.
func pointsBlockOf(t time.Time) int64 {
	elapsed := t.Sub(SubnetBlockGenesis)
	blocks := int64(elapsed / SubnetBlockDuration)
	if elapsed < 0 && elapsed%SubnetBlockDuration != 0 {
		blocks -= 1
	}
	return blocks + 1
}

// The 1-based block containing the timestamp expression, as pointsBlockOf.
// `genesis` and `blockSeconds` name the statement's parameters for
// SubnetBlockGenesis and SubnetBlockDuration in seconds.
func pointsBlockOfSql(timestamp string, genesis string, blockSeconds string) string {
	return "(1 + floor(extract(epoch FROM (" + timestamp + " - " + genesis + "::timestamp)) / " + blockSeconds + "::numeric)::bigint)"
}

// The first payout block of the payment alias (pointsPayoutBlocks).
func pointsPayoutFirstBlockSql(payment string, genesis string, blockSeconds string) string {
	return "greatest(1, " + pointsBlockOfSql(payment+".min_sweep_time", genesis, blockSeconds) + ")"
}

// The last payout block of the payment alias (pointsPayoutBlocks).
func pointsPayoutLastBlockSql(payment string, genesis string, blockSeconds string) string {
	first := pointsPayoutFirstBlockSql(payment, genesis, blockSeconds)
	closed := "(" + pointsBlockOfSql(payment+".create_time", genesis, blockSeconds) + " - 1)"
	return "(CASE WHEN " + closed + " - " + first + " <= 1 THEN greatest(" + first + ", " + closed + ") ELSE " + first + " END)"
}

// The payment alias has no measured blocks and earns in its payout weeks. A
// payment that paid no bytes paid no traffic.
func pointsPayoutWeekPaymentSql(payment string, genesis string) string {
	return payment + `.payout_byte_count > 0 AND
		` + payment + `.create_time >= ` + genesis + `::timestamp AND (
			-- the rollup has not measured it
			(NOT ` + payment + `.block_rollup_complete AND
				NOT (` + payment + `.canceled AND ` + legacyProviderPaymentSweepsMovedSql(payment+".payment_id") + `))
			OR
			-- the rollup measured a completed payment whose sweeps reaping had
			-- already deleted
			(` + payment + `.block_rollup_complete AND ` + payment + `.completed AND
				` + payment + `.min_sweep_time >= ` + genesis + `::timestamp AND
				NOT EXISTS (
					SELECT 1 FROM account_payment_block AS measured
					WHERE measured.payment_id = ` + payment + `.payment_id
				))
		)`
}

// Every payment with a positive point posted since the genesis.
func pointsPositivePaymentSql(genesis string) string {
	return `
		SELECT positive.account_payment_id
		FROM account_point AS positive
		WHERE positive.account_payment_id IS NOT NULL
			AND positive.point_value > 0
			AND positive.create_time >= ` + genesis + `::timestamp
	`
}

// A positive point posted since the genesis without a payment row.
func pointsUnlinkedPointSql(point string, genesis string) string {
	return point + `.point_value > 0 AND
		` + point + `.create_time >= ` + genesis + `::timestamp AND
		NOT EXISTS (
			SELECT 1 FROM account_payment AS linked
			WHERE linked.payment_id = ` + point + `.account_payment_id
		)`
}

// The block of an unlinked point: the last one that closed before it posted.
func pointsUnlinkedBlockSql(point string, genesis string, blockSeconds string) string {
	return "(" + pointsBlockOfSql(point+".create_time", genesis, blockSeconds) + " - 1)"
}

// The payments that earn in their payout weeks, with those weeks.
func pointsPayoutWeekPaymentCte(genesis string, blockSeconds string) string {
	return `
		payout_week_payment AS MATERIALIZED (
			SELECT
				payment.payment_id,
				payment.block_rollup_complete,
				` + pointsPayoutFirstBlockSql("payment", genesis, blockSeconds) + ` AS first_block,
				` + pointsPayoutLastBlockSql("payment", genesis, blockSeconds) + ` AS last_block
			FROM account_payment AS payment
			WHERE payment.payment_id IN (` + pointsPositivePaymentSql(genesis) + `)
				AND ` + pointsPayoutWeekPaymentSql("payment", genesis) + `
		)
	`
}

// Every (block, network) of the completed blocks $1 in which the network
// earned points: from measured rows, payout weeks and unlinked points. $2 is
// the genesis and $3 the block seconds. A payment's measured rows and payout
// weeks are one set, so each of its points is summed once per block.
var pointsNetworkBlocksSql = `
	WITH ` + pointsPayoutWeekPaymentCte("$2", "$3") + `,
	payment_block AS (
		SELECT measured.payment_id, measured.block_number
		FROM account_payment_block AS measured
		WHERE measured.block_number = ANY($1::bigint[])
		UNION
		SELECT payout_week_payment.payment_id, week.block_number
		FROM payout_week_payment
		CROSS JOIN LATERAL generate_series(
			payout_week_payment.first_block,
			payout_week_payment.last_block
		) AS week(block_number)
		WHERE week.block_number = ANY($1::bigint[])
	)
	SELECT payment_block.block_number, account_point.network_id
	FROM payment_block
	INNER JOIN account_point ON account_point.account_payment_id = payment_block.payment_id
	GROUP BY payment_block.block_number, account_point.network_id
	HAVING 0 < SUM(account_point.point_value)

	UNION

	SELECT ` + pointsUnlinkedBlockSql("account_point", "$2", "$3") + `, account_point.network_id
	FROM account_point
	WHERE ` + pointsUnlinkedPointSql("account_point", "$2") + `
		AND ` + pointsUnlinkedBlockSql("account_point", "$2", "$3") + ` = ANY($1::bigint[])
	GROUP BY 1, 2
`

// How much of a snapshot is approximate. $1 is the genesis and $2 the block
// seconds.
var pointsLeaderboardApproximationSql = `
	WITH ` + pointsPayoutWeekPaymentCte("$1", "$2") + `,
	payout_week_point AS MATERIALIZED (
		SELECT account_point.network_id
		FROM account_point
		INNER JOIN payout_week_payment ON
			payout_week_payment.payment_id = account_point.account_payment_id
		WHERE account_point.point_value > 0
	), unlinked_point AS MATERIALIZED (
		SELECT account_point.network_id
		FROM account_point
		WHERE ` + pointsUnlinkedPointSql("account_point", "$1") + `
	)
	SELECT
		(SELECT count(*) FROM payout_week_payment),
		(SELECT count(*) FROM payout_week_payment WHERE block_rollup_complete),
		(SELECT count(*) FROM payout_week_point),
		(SELECT count(*) FROM unlinked_point),
		(SELECT count(*) FROM (
			SELECT network_id FROM payout_week_point
			UNION
			SELECT network_id FROM unlinked_point
		) AS approximate_network),
		(SELECT count(*) FROM account_payment AS pending
			WHERE NOT pending.block_rollup_complete
				AND pending.payment_id IN (` + pointsPositivePaymentSql("$1") + `))
`

// PointsLeaderboardApproximation counts the inputs of a snapshot that earn in
// payout weeks instead of measured block rows. The snapshot stores none of it;
// the rebuild logs it and exports it as gauges.
type PointsLeaderboardApproximation struct {
	// payments whose points earn in their payout weeks
	PayoutWeekPayments int64
	// of those, completed payments the rollup measured after reaping; the
	// rollup cannot replace their payout weeks
	UnmeasuredPayments int64
	// positive point rows of the payout-week payments
	PayoutWeekPoints int64
	// positive point rows without a payment row
	UnlinkedPoints int64
	// networks with any payout-week or unlinked point
	ApproximateNetworks int64
	// payments with positive post-genesis points that the rollup has not
	// finished: the backfill's remaining work
	BackfillPendingPayments int64
}

func loadPointsLeaderboardApproximation(ctx context.Context) (approximation PointsLeaderboardApproximation, returnErr error) {
	// stats read: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			pointsLeaderboardApproximationSql,
			SubnetBlockGenesis,
			int64(SubnetBlockDuration/time.Second),
		)
		server.WithPgResult(result, err, func() {
			if err != nil {
				returnErr = err
				return
			}
			if !result.Next() {
				returnErr = fmt.Errorf("points leaderboard approximation query returned no row")
				return
			}
			returnErr = result.Scan(
				&approximation.PayoutWeekPayments,
				&approximation.UnmeasuredPayments,
				&approximation.PayoutWeekPoints,
				&approximation.UnlinkedPoints,
				&approximation.ApproximateNetworks,
				&approximation.BackfillPendingPayments,
			)
		})
	})
	return
}
