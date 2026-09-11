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

const pointsEpochCensusQuery = `
	WITH lifecycle_clock AS MATERIALIZED (
		SELECT clock_timestamp() AT TIME ZONE 'UTC' AS now_utc
	), deployment_census AS (
		SELECT deployment_key,
		       count(*) FILTER (WHERE status = 'finalized') AS finalized_count,
		       coalesce(max(epoch) FILTER (WHERE status = 'finalized'), 0) AS latest_epoch
		FROM st_epoch
		GROUP BY deployment_key
	), latest_finalized AS (
		SELECT DISTINCT ON (deployment_key)
		       deployment_key, finalized_time
		FROM st_epoch
		WHERE status = 'finalized'
		ORDER BY deployment_key, epoch DESC
	)
	SELECT deployment_census.deployment_key,
	       deployment_census.finalized_count,
	       deployment_census.latest_epoch,
	       latest_finalized.finalized_time IS NOT NULL,
	       coalesce(round(extract(epoch FROM
	         lifecycle_clock.now_utc - latest_finalized.finalized_time))::bigint, 0)
	FROM deployment_census
	CROSS JOIN lifecycle_clock
	LEFT JOIN latest_finalized USING (deployment_key)
	ORDER BY deployment_census.deployment_key
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
	epochAvailable  bool
	rowCount        uint64
	positiveBlocks  uint64
	positiveStreak  uint64
	positiveLongest uint64
}

type pointsEpochState struct {
	finalized               uint64
	latest                  uint64
	latestFinalizedAgeKnown bool
	latestFinalizedAge      int64
}

const (
	pointsEpochProvenanceSourceNewer   = "source-newer"
	pointsEpochProvenanceSnapshotNewer = "snapshot-newer"
	pointsEpochProvenanceAmbiguous     = "ambiguous"
)

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
	if len(rows) != 1 || len(rows[0]) != 8 {
		return pointsSnapshotState{}, fmt.Errorf("points snapshot query returned %d rows with an invalid shape", len(rows))
	}
	state := pointsSnapshotState{present: true}
	var err error
	if state.ageSeconds, err = parsePointsSigned(rows[0], 0, "snapshot age"); err != nil {
		return pointsSnapshotState{}, err
	}
	if state.epochAvailable, err = strconv.ParseBool(rows[0].str(3)); err != nil {
		return pointsSnapshotState{}, fmt.Errorf("points readiness has invalid epoch-metrics availability %q", rows[0].str(3))
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
		column := index + 1
		if 2 <= index {
			column++
		}
		if *item.dest, err = parsePointsUnsigned(rows[0], column, item.name); err != nil {
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
		if len(row) != 5 || row.str(0) == "" {
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
		latestFinalizedAgeKnown, err := strconv.ParseBool(row.str(3))
		if err != nil {
			return nil, fmt.Errorf("points readiness has invalid latest-finalization provenance %q", row.str(3))
		}
		latestFinalizedAge, err := parsePointsSigned(row, 4, "latest-finalization age")
		if err != nil {
			return nil, err
		}
		if finalized == 0 && (latest != 0 || latestFinalizedAgeKnown || latestFinalizedAge != 0) {
			return nil, fmt.Errorf("points epoch census returned contradictory empty-finalization evidence")
		}
		if !latestFinalizedAgeKnown && latestFinalizedAge != 0 {
			return nil, fmt.Errorf("points epoch census returned an age without timestamp provenance")
		}
		states[row.str(0)] = pointsEpochState{
			finalized:               finalized,
			latest:                  latest,
			latestFinalizedAgeKnown: latestFinalizedAgeKnown,
			latestFinalizedAge:      latestFinalizedAge,
		}
	}
	return states, nil
}

// The snapshot and epoch census are separate bounded read-only commands, and
// both stored timestamps are application-written UTC-naive values. Attribute
// ordering only when their database-derived ages differ by more than the
// complete command budget plus one second of integer-age quantization. This
// provenance is diagnostic only: a rebuild can read its epoch inputs before a
// concurrent finalization and publish afterward, so even snapshot-newer does
// not prove drift inside the two-hour rebuild budget. A null or future timestamp
// remains ambiguous instead of being promoted to a false corruption claim.
func pointsEpochProvenance(
	snapshot pointsSnapshotState,
	active pointsEpochState,
	commandTimeout time.Duration,
) string {
	if !active.latestFinalizedAgeKnown || snapshot.ageSeconds < 0 || active.latestFinalizedAge < 0 {
		return pointsEpochProvenanceAmbiguous
	}
	skewSeconds := int64(commandTimeout / time.Second)
	if skewSeconds < 0 {
		skewSeconds = 0
	}
	skewSeconds++
	if active.latestFinalizedAge < snapshot.ageSeconds &&
		skewSeconds < snapshot.ageSeconds-active.latestFinalizedAge {
		return pointsEpochProvenanceSourceNewer
	}
	if snapshot.ageSeconds < active.latestFinalizedAge &&
		skewSeconds < active.latestFinalizedAge-snapshot.ageSeconds {
		return pointsEpochProvenanceSnapshotNewer
	}
	return pointsEpochProvenanceAmbiguous
}

func pointsEpochMismatchPending(
	snapshot pointsSnapshotState,
	active pointsEpochState,
	commandTimeout time.Duration,
) (bool, string) {
	provenance := pointsEpochProvenance(snapshot, active, commandTimeout)
	if active.latestFinalizedAgeKnown &&
		int64(pointsSnapshotMaxAge/time.Second) < active.latestFinalizedAge {
		return false, provenance
	}
	return true, provenance
}

func pointsEpochObserved(
	env *probeEnv,
	snapshot pointsSnapshotState,
	active pointsEpochState,
	activePresent bool,
	provenance string,
) string {
	return fmt.Sprintf(
		"st_enabled=%t active_deployment_configured=%t active_deployment_present=%t finalized_epochs=%d snapshot_epoch_metrics_available=%t active_latest_epoch=%d snapshot_latest_epoch=%d snapshot_age_seconds=%d latest_finalized_age_known=%t latest_finalized_age_seconds=%d provenance=%s positive_blocks=%d positive_streak=%d positive_longest_streak=%d",
		env.cfg.verificationEnabled, env.cfg.stDeploymentKey != "", activePresent,
		active.finalized, snapshot.epochAvailable, active.latest, snapshot.latestEpoch,
		snapshot.ageSeconds, active.latestFinalizedAgeKnown, active.latestFinalizedAge,
		provenance, snapshot.positiveBlocks, snapshot.positiveStreak, snapshot.positiveLongest,
	)
}

func pointsEpochAvailabilityDriftFinding(
	target string,
	env *probeEnv,
	snapshot pointsSnapshotState,
	active pointsEpochState,
	activePresent bool,
	provenance string,
	mechanism string,
) finding {
	return finding{
		probeId: "pg/points-readiness", tier: tierPage,
		class: "points-epoch-availability-drift", target: target, sustain: 1,
		symptom:   "The points snapshot's persisted epoch-metrics availability contradicts its authoritative finalized-epoch source or its own measurement payload.",
		mechanism: mechanism,
		baseline:  "Before the exact active deployment has a finalized epoch, epoch_metrics_available is false and stored numeric epoch measures are semantically unavailable; after a rebuild with finalized input it is true, including when the latest valid epoch is zero.",
		observed:  pointsEpochObserved(env, snapshot, active, activePresent, provenance),
		evidence:  "The persisted snapshot bit/header/row census and a privacy-safe deployment-scoped finalized count, latest epoch, and latest-finalization age were read from PostgreSQL. The configured deployment identity was compared only in memory and is not rendered.",
		context:   "The availability field, rebuild write, read API, and client presentation contract are already implemented. This PAGE is reserved for an impossible direction, overdue rebuild, or internally contradictory snapshot; it is not a request to add the field or fabricate chain history.",
		action:    "Preserve the contradictory snapshot, inspect active-deployment selection and the normal transactional leaderboard rebuild, repair the exact source or rebuild boundary, then let one idempotent rebuild publish a replacement. Do not edit the availability bit, snapshot rows, deployment identity, or st_epoch rows by hand.",
		verify:    "A normal rebuild publishes one internally consistent snapshot whose persisted availability bit matches the same active deployment's finalized census and whose API response carries that exact bit; repeat across the next rebuild cadence.",
		playbook:  "SIGNALS.md §17.6",
	}
}

func pointsEpochRebuildPendingFinding(
	target string,
	env *probeEnv,
	snapshot pointsSnapshotState,
	active pointsEpochState,
	activePresent bool,
	provenance string,
) finding {
	return finding{
		probeId: "pg/points-readiness", tier: tierWarn,
		class: "points-epoch-rebuild-pending", target: target, sustain: 1,
		symptom:   "The active finalized-epoch source is ahead of the latest published points snapshot.",
		mechanism: "Epoch finalization and leaderboard publication are separate transactions joined by an asynchronous durable task. The latest finalization is within the two-hour rebuild budget, or its nullable/application-clock provenance cannot prove that budget expired; even a later snapshot create time can follow an earlier input read. This state is not affirmative snapshot corruption.",
		baseline:  "A newly finalized source may lead the immutable snapshot only until the normal triggered or hourly rebuild completes, within two hours.",
		observed:  pointsEpochObserved(env, snapshot, active, activePresent, provenance),
		evidence:  "PostgreSQL returned aggregate source and snapshot ages only. A command-timeout-sized skew bound separates affirmative ordering from null, future-clock, subsecond, or cross-query ambiguity; no deployment key or timestamp is rendered.",
		context:   "This WARN preserves visibility through the legitimate finalize-to-rebuild handoff. It must not be promoted to a transactional-corruption PAGE solely because a five-minute monitor cadence sampled that handoff.",
		action:    "Allow the normal idempotent rebuild task to publish the finalized input. If a known latest-finalization age becomes older than two hours while the snapshot remains behind, diagnose the task/source boundary. Do not edit snapshot or st_epoch rows by hand.",
		verify:    "The next normal rebuild publishes epoch_metrics_available=true with latest_epoch equal to the active finalized maximum, and the pending class clears before the two-hour budget expires.",
		playbook:  "SIGNALS.md §17.6",
	}
}

func pointsEpochSnapshotDriftFinding(
	target string,
	env *probeEnv,
	snapshot pointsSnapshotState,
	active pointsEpochState,
	activePresent bool,
	provenance string,
	mechanism string,
) finding {
	return finding{
		probeId: "pg/points-readiness", tier: tierPage,
		class: "points-epoch-snapshot-drift", target: target, sustain: 1,
		symptom:   "The points snapshot and active deployment's finalized-epoch maximum have an impossible or overdue ordering.",
		mechanism: mechanism,
		baseline:  "snapshot.latest_epoch equals the maximum finalized epoch for the exact active deployment after the bounded asynchronous rebuild handoff; epoch zero remains valid when finalized_count is positive.",
		observed:  pointsEpochObserved(env, snapshot, active, activePresent, provenance),
		evidence:  "The configured deployment key selected one aggregate PostgreSQL census row in memory. Only counts, ages, booleans, epoch numbers, and a bounded provenance class are rendered.",
		action:    "Inspect active-deployment selection and the rebuild task/source boundary, then let the normal idempotent rebuild replace the stale snapshot after correcting the cause. Do not relabel a deployment or update snapshot_latest_epoch manually.",
		verify:    "A newly published snapshot matches the same active deployment's maximum finalized epoch and remains matched across the next rebuild cadence.",
		playbook:  "SIGNALS.md §17.6",
	}
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
	sourceAvailable := env.cfg.verificationEnabled && activeKey != "" && activePresent && 0 < active.finalized
	epochPayloadPresent := snapshot.latestEpoch != 0 || snapshot.positiveBlocks != 0 ||
		snapshot.positiveStreak != 0 || snapshot.positiveLongest != 0
	provenance := pointsEpochProvenance(snapshot, active, env.cfg.commandTimeout)

	// These are internally or directionally impossible regardless of the
	// asynchronous finalization-to-rebuild handoff.
	switch {
	case !snapshot.epochAvailable && epochPayloadPresent:
		findings = append(findings, pointsEpochAvailabilityDriftFinding(
			target, env, snapshot, active, activePresent, provenance,
			"The snapshot marks epoch metrics unavailable, but its header or ranked rows carry epoch-derived values. The atomic snapshot payload and its availability bit were not produced from one input boundary.",
		))
		return findings, nil
	case snapshot.epochAvailable && !sourceAvailable:
		findings = append(findings, pointsEpochAvailabilityDriftFinding(
			target, env, snapshot, active, activePresent, provenance,
			"The snapshot marks epoch metrics available without finalized windows from the enabled exact active ST deployment, so clients can present stale or source-less values as measurements.",
		))
		return findings, nil
	}

	if !sourceAvailable {
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
			mechanism: "Blocks, current streak, and longest streak are derived exclusively from finalized ST epoch windows. The persisted false availability bit correctly keeps those fields unavailable while total points continue to rank normally.",
			baseline:  "Before finalization, the snapshot persists epoch_metrics_available=false and every client treats epoch-derived values and ranks as unavailable. After the exact active deployment has at least one finalized epoch, the next snapshot persists true.",
			observed: fmt.Sprintf(
				"st_enabled=%t active_deployment_configured=%t active_deployment_present=%t finalized_epochs=%d snapshot_epoch_metrics_available=%t snapshot_latest_epoch=%d total_ranked=%d positive_blocks=%d positive_streak=%d positive_longest_streak=%d",
				env.cfg.verificationEnabled, sourceConfigured, activePresent, active.finalized,
				snapshot.epochAvailable, snapshot.latestEpoch, snapshot.totalRanked, snapshot.positiveBlocks,
				snapshot.positiveStreak, snapshot.positiveLongest,
			),
			evidence: "The snapshot header/rows and deployment-scoped finalized-epoch census were read directly from PostgreSQL. The deployment identity was compared only in memory and is not rendered.",
			context:  "The snapshot field, Server API, SDK, and app presentation contract are already implemented and deployed. Only legitimate ST readiness and finalized history can make the correctly false bit true; another software-field deployment cannot invent that history.",
			action:   "Keep the correctly false availability bit and total-points snapshot intact. If this ST deployment is intended to activate, complete its reviewed node, contract, policy, keys, funding, and migration gates and obtain the first legitimate finalized epoch, then let the normal rebuild publish availability. Do not add or hand-edit the field, enable an unready deployment, substitute legacy/open epochs, or backfill synthetic st_epoch rows.",
			verify:   "Before finalization, the persisted bit remains false and every client displays epoch metrics as unavailable while total points remain usable. After the legitimate first finalized epoch, a normal snapshot records true even when that valid epoch number is zero, latest_epoch matches the active mirror, and deterministic meaningful-zero plus positive-value controls pass.",
			playbook: "SIGNALS.md §17.6",
		})
		return findings, nil
	}

	if active.latest < snapshot.latestEpoch {
		findings = append(findings, pointsEpochSnapshotDriftFinding(
			target, env, snapshot, active, activePresent, provenance,
			"The snapshot header is ahead of the exact active deployment's finalized maximum. An asynchronous rebuild can lag its source but cannot publish an epoch that the source does not contain.",
		))
		return findings, nil
	}

	// Finalization and snapshot publication are separate transactions. A false
	// availability bit after the first finalization and a true-but-behind latest
	// epoch are therefore the same source-newer handoff, including epoch zero.
	sourceAhead := !snapshot.epochAvailable || snapshot.latestEpoch < active.latest
	if sourceAhead {
		pending, mismatchProvenance := pointsEpochMismatchPending(snapshot, active, env.cfg.commandTimeout)
		if pending {
			findings = append(findings, pointsEpochRebuildPendingFinding(
				target, env, snapshot, active, activePresent, mismatchProvenance,
			))
			return findings, nil
		}
		if !snapshot.epochAvailable {
			mechanism := "The latest finalized transition is older than the two-hour rebuild budget, but the published snapshot still marks epoch metrics unavailable."
			findings = append(findings, pointsEpochAvailabilityDriftFinding(
				target, env, snapshot, active, activePresent, mismatchProvenance, mechanism,
			))
			return findings, nil
		}
		mechanism := "The latest finalized transition is older than the two-hour rebuild budget, but the published snapshot remains behind its source."
		findings = append(findings, pointsEpochSnapshotDriftFinding(
			target, env, snapshot, active, activePresent, mismatchProvenance, mechanism,
		))
		return findings, nil
	}

	if len(findings) == 0 {
		findings = append(findings, healthyFinding("pg/points-readiness", tierWarn, "points-readiness", target))
	}
	return findings, nil
}
