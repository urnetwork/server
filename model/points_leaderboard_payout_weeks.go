package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// The payout-week fallback of the points leaderboard (owner decision
// 2026-10-28). Blocks and Streak publish as soon as a completed operator
// block exists; they no longer wait for the history rollup
// (AdvancePointsBlockRollup) to measure every payout's paid sweeps.
//
// A payment's points earn in:
//   - its stored account_payment_block rows, written by the payout plan or the
//     rollup. They are exact, and a rolled-up payment keeps only them;
//   - its payout weeks (pointsPayoutWeeks) while the rollup has not finished
//     it, together with any rows the rollup has stored so far;
//   - its payout weeks for good when the rollup measured a completed payment
//     after contract reaping had deleted its sweeps: every sweep it paid was
//     in a block, so its missing rows are unknown, not empty;
//   - for a positive point without a payment row, the last block that closed
//     before the point posted.
//
// Pending and canceled payments follow the exact rule. A pending payment
// keeps its sweeps (contract retention holds them), so the rollup measures it
// later and its rows replace its payout weeks on the next rebuild. A canceled
// payment earns only while it still owns a paid sweep: once a later plan
// re-selected its sweeps, or a legacy correction moved them, it earns in no
// block, as the rollup would measure it. A payment that paid no bytes paid no
// traffic.
//
// A network's Blocks are a set: a block counts once however many payments or
// sources earn in it, so stored rows and payout weeks never double count, and
// each payment's points are summed once per block.

// pointsPayoutWeeks is the payout-week rule: the operator blocks [first, last]
// that a payout's points earn in while its paid sweeps are unmeasured, every
// block that overlaps [min_sweep_time, end of its traffic window). The window
// ends at its plan's subsidy epoch end (subsidy_payment.end_time), the close
// bound of the slice that selected its sweeps; a plan without one ends at the
// start of the block it was planned in. The oldest sweep's block always earns,
// so a payout that paid traffic in a block never publishes a false zero. ok is
// false for a payout of traffic before the genesis only.
//
// Exact blocks start at the oldest paid sweep and end inside the traffic
// window, so this is an upper bound of them. Measured on Main 2026-10-28, it
// contained the exact blocks of all 1,411 rolled-up payments with rows and of
// all 7,191 not-yet-rolled payments whose sweeps were read directly, was exact
// for 91% of payouts without a backlog (min_sweep_time inside the window),
// and left no gap or overlap where a network's exact payments meet its payout
// weeks. Of the 154 networks with both, a single block at the plan time or the
// block before it left such a gap for 106 and 107, and the oldest sweep's block
// alone for 71: a payout that repays re-planned sweeps starts in a block an
// exact payment already holds. Blocks overlapping [min_sweep_time,
// create_time) left no gap either but credited more unpaid blocks (precision
// 0.35 against 0.57).
//
// pointsPayoutFirstBlockSql and pointsPayoutLastBlockSql are the same rule in
// SQL; TestPointsPayoutWeeksSqlMatchesRule keeps them equal.
func pointsPayoutWeeks(minSweepTime time.Time, windowEnd *time.Time, createTime time.Time) (first int64, last int64, ok bool) {
	oldest := pointsBlockOf(minSweepTime)
	var windowLast int64
	if windowEnd != nil {
		// the block of the window's last instant: the end is exclusive
		windowLast = pointsBlockOf(windowEnd.Add(-time.Microsecond))
	} else {
		windowLast = pointsBlockOf(createTime) - 1
	}
	last = max(oldest, windowLast)
	first = max(1, oldest)
	if last < first {
		return 0, 0, false
	}
	return first, last, true
}

// pointsBlockOf is the 1-based operator block containing t: zero or negative
// before the genesis. Timestamps are microseconds, as PostgreSQL stores them.
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

// The block of the last instant before the timestamp expression: the last
// block an exclusive end reaches.
func pointsBlockBeforeSql(timestamp string, genesis string, blockSeconds string) string {
	return "(ceil(extract(epoch FROM (" + timestamp + " - " + genesis + "::timestamp)) / " + blockSeconds + "::numeric)::bigint)"
}

// The first payout block of the payment alias (pointsPayoutWeeks).
func pointsPayoutFirstBlockSql(payment string, genesis string, blockSeconds string) string {
	return "greatest(1, " + pointsBlockOfSql(payment+".min_sweep_time", genesis, blockSeconds) + ")"
}

// The last payout block of the payment alias with its plan's subsidy epoch
// alias, which may be null (pointsPayoutWeeks).
func pointsPayoutLastBlockSql(payment string, epoch string, genesis string, blockSeconds string) string {
	return "greatest(" + pointsBlockOfSql(payment+".min_sweep_time", genesis, blockSeconds) + ", CASE WHEN " + epoch + ".end_time IS NULL THEN " +
		pointsBlockOfSql(payment+".create_time", genesis, blockSeconds) + " - 1 ELSE " +
		pointsBlockBeforeSql(epoch+".end_time", genesis, blockSeconds) + " END)"
}

// The payment alias earns in its payout weeks. `genesis` names the statement's
// parameter for SubnetBlockGenesis.
func pointsPayoutWeekPaymentSql(payment string, genesis string) string {
	return payment + `.payout_byte_count > 0 AND (
			-- the rollup has not finished it; a canceled payment earns only
			-- while it still owns a paid sweep, as the rollup would measure it
			(NOT ` + payment + `.block_rollup_complete AND (
				NOT ` + payment + `.canceled OR EXISTS (
					SELECT 1 FROM transfer_escrow_sweep AS owned
					WHERE owned.network_id = ` + payment + `.network_id
						AND owned.payment_id = ` + payment + `.payment_id
						AND ` + pointsPaidSweepSql("owned") + `
				)
			))
			OR
			-- the rollup measured a completed payment after reaping had
			-- deleted its sweeps: all of them were in blocks, so it measured
			-- none of its rows
			(` + payment + `.block_rollup_complete AND ` + payment + `.completed AND
				` + payment + `.min_sweep_time >= ` + genesis + `::timestamp AND
				NOT EXISTS (
					SELECT 1 FROM account_payment_block AS measured
					WHERE measured.payment_id = ` + payment + `.payment_id
				))
		)`
}

// Every payment with a positive point posted since the genesis: the payments
// the rollup visits.
func pointsPositivePaymentSql(genesis string) string {
	return `
		SELECT positive.account_payment_id
		FROM account_point AS positive
		WHERE positive.account_payment_id IS NOT NULL
			AND positive.point_value > 0
			AND positive.create_time >= ` + genesis + `::timestamp
	`
}

// A point posted since the genesis without a payment row.
func pointsUnlinkedPointSql(point string, genesis string) string {
	return point + `.create_time >= ` + genesis + `::timestamp AND
		NOT EXISTS (
			SELECT 1 FROM account_payment AS linked
			WHERE linked.payment_id = ` + point + `.account_payment_id
		)`
}

// The block of an unlinked point: the last one that closed before it posted.
func pointsUnlinkedBlockSql(point string, genesis string, blockSeconds string) string {
	return "(" + pointsBlockOfSql(point+".create_time", genesis, blockSeconds) + " - 1)"
}

// The payments that earn in their payout weeks, with those weeks. `blocks`
// names the statement's parameter for the completed block numbers; a payment
// whose weeks are all outside them is left out.
func pointsPayoutWeekPaymentCte(blocks string, genesis string, blockSeconds string) string {
	return `
		payout_week_payment AS MATERIALIZED (
			SELECT
				payment.payment_id,
				payment.block_rollup_complete,
				` + pointsPayoutFirstBlockSql("payment", genesis, blockSeconds) + ` AS first_block,
				` + pointsPayoutLastBlockSql("payment", "plan_epoch", genesis, blockSeconds) + ` AS last_block
			FROM account_payment AS payment
			LEFT JOIN subsidy_payment AS plan_epoch ON plan_epoch.payment_plan_id = payment.payment_plan_id
			WHERE payment.payment_id IN (` + pointsPositivePaymentSql(genesis) + `)
				AND ` + pointsPayoutWeekPaymentSql("payment", genesis) + `
		), payout_week AS MATERIALIZED (
			SELECT payout_week_payment.payment_id, week.block_number
			FROM payout_week_payment
			CROSS JOIN LATERAL generate_series(
				payout_week_payment.first_block,
				payout_week_payment.last_block
			) AS week(block_number)
			WHERE week.block_number = ANY(` + blocks + `::bigint[])
		)
	`
}

// Every (block, network) of the completed blocks $1 in which the network
// earned points: from stored rows, payout weeks and unlinked points. $2 is the
// genesis and $3 the block seconds. A payment's stored rows and payout weeks
// are one set, so each of its points is summed once per block.
var pointsNetworkBlocksSql = `
	WITH ` + pointsPayoutWeekPaymentCte("$1", "$2", "$3") + `,
	payment_block AS (
		SELECT measured.payment_id, measured.block_number
		FROM account_payment_block AS measured
		WHERE measured.block_number = ANY($1::bigint[])
		UNION
		SELECT payout_week.payment_id, payout_week.block_number
		FROM payout_week
	), point_block AS (
		SELECT payment_block.block_number, account_point.network_id, account_point.point_value
		FROM payment_block
		INNER JOIN account_point ON account_point.account_payment_id = payment_block.payment_id
		UNION ALL
		SELECT ` + pointsUnlinkedBlockSql("account_point", "$2", "$3") + `, account_point.network_id, account_point.point_value
		FROM account_point
		WHERE ` + pointsUnlinkedPointSql("account_point", "$2") + `
			AND ` + pointsUnlinkedBlockSql("account_point", "$2", "$3") + ` = ANY($1::bigint[])
	)
	SELECT point_block.block_number, point_block.network_id
	FROM point_block
	GROUP BY point_block.block_number, point_block.network_id
	HAVING 0 < SUM(point_block.point_value)
`

// How much of a snapshot of the completed blocks $1 is approximate. $2 is the
// genesis and $3 the block seconds.
var pointsLeaderboardApproximationSql = `
	WITH ` + pointsPayoutWeekPaymentCte("$1", "$2", "$3") + `,
	approximate_payment AS MATERIALIZED (
		SELECT payout_week_payment.payment_id, payout_week_payment.block_rollup_complete
		FROM payout_week_payment
		WHERE EXISTS (
			SELECT 1 FROM payout_week
			WHERE payout_week.payment_id = payout_week_payment.payment_id
		)
	), unlinked_point AS MATERIALIZED (
		SELECT account_point.network_id
		FROM account_point
		WHERE account_point.point_value > 0
			AND ` + pointsUnlinkedPointSql("account_point", "$2") + `
			AND ` + pointsUnlinkedBlockSql("account_point", "$2", "$3") + ` = ANY($1::bigint[])
	)
	SELECT
		(SELECT count(*) FROM approximate_payment WHERE NOT block_rollup_complete),
		(SELECT count(*) FROM approximate_payment WHERE block_rollup_complete),
		(SELECT count(*) FROM unlinked_point),
		(SELECT count(*) FROM (
			SELECT account_point.network_id
			FROM approximate_payment
			INNER JOIN account_point ON account_point.account_payment_id = approximate_payment.payment_id
			WHERE account_point.point_value > 0
			UNION
			SELECT unlinked_point.network_id FROM unlinked_point
		) AS approximate_network),
		(SELECT count(*) FROM account_payment AS pending
			WHERE NOT pending.block_rollup_complete
				AND pending.payment_id IN (` + pointsPositivePaymentSql("$2") + `))
`

// PointsLeaderboardApproximation counts the inputs of a snapshot's Blocks and
// Streak that earn in payout weeks instead of stored block rows. The snapshot
// stores none of it and the API shape is unchanged: the rebuild logs it with
// the snapshot id and exports it as gauges.
type PointsLeaderboardApproximation struct {
	// payments whose payout weeks reach a completed block because the rollup
	// has not finished them; their rows replace the weeks as it does
	RollupPendingPayments int64
	// completed payments the rollup measured after contract reaping had
	// deleted their sweeps; their payout weeks stay
	ReapedPayments int64
	// positive point rows without a payment row, each in the last block that
	// closed before it posted
	UnlinkedPoints int64
	// networks with a positive point among those inputs
	ApproximateNetworks int64
	// payments with a positive post-genesis point that the rollup has not
	// finished: its remaining work, in any block
	BackfillPendingPayments int64
}

// The count of payments whose payout weeks the snapshot uses.
func (self PointsLeaderboardApproximation) PayoutWeekPayments() int64 {
	return self.RollupPendingPayments + self.ReapedPayments
}

func loadPointsLeaderboardApproximation(ctx context.Context, windows []PointsEpochWindow) (approximation PointsLeaderboardApproximation, returnErr error) {
	epochs := make([]int64, len(windows))
	for i, window := range windows {
		epochs[i] = int64(window.Epoch)
	}
	// stats read: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			pointsLeaderboardApproximationSql,
			epochs,
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
				&approximation.RollupPendingPayments,
				&approximation.ReapedPayments,
				&approximation.UnlinkedPoints,
				&approximation.ApproximateNetworks,
				&approximation.BackfillPendingPayments,
			)
		})
	})
	return
}
