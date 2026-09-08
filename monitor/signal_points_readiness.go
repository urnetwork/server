package monitor

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// SIGNALS.md §17.6 maps to signal_points_readiness.go and
// signal_points_readiness_test.go. The signal keeps total-point availability
// separate from the finalized-epoch source required by Blocks and Streak.
func NewPointsReadinessSignal() Signal {
	return &signalAdapter{
		number: "17.6",
		key:    "points-readiness",
		name:   "Points leaderboard epoch readiness",
		probe:  pointsReadinessProbe{},
	}
}

type pointsReadinessProbe struct{}

func (pointsReadinessProbe) id() string             { return "pg/points-readiness" }
func (pointsReadinessProbe) tier() string           { return tierWarn }
func (pointsReadinessProbe) cadence() time.Duration { return 5 * time.Minute }

const pointsSnapshotQuery = `
	WITH latest AS (
		SELECT snapshot_id, create_time, latest_epoch, total_ranked
		FROM network_points_leaderboard_snapshot
		ORDER BY create_time DESC, snapshot_id DESC
		LIMIT 1
	)
	SELECT round(extract(epoch FROM clock_timestamp() - latest.create_time))::bigint,
	       latest.latest_epoch,
	       latest.total_ranked,
	       count(ranked.network_id),
	       count(*) FILTER (WHERE ranked.blocks_with_points > 0),
	       count(*) FILTER (WHERE ranked.streak > 0),
	       count(*) FILTER (WHERE ranked.longest_streak > 0)
	FROM latest
	LEFT JOIN network_points_leaderboard AS ranked
	  ON ranked.snapshot_id = latest.snapshot_id
	GROUP BY latest.snapshot_id, latest.create_time, latest.latest_epoch, latest.total_ranked;
`

const pointsEpochCensusQuery = `
	SELECT deployment_key,
	       count(*) FILTER (WHERE status = 'finalized'),
	       coalesce(max(epoch) FILTER (WHERE status = 'finalized'), 0)
	FROM st_epoch
	GROUP BY deployment_key
	ORDER BY deployment_key
	LIMIT 1001;
`

const (
	pointsSnapshotMaxAge = 2 * time.Hour
	pointsEpochCensusMax = 1000
)

type pointsSnapshotState struct {
	present         bool
	ageSeconds      int64
	latestEpoch     uint64
	totalRanked     uint64
	rowCount        uint64
	positiveBlocks  uint64
	positiveStreak  uint64
	positiveLongest uint64
}

type pointsEpochState struct {
	finalized uint64
	latest    uint64
}

func parsePointsSigned(row pgRow, column int, name string) (int64, error) {
	if len(row) <= column {
		return 0, fmt.Errorf("points readiness row is missing %s", name)
	}
	value, err := strconv.ParseInt(row.str(column), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("points readiness has invalid %s %q", name, row.str(column))
	}
	return value, nil
}

func parsePointsUnsigned(row pgRow, column int, name string) (uint64, error) {
	if len(row) <= column {
		return 0, fmt.Errorf("points readiness row is missing %s", name)
	}
	value, err := strconv.ParseUint(row.str(column), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("points readiness has invalid %s %q", name, row.str(column))
	}
	return value, nil
}

func parsePointsSnapshot(rows []pgRow) (pointsSnapshotState, error) {
	if len(rows) == 0 {
		return pointsSnapshotState{}, nil
	}
	if len(rows) != 1 || len(rows[0]) != 7 {
		return pointsSnapshotState{}, fmt.Errorf("points snapshot query returned %d rows with an invalid shape", len(rows))
	}
	state := pointsSnapshotState{present: true}
	var err error
	if state.ageSeconds, err = parsePointsSigned(rows[0], 0, "snapshot age"); err != nil {
		return pointsSnapshotState{}, err
	}
	values := []struct {
		name string
		dest *uint64
	}{
		{"latest epoch", &state.latestEpoch},
		{"total ranked", &state.totalRanked},
		{"snapshot row count", &state.rowCount},
		{"positive blocks count", &state.positiveBlocks},
		{"positive streak count", &state.positiveStreak},
		{"positive longest-streak count", &state.positiveLongest},
	}
	for index, item := range values {
		if *item.dest, err = parsePointsUnsigned(rows[0], index+1, item.name); err != nil {
			return pointsSnapshotState{}, err
		}
	}
	return state, nil
}

func parsePointsEpochCensus(rows []pgRow) (map[string]pointsEpochState, error) {
	if len(rows) > pointsEpochCensusMax {
		return nil, fmt.Errorf("points epoch census reached the %d-deployment completeness cap", pointsEpochCensusMax)
	}
	states := make(map[string]pointsEpochState, len(rows))
	for _, row := range rows {
		if len(row) != 3 || row.str(0) == "" {
			return nil, fmt.Errorf("points epoch census returned an invalid row")
		}
		if _, duplicate := states[row.str(0)]; duplicate {
			return nil, fmt.Errorf("points epoch census returned a duplicate deployment")
		}
		finalized, err := parsePointsUnsigned(row, 1, "finalized epoch count")
		if err != nil {
			return nil, err
		}
		latest, err := parsePointsUnsigned(row, 2, "latest finalized epoch")
		if err != nil {
			return nil, err
		}
		states[row.str(0)] = pointsEpochState{finalized: finalized, latest: latest}
	}
	return states, nil
}

func pointsReadinessTarget(env *probeEnv) string {
	if host := env.cfg.hostByRole("pg-primary"); host != nil {
		return host.name
	}
	return "pg"
}

func (pointsReadinessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
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
			mechanism: "The public API reads only the latest materialized snapshot. Without one, neither total points nor finalized-epoch metrics have an authoritative publication boundary.",
			baseline:  "At least one complete snapshot exists and the hourly fallback keeps its age below two hours.",
			observed:  "snapshot_present=false",
			evidence:  "The bounded latest-snapshot query returned no row.",
			action:    "Inspect the rebuild_points_leaderboard task and its migration gate. Repair the first task or schema failure and let the normal idempotent rebuild publish a snapshot; do not insert leaderboard rows manually.",
			verify:    "A normal rebuild succeeds, this signal observes one internally consistent snapshot, and the public points-leaderboard endpoint returns that snapshot.",
			playbook:  "SIGNALS.md §17.6",
		}}, nil
	}

	findings := []finding{}
	if snapshot.rowCount != snapshot.totalRanked {
		return []finding{{
			probeId: "pg/points-readiness", tier: tierPage,
			class: "points-leaderboard-incomplete", target: target, sustain: 1,
			symptom:   "The latest points snapshot header and ranked-row census disagree.",
			mechanism: "A snapshot is intended to publish atomically. A mismatched header means the API can page an incomplete rank population even though the rebuild task recorded a snapshot identity.",
			baseline:  "network_points_leaderboard_snapshot.total_ranked equals the exact row count for that snapshot.",
			observed:  fmt.Sprintf("total_ranked=%d snapshot_rows=%d", snapshot.totalRanked, snapshot.rowCount),
			evidence:  "The header and row count were read in one PostgreSQL statement from the same latest snapshot.",
			action:    "Stop treating the latest snapshot as publishable, preserve it for diagnosis, and repair the transactional rebuild path. Do not patch counts or ranked rows by hand.",
			verify:    "A normal rebuild publishes equal header and row counts and the API pages the same immutable snapshot without overlap or omission.",
			playbook:  "SIGNALS.md §17.6",
		}}, nil
	}
	if snapshot.ageSeconds < 0 || int64(pointsSnapshotMaxAge/time.Second) < snapshot.ageSeconds {
		findings = append(findings, finding{
			probeId: "pg/points-readiness", tier: tierWarn,
			class: "points-leaderboard-stale", target: target, sustain: 2,
			symptom:   "The points leaderboard snapshot is outside its hourly publication budget.",
			mechanism: "Epoch finalization and payout completion trigger rebuilds, with an hourly fallback. A snapshot older than two hours means every sort can remain internally consistent while omitting newer points or epochs.",
			baseline:  "The latest complete snapshot is no more than two hours old and is not future-dated.",
			observed:  fmt.Sprintf("snapshot_age_seconds=%d total_ranked=%d", snapshot.ageSeconds, snapshot.totalRanked),
			evidence:  "PostgreSQL computed age from its own clock and the latest snapshot create_time.",
			action:    "Inspect the rebuild_points_leaderboard task schedule and latest terminal error, repair the owning task or dependency, and allow one normal rebuild. Do not rewrite create_time.",
			verify:    "Two successive hourly boundaries publish fresh complete snapshots and this class clears after its sustain window.",
			playbook:  "SIGNALS.md §17.6",
		})
	}

	epochRows, err := env.runner.pg(ctx, pointsEpochCensusQuery)
	if err != nil {
		return nil, err
	}
	epochs, err := parsePointsEpochCensus(epochRows)
	if err != nil {
		return nil, err
	}
	activeKey := env.cfg.stDeploymentKey
	active, activePresent := epochs[activeKey]
	if !env.cfg.verificationEnabled || activeKey == "" || !activePresent || active.finalized == 0 {
		reason := "the ST subsystem is disabled"
		sourceConfigured := activeKey != ""
		if env.cfg.verificationEnabled {
			switch {
			case activeKey == "":
				reason = "the enabled ST subsystem has no observable deployment identity"
			case !activePresent:
				reason = "the active ST deployment has no mirror rows"
			default:
				reason = "the active ST deployment has no finalized epoch"
			}
		}
		findings = append(findings, finding{
			probeId: "pg/points-readiness", tier: tierWarn,
			class: "points-epoch-metrics-unavailable", target: target, sustain: 1,
			symptom:   "Blocks and Streak are unavailable because " + reason + ".",
			mechanism: "Blocks, current streak, and longest streak are derived exclusively from finalized ST epoch windows. The current compute path receives no windows and serializes unavailable values and ranks as numeric zero, while total points continue to rank normally.",
			baseline:  "A populated points leaderboard either has a configured active deployment with at least one finalized epoch, or its API explicitly marks the epoch-derived fields unavailable instead of presenting zeros as measurements.",
			observed: fmt.Sprintf(
				"st_enabled=%t active_deployment_configured=%t active_deployment_present=%t finalized_epochs=%d snapshot_latest_epoch=%d total_ranked=%d positive_blocks=%d positive_streak=%d positive_longest_streak=%d",
				env.cfg.verificationEnabled, sourceConfigured, activePresent, active.finalized,
				snapshot.latestEpoch, snapshot.totalRanked, snapshot.positiveBlocks,
				snapshot.positiveStreak, snapshot.positiveLongest,
			),
			evidence: "The snapshot header/rows and deployment-scoped finalized-epoch census were read directly from PostgreSQL. The deployment identity was compared only in memory and is not rendered.",
			context:  "This has separate software and operational closure. Software must preserve total points while exposing snapshot-consistent epoch-metric availability through the API, SDK, and apps. Actual Blocks/Streak history requires the legitimate ST deployment and finalized epochs. A service deploy cannot invent that history, and an unready chain or contract must not be enabled merely to clear this alert.",
			action:   "Add an explicit epoch-metrics availability/finalized-count field and label or hide Blocks, Streak, Longest Streak, and their ranks until available. Separately, enable only the reviewed ST deployment after its node, contract, policy, keys, funding, and migration gates pass. Do not substitute legacy payout periods or open epochs, and do not backfill synthetic st_epoch rows.",
			verify:   "With ST still disabled or pre-finalization, every client displays epoch metrics as unavailable while total points remain usable. After the legitimate first finalized epoch, the snapshot records availability even when that valid epoch number is zero, latest_epoch matches the active mirror, and deterministic meaningful-zero plus positive-value controls pass.",
			playbook: "SIGNALS.md §17.6",
		})
		return findings, nil
	}

	if snapshot.latestEpoch != active.latest {
		findings = append(findings, finding{
			probeId: "pg/points-readiness", tier: tierPage,
			class: "points-epoch-snapshot-drift", target: target, sustain: 1,
			symptom:   "The points snapshot does not include the active deployment's latest finalized epoch.",
			mechanism: "The rebuild reads finalized epochs before atomically publishing its snapshot. A differing latest epoch means the public snapshot is behind, from another deployment, or was built across an invalid source boundary.",
			baseline:  "snapshot.latest_epoch equals the maximum finalized epoch for the exact active deployment; epoch zero remains a valid available value when finalized_count is positive.",
			observed:  fmt.Sprintf("finalized_epochs=%d active_latest_epoch=%d snapshot_latest_epoch=%d", active.finalized, active.latest, snapshot.latestEpoch),
			evidence:  "The exact configured deployment key selected one PostgreSQL census row in memory; no deployment identifier is included in this alert.",
			action:    "Inspect the active deployment selection and rebuild boundary, then run the normal idempotent rebuild after correcting the mismatch. Do not relabel an old deployment or update snapshot_latest_epoch manually.",
			verify:    "A newly published snapshot matches the same active deployment's maximum finalized epoch and remains matched across the next rebuild cadence.",
			playbook:  "SIGNALS.md §17.6",
		})
		return findings, nil
	}

	if len(findings) == 0 {
		findings = append(findings, healthyFinding("pg/points-readiness", tierWarn, "points-readiness", target))
	}
	return findings, nil
}
