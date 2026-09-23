package monitor

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// SIGNALS.md §17.6 keeps total-point publication separate from the completed
// operator payout-block rollup. ST configuration and chain epochs are unrelated.
func NewPointsReadinessSignal() Signal {
	return &signalAdapter{
		number: "17.6",
		key:    "points-readiness",
		name:   "Points leaderboard operator-block readiness",
		probe:  pointsReadinessProbe{},
	}
}

type pointsReadinessProbe struct{}

func (self pointsReadinessProbe) id() string             { return "pg/points-readiness" }
func (self pointsReadinessProbe) tier() string           { return tierWarn }
func (self pointsReadinessProbe) cadence() time.Duration { return 5 * time.Minute }

const pointsSnapshotQuery = `
	WITH latest AS (
		SELECT snapshot_id, create_time, latest_epoch, total_ranked, epoch_metrics_available
		FROM network_points_leaderboard_snapshot
		ORDER BY create_time DESC, snapshot_id DESC
		LIMIT 1
	)
	SELECT round(extract(epoch FROM
	         (clock_timestamp() AT TIME ZONE 'UTC') - latest.create_time))::bigint,
	       latest.latest_epoch,
	       latest.total_ranked,
	       latest.epoch_metrics_available,
	       count(ranked.network_id),
	       count(*) FILTER (WHERE ranked.blocks_with_points > 0),
	       count(*) FILTER (WHERE ranked.streak > 0),
	       count(*) FILTER (WHERE ranked.longest_streak > 0)
	FROM latest
	LEFT JOIN network_points_leaderboard AS ranked
	  ON ranked.snapshot_id = latest.snapshot_id
	GROUP BY latest.snapshot_id, latest.create_time, latest.latest_epoch,
	         latest.total_ranked, latest.epoch_metrics_available;
`

const pointsSnapshotMaxAge = 2 * time.Hour

// One output row, bounded by the monitor command deadline. The eligibility
// predicate matches model.pointsBlockRollupComplete, including current-period
// positive points. Pending counts are point rows, not payments or contracts.
func pointsOperatorSourceQuery() string {
	return fmt.Sprintf(`
		WITH lifecycle_clock AS MATERIALIZED (
			SELECT clock_timestamp() AT TIME ZONE 'UTC' AS now_utc
		), calendar AS (
			SELECT greatest(0, floor(extract(epoch FROM
				(now_utc - timestamp '%[1]s')) / %[2]d))::bigint AS closed_blocks
			FROM lifecycle_clock
		), rollup AS (
			SELECT count(*) FILTER (WHERE point.account_payment_id IS NULL
					OR payment.payment_id IS NULL) AS missing_payment_points,
				count(*) FILTER (WHERE payment.payment_id IS NOT NULL
					AND NOT payment.block_rollup_complete) AS pending_rollup_points,
				coalesce(max(floor(extract(epoch FROM
					lifecycle_clock.now_utc - point.create_time))) FILTER (
					WHERE payment.payment_id IS NOT NULL
						AND NOT payment.block_rollup_complete), 0)::bigint AS oldest_pending_age
			FROM account_point AS point
			LEFT JOIN account_payment AS payment
				ON payment.payment_id = point.account_payment_id
			CROSS JOIN lifecycle_clock
			WHERE point.create_time >= timestamp '%[1]s'
				AND point.point_value > 0
		)
		SELECT calendar.closed_blocks,
			CASE WHEN calendar.closed_blocks = 0 THEN 0 ELSE
				floor(extract(epoch FROM lifecycle_clock.now_utc -
					(timestamp '%[1]s' + calendar.closed_blocks * interval '%[2]d seconds')))
			END::bigint AS latest_close_age,
			rollup.missing_payment_points, rollup.pending_rollup_points,
			rollup.oldest_pending_age
		FROM calendar CROSS JOIN lifecycle_clock CROSS JOIN rollup;
	`, model.SubnetBlockGenesis.UTC().Format("2006-01-02 15:04:05"), int64(model.SubnetBlockDuration/time.Second))
}

type pointsSnapshotState struct {
	present         bool
	ageSeconds      int64
	latestEpoch     uint64
	totalRanked     uint64
	epochAvailable  bool
	rowCount        uint64
	positiveBlocks  uint64
	positiveStreak  uint64
	positiveLongest uint64
}

type pointsOperatorSourceState struct {
	closedBlocks     uint64
	latestCloseAge   int64
	missingPoints    uint64
	pendingPoints    uint64
	oldestPendingAge int64
}

func parsePointsSigned(row pgRow, column int, name string) (int64, error) {
	if len(row) <= column {
		return 0, fmt.Errorf("points readiness row is missing %s", name)
	}
	value, err := strconv.ParseInt(row.str(column), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("points readiness has invalid %s", name)
	}
	return value, nil
}

func parsePointsUnsigned(row pgRow, column int, name string) (uint64, error) {
	if len(row) <= column {
		return 0, fmt.Errorf("points readiness row is missing %s", name)
	}
	value, err := strconv.ParseUint(row.str(column), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("points readiness has invalid %s", name)
	}
	return value, nil
}

func parsePointsSnapshot(rows []pgRow) (pointsSnapshotState, error) {
	if len(rows) == 0 {
		return pointsSnapshotState{}, nil
	}
	if len(rows) != 1 || len(rows[0]) != 8 {
		return pointsSnapshotState{}, fmt.Errorf("points snapshot query returned an invalid shape")
	}
	state := pointsSnapshotState{present: true}
	var err error
	if state.ageSeconds, err = parsePointsSigned(rows[0], 0, "snapshot age"); err != nil {
		return pointsSnapshotState{}, err
	}
	if state.epochAvailable, err = strconv.ParseBool(rows[0].str(3)); err != nil {
		return pointsSnapshotState{}, fmt.Errorf("points readiness has invalid epoch-metrics availability")
	}
	values := []struct {
		name   string
		column int
		dest   *uint64
	}{
		{name: "latest block", column: 1, dest: &state.latestEpoch},
		{name: "total ranked", column: 2, dest: &state.totalRanked},
		{name: "snapshot row count", column: 4, dest: &state.rowCount},
		{name: "positive blocks count", column: 5, dest: &state.positiveBlocks},
		{name: "positive streak count", column: 6, dest: &state.positiveStreak},
		{name: "positive longest-streak count", column: 7, dest: &state.positiveLongest},
	}
	for _, item := range values {
		if *item.dest, err = parsePointsUnsigned(rows[0], item.column, item.name); err != nil {
			return pointsSnapshotState{}, err
		}
	}
	if state.rowCount < state.positiveBlocks || state.rowCount < state.positiveStreak || state.rowCount < state.positiveLongest {
		return pointsSnapshotState{}, fmt.Errorf("points snapshot positive counts exceed its row count")
	}
	return state, nil
}

func parsePointsOperatorSource(rows []pgRow) (pointsOperatorSourceState, error) {
	if len(rows) != 1 || len(rows[0]) != 5 {
		return pointsOperatorSourceState{}, fmt.Errorf("points operator source query returned an invalid shape")
	}
	state := pointsOperatorSourceState{}
	var err error
	for _, item := range []struct {
		name   string
		column int
		dest   *uint64
	}{
		{name: "closed operator blocks", column: 0, dest: &state.closedBlocks},
		{name: "missing payment point count", column: 2, dest: &state.missingPoints},
		{name: "pending rollup point count", column: 3, dest: &state.pendingPoints},
	} {
		if *item.dest, err = parsePointsUnsigned(rows[0], item.column, item.name); err != nil {
			return pointsOperatorSourceState{}, err
		}
	}
	if state.latestCloseAge, err = parsePointsSigned(rows[0], 1, "latest operator close age"); err != nil {
		return pointsOperatorSourceState{}, err
	}
	if state.oldestPendingAge, err = parsePointsSigned(rows[0], 4, "oldest pending point age"); err != nil {
		return pointsOperatorSourceState{}, err
	}
	if state.latestCloseAge < 0 || int64(model.SubnetBlockDuration/time.Second) <= state.latestCloseAge ||
		(state.closedBlocks == 0 && state.latestCloseAge != 0) ||
		(state.pendingPoints == 0 && state.oldestPendingAge != 0) {
		return pointsOperatorSourceState{}, fmt.Errorf("points operator source has contradictory count or age evidence")
	}
	return state, nil
}

func pointsReadinessTarget(env *probeEnv) string {
	if host := env.cfg.hostByRole("pg-primary"); host != nil {
		return host.name
	}
	return "pg"
}

// Field/class names retain compatibility, but epoch now means the 1-based
// operator payout block. No ST identity is read, compared, or rendered.
func pointsOperatorFinding(target, tier, class, symptom, mechanism, action string, snapshot pointsSnapshotState, source pointsOperatorSourceState) finding {
	return finding{
		probeId: "pg/points-readiness", tier: tier, class: class, target: target, sustain: 1,
		symptom: symptom, mechanism: mechanism,
		baseline: "Blocks and Streak use completed Sunday-00 UTC seven-day operator payout periods and a complete paid-traffic rollup. ST state is not a prerequisite. The compatibility field latest_epoch is the 1-based operator block, and epoch_metrics_available describes the immutable snapshot's input boundary.",
		observed: fmt.Sprintf(
			"source=operator-paid-traffic completed_operator_blocks=%d latest_close_age_seconds=%d missing_payment_points=%d pending_rollup_points=%d oldest_pending_point_age_seconds=%d rollup_completion_age=unknown snapshot_epoch_metrics_available=%t snapshot_latest_epoch=%d snapshot_age_seconds=%d total_ranked=%d positive_blocks=%d positive_streak=%d positive_longest_streak=%d",
			source.closedBlocks, source.latestCloseAge, source.missingPoints, source.pendingPoints,
			source.oldestPendingAge, snapshot.epochAvailable, snapshot.latestEpoch, snapshot.ageSeconds,
			snapshot.totalRanked, snapshot.positiveBlocks, snapshot.positiveStreak, snapshot.positiveLongest,
		),
		evidence: "Two bounded read-only PostgreSQL statements returned a snapshot header/row census and UTC operator-calendar/positive-point rollup census. Only counts, ages, block numbers and booleans are rendered; no network or payment identity is returned.",
		context:  "Rollup completion has no persisted completion timestamp. A new pending payment can follow a valid immutable snapshot, and a just-completed backfill can precede a still-unavailable snapshot. Current source state alone cannot prove either snapshot corruption or an overdue completion-to-rebuild handoff.",
		action:   action,
		verify:   "After the bounded normal rollup and rebuild complete, a fresh internally consistent snapshot records the latest completed operator block with availability true. Meaningful zero Blocks/Streak values remain valid. Do not edit snapshot bits, payment completion flags or rollup rows by hand, and do not enable ST to repair this plane.",
		playbook: "SIGNALS.md §17.6",
	}
}

func (self pointsReadinessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := pointsReadinessTarget(env)
	snapshotRows, err := env.runner.pg(ctx, pointsSnapshotQuery)
	if err != nil {
		return nil, err
	}
	snapshot, err := parsePointsSnapshot(snapshotRows)
	if err != nil {
		return nil, err
	}
	if !snapshot.present {
		return []finding{{
			probeId: "pg/points-readiness", tier: tierWarn,
			class: "points-leaderboard-unavailable", target: target, sustain: 1,
			symptom:   "The points leaderboard has no published snapshot.",
			mechanism: "The public API reads an immutable snapshot. Neither total points nor operator-block metrics have a publication boundary without one.",
			baseline:  "At least one complete snapshot exists and the hourly fallback keeps its age below two hours.",
			observed:  "snapshot_present=false",
			evidence:  "The bounded latest-snapshot query returned no row.",
			action:    "Inspect the rebuild_points_leaderboard task and its migration gate. Repair the owning failure and let the normal idempotent rebuild publish; do not insert leaderboard rows manually.",
			verify:    "A normal rebuild publishes one internally consistent snapshot returned by the public points endpoint.",
			playbook:  "SIGNALS.md §17.6",
		}}, nil
	}
	if snapshot.rowCount != snapshot.totalRanked {
		return []finding{{
			probeId: "pg/points-readiness", tier: tierPage,
			class: "points-leaderboard-incomplete", target: target, sustain: 1,
			symptom:   "The latest points snapshot header and ranked-row census disagree.",
			mechanism: "A snapshot is intended to publish atomically. A mismatched header can expose an incomplete rank population.",
			baseline:  "network_points_leaderboard_snapshot.total_ranked equals the exact row count for that snapshot.",
			observed:  fmt.Sprintf("total_ranked=%d snapshot_rows=%d", snapshot.totalRanked, snapshot.rowCount),
			evidence:  "The header and row count were read in one PostgreSQL statement from the same snapshot.",
			action:    "Preserve the snapshot and repair the transactional rebuild path. Do not patch counts or ranked rows by hand.",
			verify:    "A normal rebuild publishes equal header and row counts and the API pages the same snapshot without overlap or omission.",
			playbook:  "SIGNALS.md §17.6",
		}}, nil
	}
	findings := []finding{}
	if snapshot.ageSeconds < 0 || int64(pointsSnapshotMaxAge/time.Second) < snapshot.ageSeconds {
		findings = append(findings, finding{
			probeId: "pg/points-readiness", tier: tierWarn,
			class: "points-leaderboard-stale", target: target, sustain: 2,
			symptom:   "The points leaderboard snapshot is outside its hourly publication budget.",
			mechanism: "Payout completion triggers rebuilds, with an hourly fallback. A snapshot older than two hours can omit new points or completed operator blocks.",
			baseline:  "The latest complete snapshot is no more than two hours old and is not future-dated.",
			observed:  fmt.Sprintf("snapshot_age_seconds=%d total_ranked=%d", snapshot.ageSeconds, snapshot.totalRanked),
			evidence:  "PostgreSQL computed age from its UTC-normalized clock and snapshot create_time.",
			action:    "Inspect the rebuild_points_leaderboard schedule and latest terminal error, repair the owning task or dependency, and allow a normal rebuild. Do not rewrite create_time.",
			verify:    "Two successive hourly boundaries publish fresh complete snapshots.",
			playbook:  "SIGNALS.md §17.6",
		})
	}

	sourceRows, err := env.runner.pg(ctx, pointsOperatorSourceQuery())
	if err != nil {
		return nil, err
	}
	source, err := parsePointsOperatorSource(sourceRows)
	if err != nil {
		return nil, err
	}
	appendFinding := func(tier, class, symptom, mechanism, action string) {
		findings = append(findings, pointsOperatorFinding(target, tier, class, symptom, mechanism, action, snapshot, source))
	}
	payloadPresent := snapshot.latestEpoch != 0 || snapshot.positiveBlocks != 0 || snapshot.positiveStreak != 0 || snapshot.positiveLongest != 0
	switch {
	case !snapshot.epochAvailable && payloadPresent:
		appendFinding(tierPage, "points-epoch-availability-drift",
			"The snapshot marks operator-block metrics unavailable but carries an affirmative block payload.",
			"The same immutable snapshot's availability bit and payload contradict each other, independently of current rollup progress.",
			"Preserve the snapshot and inspect the transactional rebuild and availability write. Repair that boundary, then publish through a normal rebuild.")
	case snapshot.epochAvailable && snapshot.latestEpoch == 0:
		appendFinding(tierPage, "points-epoch-availability-drift",
			"The snapshot marks operator-block metrics available without a completed operator block number.",
			"Operator blocks are 1-based; a true availability bit requires at least one completed block. This is distinct from a valid measured zero Blocks or Streak value.",
			"Verify the operator-rollup release boundary and rebuild source, then let the normal rebuild replace the snapshot.")
	case source.closedBlocks < snapshot.latestEpoch:
		appendFinding(tierPage, "points-epoch-snapshot-drift",
			"The snapshot block number is ahead of the completed operator calendar.",
			"A rebuild may lag completed Sunday-00 UTC blocks but cannot publish an open or future block as completed. This can also expose a source-version or application-clock mismatch.",
			"Verify the deployed operator source and UTC clock, preserve the snapshot, and repair the source/rebuild boundary before a normal rebuild.")
	case source.closedBlocks == 0:
		// Before the first Sunday closes, unavailable block metrics are expected.
	case source.missingPoints != 0 || source.pendingPoints != 0:
		class := "points-block-rollup-incomplete"
		if !snapshot.epochAvailable {
			class = "points-epoch-metrics-unavailable"
		}
		appendFinding(tierWarn, class,
			"The current positive-point source is not fully materialized into operator payout blocks.",
			"Positive post-genesis point rows with a missing linked payment or incomplete payment rollup keep a new block snapshot unavailable. An earlier available snapshot is not invalidated by a later pending payment. Oldest pending point age is not stalled duration or completion age.",
			"Inspect bounded rollup batches, the owning payment linkage and rebuild tasks. Pending linked payments can advance normally; missing links require source diagnosis. Preserve total-point ranks and do not fabricate payment or block rows.")
	case !snapshot.epochAvailable || snapshot.latestEpoch < source.closedBlocks:
		appendFinding(tierWarn, "points-epoch-rebuild-pending",
			"The operator calendar and currently complete rollup are ahead of the published block snapshot.",
			"Rollup and snapshot publication are separate reads and transactions. Completion time is unknown; even an old Sunday boundary cannot prove that this rebuild handoff is overdue. A stale snapshot retains its separate two-hour warning.",
			"Let the normal bounded rollup/rebuild task publish. Check repeated progress and task failures if this persists; do not infer ST readiness or flip the availability bit.")
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("pg/points-readiness", tierWarn, "points-readiness", target))
	}
	return findings, nil
}
