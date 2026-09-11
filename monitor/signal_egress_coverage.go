package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/model"
)

const providerEgressProbeTaskFunction = "github.com/urnetwork/server/taskworker/work.ProviderEgressProbe"

// SIGNALS.md §2.19 maps to signal_egress_coverage.go and
// signal_egress_coverage_test.go. It proves that every configured durable
// provider-probe shard exists and that due work produces fresh aggregate
// evidence; it never exports provider or task identifiers.
func NewEgressCoverageSignal() Signal {
	return &signalAdapter{
		number: "2.19", key: "egress-coverage", name: "Provider egress probe coverage",
		probe: egressCoverageProbe{},
	}
}

type egressCoverageProbe struct{}

func (egressCoverageProbe) id() string             { return "pg/egress-coverage" }
func (egressCoverageProbe) tier() string           { return tierWarn }
func (egressCoverageProbe) cadence() time.Duration { return 5 * time.Minute }

type egressCoverageBatchArgs struct {
	Limit                   int  `json:"limit"`
	Concurrency             int  `json:"concurrency"`
	ProbeTimeoutSeconds     int  `json:"probe_timeout_seconds"`
	AllDestinations         bool `json:"all_destinations,omitempty"`
	Bandwidth               bool `json:"bandwidth,omitempty"`
	BandwidthTimeoutSeconds int  `json:"bandwidth_timeout_seconds,omitempty"`
}

type egressCoverageTaskArgs struct {
	ShardIndex       int                     `json:"shard_index"`
	ShardCount       int                     `json:"shard_count"`
	IdleDelaySeconds int                     `json:"idle_delay_seconds"`
	MaxTimeSeconds   int                     `json:"max_time_seconds"`
	Full             egressCoverageBatchArgs `json:"full"`
	Blackhole        egressCoverageBatchArgs `json:"blackhole"`
	APIURL           string                  `json:"api_url"`
	PlatformURL      string                  `json:"platform_url"`
	PublicAPIURL     string                  `json:"public_api_url,omitempty"`
	BandwidthCDNURL  string                  `json:"bandwidth_cdn_url,omitempty"`
}

type egressCoverageGeometry struct {
	shardCount              int
	idleDelaySeconds        int
	maxTimeSeconds          int
	fullLimit               int
	fullConcurrency         int
	fullTimeoutSeconds      int
	blackholeConcurrency    int
	blackholeTimeoutSeconds int
	indices                 []int
}

type egressCoverageConfig struct {
	shardCount       int
	idleDelaySeconds int
	maxTimeSeconds   int
	full             egressCoverageBatchArgs
	blackhole        egressCoverageBatchArgs
	apiURL           string
	platformURL      string
	publicAPIURL     string
	bandwidthCDNURL  string
}

func (egressCoverageProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	schemaRows, err := env.runner.pg(ctx, `
		SELECT
		 EXISTS (
		  SELECT 1 FROM pg_attribute
		  WHERE attrelid='provider_egress_health'::regclass
		    AND attname='tls_authentication_failure'
		    AND NOT attisdropped
		 ),
		 EXISTS (
		  SELECT 1
		  FROM pg_index AS index_record
		  JOIN pg_class AS index_relation ON index_relation.oid = index_record.indexrelid
		  JOIN pg_class AS table_relation ON table_relation.oid = index_record.indrelid
		  JOIN pg_namespace AS namespace ON namespace.oid = table_relation.relnamespace
		  WHERE namespace.nspname = 'public'
		    AND table_relation.relname = 'provider_egress_health'
		    AND index_relation.relname = 'provider_egress_health_measured_at_client_id'
		    AND regexp_replace(pg_get_indexdef(index_relation.oid), '[[:space:]]+', ' ', 'g') =
		        '`+providerEgressHealthDeadlineIndexDefinition+`'
		    AND index_record.indpred IS NULL
		    AND index_record.indisvalid
		    AND index_record.indisready
		 );
	`)
	if err != nil {
		return nil, err
	}
	if len(schemaRows) != 1 || len(schemaRows[0]) != 2 {
		return nil, fmt.Errorf("provider egress coverage schema query returned an invalid shape")
	}
	tlsIntegrityArmed, err := strconv.ParseBool(schemaRows[0].str(0))
	if err != nil {
		return nil, fmt.Errorf("provider egress coverage returned invalid TLS-integrity arming state %q", schemaRows[0].str(0))
	}
	healthDeadlineIndexArmed, err := strconv.ParseBool(schemaRows[0].str(1))
	if err != nil {
		return nil, fmt.Errorf("provider egress coverage returned invalid health-deadline index state %q", schemaRows[0].str(1))
	}

	taskRows, err := env.runner.pg(ctx, `
		SELECT COALESCE(run_once_key, ''), args_json, run_max_time_seconds::text
		FROM pending_task
		WHERE function_name = 'github.com/urnetwork/server/taskworker/work.ProviderEgressProbe'
		ORDER BY run_once_key;
	`)
	if err != nil {
		return nil, err
	}
	target := pgTarget(env)
	if !tlsIntegrityArmed || len(taskRows) == 0 {
		missing := []string{}
		if !tlsIntegrityArmed {
			missing = append(missing, "tls_authentication_failure schema")
		}
		if !healthDeadlineIndexArmed {
			missing = append(missing, "health-deadline ordered index")
		}
		if len(taskRows) == 0 {
			missing = append(missing, "durable ProviderEgressProbe tasks")
		}
		action := "Apply only the pending append-only provider-egress migrations, including migration 657 when its exact health-deadline index is absent. After migration coherence, deploy the API artifact containing the EDF scheduler, then deploy the Taskworker artifact containing selective attempt cleanup and let normal task initialization converge the shards; do not insert, delete, or hand-edit pending_task rows."
		if tlsIntegrityArmed && healthDeadlineIndexArmed {
			action = "The append-only provider-egress schema, including migration 657, is already armed; do not repeat migrations. Deploy the API artifact containing the EDF scheduler, then deploy the Taskworker artifact containing selective attempt cleanup and let normal task initialization converge the shards; do not insert, delete, or hand-edit pending_task rows."
		}
		return []finding{{
			probeId: "pg/egress-coverage", tier: tierWarn,
			class: "egress-probe-unarmed", target: target, frame: "rollout", sustain: 2,
			symptom:   "The provider-egress pipeline is not fully armed: " + strings.Join(missing, " and ") + " are absent.",
			mechanism: "Provider scoring can only rely on egress evidence after the append-only integrity schema and the host-independent recurring task shards both exist. An empty task result is rollout absence, not proof that zero providers need measurement.",
			baseline:  "The TLS-integrity column exists, the health-deadline index is a valid, ready, non-partial btree over exactly (measured_at, client_id), and pending_task contains one internally consistent ProviderEgressProbe row for every configured shard.",
			observed:  fmt.Sprintf("tls_integrity_armed=%t health_deadline_index_armed=%t provider_egress_task_rows=%d", tlsIntegrityArmed, healthDeadlineIndexArmed, len(taskRows)),
			evidence:  "Only schema presence and aggregate task-row count are exported; task IDs, client IDs, credentials, endpoints, and argument JSON remain private.",
			context:   "This is a software rollout/operational boundary, not a Proxy hardware-capacity alert. The generic task canary remains responsible for individual claim, timeout, and reschedule errors.",
			action:    action,
			verify:    "Migration 657's exact ordered health-deadline index is valid and ready, every API is on the EDF scheduler generation, every Taskworker is on the selective-cleanup/task generation, all shard rows appear, and this signal observes fresh category-local output or no due work for two cadences.",
			playbook:  "SIGNALS.md §2.19, §2.10, and §8.9",
		}}, nil
	}

	geometry, geometryErr := inspectEgressCoverageTasks(taskRows)
	if geometryErr != nil {
		return []finding{{
			probeId: "pg/egress-coverage", tier: tierPage,
			class: "egress-probe-shards", target: target, frame: "durable-geometry", sustain: 1,
			symptom:   "Durable provider-egress tasks do not form one complete, internally consistent shard geometry.",
			mechanism: "Each recurring task carries the total shard count, one disjoint index, and the complete endpoint, destination, and bandwidth execution snapshot. A missing, duplicate, mixed-generation, unknown-setting, or malformed row strands or changes part of the provider fleet even while sibling shards continue producing healthy-looking global timestamps.",
			baseline:  "Exactly shard_count rows exist, their indexes cover [0, shard_count), each run_once_key names its index, and every row carries the same complete bounded execution settings.",
			observed:  fmt.Sprintf("provider_egress_task_rows=%d geometry_error=%s", len(taskRows), geometryErr),
			evidence:  "The parser recognizes the complete current task schema, rejects unknown JSON fields, and reports bounded structural reasons only; it never copies task IDs, raw JSON, provider IDs, credentials, or endpoint values into the alert.",
			context:   "A healthy sibling task cannot compensate for a missing hash partition. Generic task health may still be green because this check concerns fleet coverage, not whether one existing row can execute.",
			action:    "Converge every Taskworker on one configuration and allow the normal ProviderEgressProbe post-step/bootstrap scheduler to replace stale geometry. Do not manually clone, delete, or rewrite task rows.",
			verify:    "The durable rows converge to one complete geometry and execution snapshot, and every shard either has no due candidates or advances its own aggregate full and blackhole evidence within the derived execution bound.",
			playbook:  "SIGNALS.md §2.19 and §8.9",
		}}, nil
	}

	activityRows, err := env.runner.pg(ctx, egressCoverageActivityQuery(geometry.shardCount))
	if err != nil {
		return nil, err
	}
	activity, err := parseEgressCoverageActivity(activityRows, geometry.shardCount)
	if err != nil {
		return nil, err
	}
	stallSeconds := int64(geometry.maxTimeSeconds) + int64(geometry.idleDelaySeconds) + int64((5*time.Minute)/time.Second)
	findings := []finding{
		healthyFinding("pg/egress-coverage", tierPage, "egress-probe-shards", target),
	}
	if healthDeadlineIndexArmed {
		findings = append(findings, healthyFinding("pg/egress-coverage", tierWarn, "egress-probe-unarmed", target))
	} else {
		findings = append(findings, finding{
			probeId: "pg/egress-coverage", tier: tierWarn,
			class: "egress-probe-unarmed", target: target, frame: "health-deadline-index", sustain: 2,
			symptom:   "The ordered stale-health scheduler index is absent, while the existing provider-egress activity observations remain available.",
			mechanism: "The bounded EDF scheduler drives the stale-health head by measured_at and client_id. Without the matching index PostgreSQL can sort the complete health table; this is a rollout/performance prerequisite, not evidence that the active probe pipeline or its other categories are unobservable.",
			baseline:  "provider_egress_health_measured_at_client_id is a valid, ready, non-partial btree over exactly (measured_at, client_id), and the shard, activity, fairness, and capacity observations continue independently.",
			observed:  fmt.Sprintf("tls_integrity_armed=%t health_deadline_index_armed=false provider_egress_task_rows=%d", tlsIntegrityArmed, len(taskRows)),
			evidence:  "Only schema presence and aggregate task-row count are exported; no provider, task, endpoint, or credential value leaves PostgreSQL.",
			context:   "This finding must not suppress the already-deployed shard, liveness, fairness, or capacity signals during migration rollout.",
			action:    "Apply migration 657 through the append-only migration runner, then deploy the API artifact containing the EDF scheduler, then deploy the Taskworker artifact containing selective attempt cleanup and allow its durable tasks to converge. Do not hand-create indexes or rewrite durable task rows.",
			verify:    "Migration coherence confirms migration 657's exact index is valid and ready, every API is on the EDF scheduler generation, every Taskworker is on the selective-cleanup/task generation, and all existing provider-egress findings remain observable for two cadences.",
			playbook:  "SIGNALS.md §2.19 and §8.9",
		})
	}
	for _, snapshot := range activity {
		frame := fmt.Sprintf("shard-%d-of-%d", snapshot.shardIndex, geometry.shardCount)
		if snapshot.eligible > 0 && snapshot.fullDue() > 0 && (snapshot.fullAgeSeconds < 0 || stallSeconds < snapshot.fullAgeSeconds) {
			findings = append(findings, egressCoverageStallFinding(
				target, frame, "full", snapshot.fullDue(), snapshot.fullAgeSeconds,
				snapshot.eligible, snapshot.fullCurrent, stallSeconds,
			))
		} else {
			findings = append(findings, healthyFinding("pg/egress-coverage", tierPage, "egress-full-stalled", target))
		}
		if snapshot.eligible > 0 && snapshot.blackholeDue > 0 && (snapshot.blackholeAgeSeconds < 0 || stallSeconds < snapshot.blackholeAgeSeconds) {
			findings = append(findings, egressCoverageStallFinding(
				target, frame, "blackhole", snapshot.blackholeDue, snapshot.blackholeAgeSeconds,
				snapshot.eligible, snapshot.blackholeCurrent, stallSeconds,
			))
		} else {
			findings = append(findings, healthyFinding("pg/egress-coverage", tierPage, "egress-blackhole-stalled", target))
		}
	}
	if capacity, ok := egressBlackholeCapacityFinding(target, geometry, activity); ok {
		findings = append(findings, capacity)
	} else {
		findings = append(findings, healthyFinding("pg/egress-coverage", tierPage, "egress-blackhole-capacity", target))
	}
	if capacity, ok := egressFullCapacityFinding(target, geometry, activity); ok {
		findings = append(findings, capacity)
	} else {
		findings = append(findings, healthyFinding("pg/egress-coverage", tierPage, "egress-full-capacity", target))
	}
	fairness := egressFullFairnessFindings(target, geometry, activity)
	findings = append(findings, healthyFinding("pg/egress-coverage", tierPage, "egress-full-fairness", target))
	findings = append(findings, fairness...)
	return findings, nil
}

func inspectEgressCoverageTasks(rows []pgRow) (egressCoverageGeometry, error) {
	geometry := egressCoverageGeometry{}
	seen := map[int]bool{}
	var expected egressCoverageConfig
	problems := []string{}
	for rowIndex, row := range rows {
		if len(row) != 3 {
			problems = append(problems, fmt.Sprintf("row_%d_invalid_shape", rowIndex+1))
			continue
		}
		args, err := decodeEgressCoverageTaskArgs(row.str(1))
		if err != nil {
			problems = append(problems, fmt.Sprintf("row_%d_malformed_args", rowIndex+1))
			continue
		}
		config := egressCoverageConfig{
			shardCount: args.ShardCount, idleDelaySeconds: args.IdleDelaySeconds, maxTimeSeconds: args.MaxTimeSeconds,
			full: args.Full, blackhole: args.Blackhole,
			apiURL: args.APIURL, platformURL: args.PlatformURL,
			publicAPIURL: args.PublicAPIURL, bandwidthCDNURL: args.BandwidthCDNURL,
		}
		if config.shardCount < 1 || 256 < config.shardCount || args.ShardIndex < 0 || config.shardCount <= args.ShardIndex ||
			config.idleDelaySeconds < 1 || config.maxTimeSeconds < 1 ||
			!validEgressCoverageBatchArgs(config.full) || !validEgressCoverageBatchArgs(config.blackhole) ||
			strings.TrimSpace(config.apiURL) == "" || strings.TrimSpace(config.platformURL) == "" {
			problems = append(problems, fmt.Sprintf("row_%d_invalid_settings", rowIndex+1))
			continue
		}
		storedMaxTime, err := strconv.Atoi(strings.TrimSpace(row.str(2)))
		if err != nil || storedMaxTime != args.MaxTimeSeconds {
			problems = append(problems, fmt.Sprintf("row_%d_max_time_mismatch", rowIndex+1))
		}
		wantRunOnce := fmt.Sprintf("[\"provider_egress_probe\",%d]", args.ShardIndex)
		if row.str(0) != wantRunOnce {
			problems = append(problems, fmt.Sprintf("row_%d_run_once_mismatch", rowIndex+1))
		}
		if seen[args.ShardIndex] {
			problems = append(problems, fmt.Sprintf("duplicate_shard_%d", args.ShardIndex))
		}
		seen[args.ShardIndex] = true
		if geometry.shardCount == 0 {
			geometry.shardCount = args.ShardCount
			geometry.idleDelaySeconds = args.IdleDelaySeconds
			geometry.maxTimeSeconds = args.MaxTimeSeconds
			geometry.fullLimit = args.Full.Limit
			geometry.fullConcurrency = args.Full.Concurrency
			geometry.fullTimeoutSeconds = args.Full.ProbeTimeoutSeconds
			geometry.blackholeConcurrency = args.Blackhole.Concurrency
			geometry.blackholeTimeoutSeconds = args.Blackhole.ProbeTimeoutSeconds
			expected = config
		} else if expected != config {
			problems = append(problems, fmt.Sprintf("row_%d_mixed_settings", rowIndex+1))
		}
	}
	if geometry.shardCount > 0 {
		for shardIndex := 0; shardIndex < geometry.shardCount; shardIndex++ {
			if !seen[shardIndex] {
				problems = append(problems, fmt.Sprintf("missing_shard_%d", shardIndex))
			}
		}
		if len(rows) != geometry.shardCount {
			problems = append(problems, fmt.Sprintf("row_count_%d_want_%d", len(rows), geometry.shardCount))
		}
	}
	if len(problems) > 0 {
		if len(problems) > 12 {
			problems = append(problems[:12], "additional_problems_redacted")
		}
		return egressCoverageGeometry{}, fmt.Errorf("%s", strings.Join(problems, ","))
	}
	geometry.indices = make([]int, 0, len(seen))
	for shardIndex := range seen {
		geometry.indices = append(geometry.indices, shardIndex)
	}
	sort.Ints(geometry.indices)
	return geometry, nil
}

func decodeEgressCoverageTaskArgs(raw string) (egressCoverageTaskArgs, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var args egressCoverageTaskArgs
	if err := decoder.Decode(&args); err != nil {
		return egressCoverageTaskArgs{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return egressCoverageTaskArgs{}, fmt.Errorf("multiple JSON values")
		}
		return egressCoverageTaskArgs{}, err
	}
	return args, nil
}

func validEgressCoverageBatchArgs(args egressCoverageBatchArgs) bool {
	return 0 < args.Limit && 0 < args.Concurrency && args.Concurrency <= args.Limit &&
		0 < args.ProbeTimeoutSeconds && (!args.Bandwidth || 0 < args.BandwidthTimeoutSeconds)
}

func egressCoverageActivityQuery(shardCount int) string {
	return fmt.Sprintf(`
		WITH lifecycle_clock AS MATERIALIZED (
		 SELECT now() AT TIME ZONE 'UTC' AS now_utc
		), shards AS (
		 SELECT generate_series(0, %d - 1) AS shard_index
		), eligible AS MATERIALIZED (
		 SELECT nclr.client_id,
		        ((hashtext(nclr.client_id::text) %% %d) + %d) %% %d AS shard_index
		 FROM network_client_location_reliability nclr
		 INNER JOIN network_client nc USING (client_id)
		 WHERE nc.active AND nc.source_client_id IS NULL
		   AND nclr.connected AND nclr.valid
		   AND EXISTS (
		    SELECT 1 FROM provide_key pk
		    WHERE pk.client_id=nclr.client_id AND pk.provide_mode=3
		   )
		), classified AS MATERIALIZED (
		 SELECT e.shard_index, e.client_id,
		        pel.observed_at, pea.attempt_at, peh.measured_at, pbc.checked_at,
		        pel.client_id IS NULL AS no_location,
		        CASE
		          WHEN pel.client_id IS NULL THEN 'no-location'
		          WHEN peh.client_id IS NULL THEN 'missing-health'
		          WHEN pel.observed_at < lifecycle_clock.now_utc - interval '84 hours' AND
		            (
		              peh.measured_at >= lifecycle_clock.now_utc - interval '12 hours' OR
		              pel.observed_at + interval '7 days' <= peh.measured_at + interval '24 hours'
		            ) THEN 'stale-location'
		          WHEN peh.measured_at < lifecycle_clock.now_utc - interval '12 hours' THEN 'stale-health'
		          ELSE ''
		        END AS urgent_lane,
		        (pea.client_id IS NULL OR
		          pea.attempt_at < lifecycle_clock.now_utc - interval '6 hours') AS attempt_due,
		        lifecycle_clock.now_utc
		 FROM eligible e
		 CROSS JOIN lifecycle_clock
		 LEFT JOIN provider_egress_location pel USING (client_id)
		 LEFT JOIN provider_egress_probe_attempt pea USING (client_id)
		 LEFT JOIN provider_egress_health peh USING (client_id)
		 LEFT JOIN provider_blackhole_check pbc USING (client_id)
		), snapshot AS (
		 SELECT s.shard_index,
		        count(c.client_id) AS eligible,
		        count(c.client_id) FILTER (WHERE c.no_location AND c.attempt_due) AS no_location_due,
		        count(c.client_id) FILTER (WHERE c.urgent_lane = 'stale-location' AND c.attempt_due) AS stale_location_due,
		        count(c.client_id) FILTER (WHERE c.urgent_lane = 'stale-health' AND c.attempt_due) AS stale_health_due,
		        count(c.client_id) FILTER (WHERE
		          c.urgent_lane = 'stale-location' AND c.attempt_due AND
		          c.observed_at < c.now_utc - interval '7 days'
		        ) AS stale_location_expired_due,
		        count(c.client_id) FILTER (WHERE
		          c.urgent_lane = 'stale-health' AND c.attempt_due AND
		          c.measured_at < c.now_utc - interval '24 hours'
		        ) AS stale_health_expired_due,
		        count(c.client_id) FILTER (WHERE
		          c.checked_at IS NULL OR c.checked_at < c.now_utc - interval '90 minutes'
		        ) AS blackhole_due,
		        max(GREATEST(c.observed_at, c.attempt_at, c.measured_at)) AS latest_full,
		        max(c.checked_at) AS latest_blackhole,
		        count(c.client_id) FILTER (WHERE c.observed_at >= c.now_utc - interval '7 days') AS full_current,
		        count(c.client_id) FILTER (WHERE c.checked_at >= c.now_utc - interval '3 hours') AS blackhole_current,
		        count(c.client_id) FILTER (WHERE c.attempt_at >= c.now_utc - interval '1 hour') AS full_attempted_last_hour,
		        count(c.client_id) FILTER (WHERE c.checked_at >= c.now_utc - interval '1 hour') AS blackhole_checked_last_hour,
		        min(c.observed_at) FILTER (WHERE c.urgent_lane = 'stale-location' AND c.attempt_due) AS oldest_stale_location,
		        min(c.measured_at) FILTER (WHERE c.urgent_lane = 'stale-health' AND c.attempt_due) AS oldest_stale_health,
		        count(c.client_id) FILTER (WHERE c.urgent_lane = 'missing-health' AND c.attempt_due) AS missing_health_due,
		        count(c.client_id) FILTER (WHERE
		          c.urgent_lane = 'missing-health' AND c.attempt_due AND
		          c.observed_at < c.now_utc - interval '24 hours'
		        ) AS missing_health_expired_due,
		        min(c.observed_at) FILTER (WHERE c.urgent_lane = 'missing-health' AND c.attempt_due) AS oldest_missing_health_anchor,
		        max(c.now_utc) AS now_utc
		 FROM shards s
		 LEFT JOIN classified c USING (shard_index)
		 GROUP BY s.shard_index
		)
		SELECT shard_index::text, eligible::text,
		       no_location_due::text, stale_location_due::text, stale_health_due::text,
		       stale_location_expired_due::text, stale_health_expired_due::text,
		       blackhole_due::text,
		       COALESCE(floor(extract(epoch FROM (now_utc - latest_full)))::bigint, -1)::text,
		       COALESCE(floor(extract(epoch FROM (now_utc - latest_blackhole)))::bigint, -1)::text,
		       full_current::text, blackhole_current::text,
		       full_attempted_last_hour::text, blackhole_checked_last_hour::text,
		       COALESCE(floor(extract(epoch FROM (now_utc - oldest_stale_location)))::bigint, -1)::text,
		       COALESCE(floor(extract(epoch FROM (now_utc - oldest_stale_health)))::bigint, -1)::text,
		       missing_health_due::text, missing_health_expired_due::text,
		       COALESCE(floor(extract(epoch FROM (now_utc - oldest_missing_health_anchor)))::bigint, -1)::text
		FROM snapshot
		ORDER BY shard_index;
	`, shardCount, shardCount, shardCount, shardCount)
}

type egressCoverageSnapshot struct {
	shardIndex                    int
	eligible                      int64
	noLocationDue                 int64
	staleLocationDue              int64
	staleHealthDue                int64
	staleLocationExpiredDue       int64
	staleHealthExpiredDue         int64
	blackholeDue                  int64
	fullAgeSeconds                int64
	blackholeAgeSeconds           int64
	fullCurrent                   int64
	blackholeCurrent              int64
	fullAttemptsLastHour          int64
	blackholeLastHour             int64
	staleLocationOldestAgeSeconds int64
	staleHealthOldestAgeSeconds   int64
	missingHealthDue              int64
	missingHealthExpiredDue       int64
	missingHealthOldestAgeSeconds int64
}

func (s egressCoverageSnapshot) fullDue() int64 {
	return s.noLocationDue + s.staleLocationDue + s.staleHealthDue + s.missingHealthDue
}

func parseEgressCoverageActivity(rows []pgRow, shardCount int) ([]egressCoverageSnapshot, error) {
	if len(rows) != shardCount {
		return nil, fmt.Errorf("provider egress activity returned %d shard rows, want %d", len(rows), shardCount)
	}
	snapshots := make([]egressCoverageSnapshot, 0, shardCount)
	seen := map[int]bool{}
	for _, row := range rows {
		if len(row) != 19 {
			return nil, fmt.Errorf("provider egress activity returned an invalid row shape")
		}
		values := make([]int64, 19)
		for i := range row {
			value, err := strconv.ParseInt(strings.TrimSpace(row.str(i)), 10, 64)
			isAge := i == 8 || i == 9 || i == 14 || i == 15 || i == 18
			if err != nil || (!isAge && value < 0) || (isAge && value < -1) {
				return nil, fmt.Errorf("provider egress activity returned invalid numeric field %d", i)
			}
			values[i] = value
		}
		shardIndex := int(values[0])
		if shardIndex < 0 || shardCount <= shardIndex || seen[shardIndex] {
			return nil, fmt.Errorf("provider egress activity returned invalid shard index %d", shardIndex)
		}
		seen[shardIndex] = true
		snapshot := egressCoverageSnapshot{
			shardIndex: shardIndex, eligible: values[1],
			noLocationDue: values[2], staleLocationDue: values[3], staleHealthDue: values[4],
			staleLocationExpiredDue: values[5], staleHealthExpiredDue: values[6], blackholeDue: values[7],
			fullAgeSeconds: values[8], blackholeAgeSeconds: values[9],
			fullCurrent: values[10], blackholeCurrent: values[11], fullAttemptsLastHour: values[12],
			blackholeLastHour: values[13], staleLocationOldestAgeSeconds: values[14], staleHealthOldestAgeSeconds: values[15],
			missingHealthDue: values[16], missingHealthExpiredDue: values[17], missingHealthOldestAgeSeconds: values[18],
		}
		if snapshot.noLocationDue > snapshot.eligible || snapshot.staleLocationDue > snapshot.eligible ||
			snapshot.staleHealthDue > snapshot.eligible || snapshot.missingHealthDue > snapshot.eligible ||
			snapshot.fullDue() > snapshot.eligible ||
			snapshot.staleLocationExpiredDue > snapshot.staleLocationDue ||
			snapshot.staleHealthExpiredDue > snapshot.staleHealthDue ||
			snapshot.missingHealthExpiredDue > snapshot.missingHealthDue ||
			snapshot.blackholeDue > snapshot.eligible ||
			snapshot.fullCurrent > snapshot.eligible || snapshot.blackholeCurrent > snapshot.eligible ||
			snapshot.fullAttemptsLastHour > snapshot.eligible ||
			snapshot.blackholeLastHour > snapshot.blackholeCurrent ||
			(snapshot.staleLocationDue > 0 && snapshot.staleLocationOldestAgeSeconds < 0) ||
			(snapshot.staleHealthDue > 0 && snapshot.staleHealthOldestAgeSeconds < 0) ||
			(snapshot.missingHealthDue > 0 && snapshot.missingHealthOldestAgeSeconds < 0) {
			return nil, fmt.Errorf("provider egress activity returned contradictory shard counts")
		}
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].shardIndex < snapshots[j].shardIndex })
	return snapshots, nil
}

func egressBlackholeCapacityFinding(
	target string,
	geometry egressCoverageGeometry,
	snapshots []egressCoverageSnapshot,
) (finding, bool) {
	var eligible, current, checkedLastHour int64
	for _, snapshot := range snapshots {
		eligible += snapshot.eligible
		current += snapshot.blackholeCurrent
		checkedLastHour += snapshot.blackholeLastHour
	}
	if eligible == 0 || current >= eligible || checkedLastHour <= 0 {
		return finding{}, false
	}
	maxAgeSeconds := int64(model.ProviderBlackholeCheckMaxAge / time.Second)
	projectedSweepSeconds := (eligible*int64(time.Hour/time.Second) + checkedLastHour - 1) / checkedLastHour
	if projectedSweepSeconds <= maxAgeSeconds {
		return finding{}, false
	}
	requiredPerHour := (eligible*int64(time.Hour/time.Second) + maxAgeSeconds - 1) / maxAgeSeconds
	configuredBlackholeConcurrency := int64(geometry.shardCount) * int64(geometry.blackholeConcurrency)
	probeTimeoutSeconds := int64(geometry.blackholeTimeoutSeconds)
	blackholeOnlyTimeoutCeilingPerHour := configuredBlackholeConcurrency * int64(time.Hour/time.Second) / probeTimeoutSeconds
	blackholeOnlyDeadlineMinimumConcurrency := (requiredPerHour*probeTimeoutSeconds + int64(time.Hour/time.Second) - 1) / int64(time.Hour/time.Second)
	coveragePercent := 100 * float64(current) / float64(eligible)
	return finding{
		probeId: "pg/egress-coverage", tier: tierPage,
		class: "egress-blackhole-capacity", target: target, frame: "fleet-refresh", sustain: 2,
		symptom: fmt.Sprintf(
			"Provider blackhole checks cover %.1f%% of the eligible fleet, and the last-hour rate projects a %s sweep beyond the %s verdict lifetime.",
			coveragePercent, (time.Duration(projectedSweepSeconds) * time.Second).Round(time.Second), model.ProviderBlackholeCheckMaxAge,
		),
		mechanism: "Shard timestamps are advancing, but aggregate production is too slow to refresh the complete eligible population before verdicts expire. A known-dark provider therefore ages out of the exclusion set and becomes selectable again without a successful recheck; shard-local liveness alone cannot see this chronic under-capacity state. The blackhole-only slot calculation does not include residence time spent on full probes inside the same durable task.",
		baseline:  fmt.Sprintf("The measured one-hour blackhole-check rate is at least %d providers/hour, so one complete fleet sweep fits inside the %s verdict lifetime, or current coverage is already complete.", requiredPerHour, model.ProviderBlackholeCheckMaxAge),
		observed: fmt.Sprintf(
			"eligible=%d current=%d current_percent=%.1f checked_last_hour=%d required_per_hour=%d projected_sweep=%s verdict_max_age=%s configured_shards=%d configured_blackhole_concurrency_per_shard=%d configured_total_blackhole_concurrency=%d blackhole_probe_timeout_seconds=%d blackhole_only_timeout_ceiling_per_hour=%d blackhole_only_deadline_minimum_concurrency=%d configured_full_limit_per_shard=%d configured_full_concurrency_per_shard=%d full_probe_timeout_seconds=%d",
			eligible, current, coveragePercent, checkedLastHour, requiredPerHour,
			(time.Duration(projectedSweepSeconds) * time.Second).Round(time.Second), model.ProviderBlackholeCheckMaxAge,
			geometry.shardCount, geometry.blackholeConcurrency, configuredBlackholeConcurrency,
			probeTimeoutSeconds, blackholeOnlyTimeoutCeilingPerHour, blackholeOnlyDeadlineMinimumConcurrency,
			geometry.fullLimit, geometry.fullConcurrency, geometry.fullTimeoutSeconds,
		),
		evidence: "The query counts one latest row per eligible provider inside PostgreSQL and joins those aggregate rates only to the complete common execution geometry parsed from the durable task arguments. Provider, network, task, endpoint, and failure identities never leave the database.",
		context:  "This is a software execution-capacity and negative-evidence lifecycle boundary, not proof that Proxy hosts need more active-client hardware. A common timeout cohort can consume the full blackhole deadline and depress throughput. The blackhole-only timeout rate is not a whole-task ceiling when a running artifact serializes full work in the same shard; measured throughput remains authoritative because full-probe residence, setup, teardown, fast successes, and mixed failure latencies change the realized rate.",
		action:   "Run §2.23 and §2.24 first, then establish the running Taskworker's execution behavior. If full work blocks blackhole progress inside one shard task, deploy the architecture-preserving correction that overlaps one full batch with a repeated blackhole drain while reserving its configured concurrency; do not increase concurrency first. If independent drain is already present and the measured rate still misses the bound, capacity-test any geometry change against PostgreSQL/PgBouncer, API, and Taskworker CPU/memory headroom. Separately obtain an explicit correctness decision for retaining a failed verdict until a successful recheck; do not merely lengthen the max age, delete evidence, or suppress the provider gate.",
		verify:   "After convergence, for two complete verdict lifetimes every shard advances, current coverage reaches the complete eligible population, the measured hourly rate stays at or above the required rate, the projected sweep remains inside the verdict lifetime, known-dark providers never re-enter selection only because evidence aged, and healthy controls remain selectable. Keep more than 25% PostgreSQL normal-role headroom and verify PgBouncer, API, and Taskworker CPU/memory controls throughout the sustained duty cycle.",
		playbook: "SIGNALS.md §2.19, §2.23, and §2.24",
	}, true
}

func egressFullCapacityFinding(
	target string,
	geometry egressCoverageGeometry,
	snapshots []egressCoverageSnapshot,
) (finding, bool) {
	var eligible, current, due, attemptedLastHour int64
	for _, snapshot := range snapshots {
		eligible += snapshot.eligible
		current += snapshot.fullCurrent
		due += snapshot.fullDue()
		attemptedLastHour += snapshot.fullAttemptsLastHour
	}
	if eligible == 0 || due == 0 || attemptedLastHour <= 0 {
		return finding{}, false
	}

	maxAgeSeconds := int64(model.ProviderEgressLocationMaxAge / time.Second)
	projectedSweepSeconds := (due*int64(time.Hour/time.Second) + attemptedLastHour - 1) / attemptedLastHour
	if projectedSweepSeconds <= maxAgeSeconds {
		return finding{}, false
	}
	requiredPerHour := (due*int64(time.Hour/time.Second) + maxAgeSeconds - 1) / maxAgeSeconds
	coveragePercent := 100 * float64(current) / float64(eligible)
	configuredFullConcurrency := int64(geometry.shardCount) * int64(geometry.fullConcurrency)

	return finding{
		probeId: "pg/egress-coverage", tier: tierPage,
		class: "egress-full-capacity", target: target, frame: "fleet-refresh", sustain: 2,
		symptom: fmt.Sprintf(
			"Full probes cover %.1f%% of the eligible fleet, and %d due providers at the last-hour attempt rate project a %s drain beyond the %s location lifetime.",
			coveragePercent, due, (time.Duration(projectedSweepSeconds) * time.Second).Round(time.Second), model.ProviderEgressLocationMaxAge,
		),
		mechanism: "The durable shards are producing attempts, but their aggregate gross rate is insufficient to serve the current due population inside the existing seven-day evidence lifetime. Deadline ordering prevents stale location, stale health, or missing health from being starved by the unlocated lane; it cannot manufacture the missing probe throughput.",
		baseline:  fmt.Sprintf("With incomplete full coverage, the measured gross attempt rate is at least %d unique providers/hour, so the complete due population fits inside %s.", requiredPerHour, model.ProviderEgressLocationMaxAge),
		observed: fmt.Sprintf(
			"eligible=%d current=%d current_percent=%.1f due=%d attempted_last_hour=%d required_per_hour=%d projected_drain=%s location_max_age=%s configured_shards=%d configured_full_limit_per_shard=%d configured_full_concurrency_per_shard=%d configured_total_full_concurrency=%d full_probe_timeout_seconds=%d",
			eligible, current, coveragePercent, due, attemptedLastHour, requiredPerHour,
			(time.Duration(projectedSweepSeconds) * time.Second).Round(time.Second), model.ProviderEgressLocationMaxAge,
			geometry.shardCount, geometry.fullLimit, geometry.fullConcurrency, configuredFullConcurrency, geometry.fullTimeoutSeconds,
		),
		evidence: "PostgreSQL counts one latest attempt and one latest evidence row per eligible provider, partitions the mutually exclusive due categories inside the normalized shard hash, and exports aggregate counts only. Provider, network, task, endpoint, and failure identities never leave the database.",
		context:  "This is gross full-probe execution capacity, not proof of any one failure mechanism or a license to increase concurrency without resource gates. The attempt rate includes both successes and failures. The repository still has no product decision for a maximum retry interval or a capacity allocation between first attempts and retries, so this finding does not claim complete unlocated-lane fairness.",
		action:   "First converge the bounded deadline scheduler and independent blackhole drain, then measure the resulting full rate. If the projection still exceeds seven days, capacity-test a full-probe geometry or latency repair against PostgreSQL/PgBouncer, API, Taskworker, and Proxy headroom. Do not invent fixed lane weights, suppress retries, lengthen evidence lifetimes, or raise concurrency solely from this aggregate.",
		verify:   "After every Taskworker has converged, all shards advance for two cadences, the measured gross full-attempt rate stays at or above the live required rate, and the projected due drain remains inside seven days for a complete seven-day sweep. Preserve more than 25% PostgreSQL normal-role headroom and healthy API, Taskworker, Proxy, and PgBouncer controls.",
		playbook: "SIGNALS.md §2.19, §1.3a, §1.3b, §2.23, and §2.24",
	}, true
}

func egressFullFairnessFindings(
	target string,
	geometry egressCoverageGeometry,
	snapshots []egressCoverageSnapshot,
) []finding {
	allShardsUnlocatedSaturated := len(snapshots) > 0
	for _, snapshot := range snapshots {
		if snapshot.noLocationDue < int64(geometry.fullLimit) {
			allShardsUnlocatedSaturated = false
			break
		}
	}

	findings := []finding{}
	for _, snapshot := range snapshots {
		categories := []struct {
			name             string
			due              int64
			expiredDue       int64
			oldestAgeSeconds int64
			maxAge           time.Duration
		}{
			{
				name: "stale-location", due: snapshot.staleLocationDue,
				expiredDue:       snapshot.staleLocationExpiredDue,
				oldestAgeSeconds: snapshot.staleLocationOldestAgeSeconds,
				maxAge:           model.ProviderEgressLocationMaxAge,
			},
			{
				name: "stale-health", due: snapshot.staleHealthDue,
				expiredDue:       snapshot.staleHealthExpiredDue,
				oldestAgeSeconds: snapshot.staleHealthOldestAgeSeconds,
				maxAge:           model.ProviderEgressHealthMaxAge,
			},
			{
				name: "missing-health", due: snapshot.missingHealthDue,
				expiredDue:       snapshot.missingHealthExpiredDue,
				oldestAgeSeconds: snapshot.missingHealthOldestAgeSeconds,
				maxAge:           model.ProviderEgressHealthMaxAge,
			},
		}
		for _, category := range categories {
			if category.due == 0 {
				continue
			}
			maxAgeSeconds := int64(category.maxAge / time.Second)
			remainingSeconds := maxAgeSeconds - category.oldestAgeSeconds
			projectedSeconds := int64(-1)
			if 0 < snapshot.fullAttemptsLastHour {
				projectedSeconds = (category.due*int64(time.Hour/time.Second) + snapshot.fullAttemptsLastHour - 1) / snapshot.fullAttemptsLastHour
			}
			deadlineAtRisk := 0 <= projectedSeconds && remainingSeconds < projectedSeconds
			deadlineMissed := 0 < category.expiredDue
			if !deadlineMissed && !deadlineAtRisk {
				continue
			}

			projectedText := "unavailable_zero_gross_attempts"
			if 0 <= projectedSeconds {
				projectedText = (time.Duration(projectedSeconds) * time.Second).Round(time.Second).String()
			}
			frame := fmt.Sprintf("shard-%d-of-%d/%s", snapshot.shardIndex, geometry.shardCount, category.name)
			findings = append(findings, finding{
				probeId: "pg/egress-coverage", tier: tierPage,
				class: "egress-full-fairness", target: target, frame: frame, sustain: 2,
				symptom: fmt.Sprintf(
					"Full-probe category %s has %d due providers in %s, including %d past its absolute deadline; assigning every recent full attempt to this category projects %s.",
					category.name, category.due, frame, category.expiredDue, projectedText,
				),
				mechanism: "The category has crossed an existing absolute deadline, or cannot meet the oldest row's remaining window even under the optimistic assumption that every gross full-probe attempt serves it. Location and existing-health rows use their hard evidence expiries; missing health uses location observed_at plus the existing 24-hour health lifetime and does not claim that health evidence ever existed. This success-inclusive lower bound avoids treating successful probes that leave a category as missing progress. The corrected scheduler merges bounded indexed evidence heads by absolute deadline before filling from unlocated work.",
				baseline: fmt.Sprintf(
					"No due %s row has crossed its %s category deadline, and when gross attempts are measurable the optimistic all-capacity projection fits inside the oldest row's remaining window.",
					category.name, category.maxAge,
				),
				observed: fmt.Sprintf(
					"frame=%s category=%s due=%d expired_due=%d gross_full_attempted_last_hour=%d oldest_deadline_anchor_age=%s remaining_deadline_window=%s optimistic_all_capacity_drain=%s deadline_missed=%t deadline_at_risk=%t all_shards_unlocated_can_fill_batch=%t configured_full_limit_per_shard=%d",
					frame, category.name, category.due, category.expiredDue, snapshot.fullAttemptsLastHour,
					(time.Duration(category.oldestAgeSeconds) * time.Second).Round(time.Second),
					(time.Duration(remainingSeconds) * time.Second).Round(time.Second), projectedText,
					deadlineMissed, deadlineAtRisk, allShardsUnlocatedSaturated, geometry.fullLimit,
				),
				evidence: "The query separates no-location from present-location work, then assigns each present-location urgent row to the location, existing-health, or missing-health lane with the earliest absolute deadline; an exact location/health tie is stable in favor of location, matching the scheduler merge. It returns shard-local due, expired, oldest-anchor-age, and gross success-inclusive attempt aggregates only. Giving one category all gross attempts is deliberately optimistic; if that bound fails, no unknown allocation can make the deadline. No provider or task identifier leaves PostgreSQL.",
				context:  "The all-shards-unlocated shape is diagnostic only: it proves starvation when joined to a running API artifact with fixed pass precedence, but it is not itself a post-EDF failure. This finding is deadline preservation for already-evidenced providers, not complete scheduler fairness. First attempts and retries still share the unlocated lane, whose only defined contract is a six-hour minimum retry backoff; maximum retry delay or an allocation remains an explicit product decision.",
				action:   "Establish the running API scheduler behavior. If it retains fixed pass precedence and every unlocated head can fill the batch, apply the ordered health index migration and deploy the bounded EDF API correction; deploy the selective attempt cleanup with Taskworker. If EDF is already running, diagnose full capacity and category outcomes without inventing weights or deleting evidence.",
				verify:   "After API and Taskworker convergence, require expired_due=0 and an optimistic drain within each urgent category's remaining deadline window for two samples, preserve stale-health evidence through its 12-hour due-to-expiry window, keep missing-health at zero or advancing after attempt backoff, and complete a seven-day full sweep. Separately retain the open product decision and capacity gate for first attempts versus retries.",
				playbook: "SIGNALS.md §2.19, §2.23, and §2.24",
			})
		}
	}
	return findings
}

func egressCoverageStallFinding(target, frame, kind string, due, age, eligible, current, stallSeconds int64) finding {
	class := "egress-" + kind + "-stalled"
	evidenceName := kind + " probe"
	if kind == "blackhole" {
		evidenceName = "blackhole check"
	}
	ageText := "never"
	if age >= 0 {
		ageText = (time.Duration(age) * time.Second).Round(time.Second).String()
	}
	return finding{
		probeId: "pg/egress-coverage", tier: tierPage,
		class: class, target: target, frame: frame, sustain: 2,
		symptom:   fmt.Sprintf("Provider-egress %s has %d due candidates in %s but its newest aggregate evidence is %s old.", kind, due, frame, ageText),
		mechanism: "The durable shard exists, but due providers are not reaching a persisted probe outcome. Hash-local evidence prevents activity in healthy sibling shards from hiding a stalled slice of the fleet.",
		baseline:  fmt.Sprintf("When a shard has due work, its newest %s evidence is no older than max_time + idle_delay + one monitor cadence (%s).", evidenceName, (time.Duration(stallSeconds) * time.Second).String()),
		observed:  fmt.Sprintf("frame=%s eligible=%d due=%d current=%d newest_evidence_age=%s derived_stall_bound=%s", frame, eligible, due, current, ageText, (time.Duration(stallSeconds) * time.Second).String()),
		evidence:  "Counts and ages are aggregated inside the shard's normalized PostgreSQL hash partition; no provider or task identifier leaves the database.",
		context:   "This is a software execution or operational rollout failure. It does not establish a Proxy memory/hardware ceiling, and raising provider capacity cannot make a non-advancing task persist evidence.",
		action:    "Correlate the shard frame with ProviderEgressProbe task errors and bounded Taskworker logs. Repair authentication, API reachability, task claim, or probe execution as the evidence identifies; converge the intended Taskworker generation. Do not delete provider evidence or manually rewrite the recurring task.",
		verify:    "The affected shard's newest evidence advances inside the derived bound for two cadences, or its due count drains to zero, while generic task canaries remain healthy.",
		playbook:  "SIGNALS.md §2.19, §1.2, and §8.9",
	}
}
