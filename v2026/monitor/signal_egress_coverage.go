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
)

const providerEgressProbeTaskFunction = "github.com/urnetwork/server/v2026/taskworker/work.ProviderEgressProbe"

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
	shardCount       int
	idleDelaySeconds int
	maxTimeSeconds   int
	indices          []int
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
		SELECT EXISTS (
		 SELECT 1 FROM pg_attribute
		 WHERE attrelid='provider_egress_health'::regclass
		   AND attname='tls_authentication_failure'
		   AND NOT attisdropped
		);
	`)
	if err != nil {
		return nil, err
	}
	if len(schemaRows) != 1 || len(schemaRows[0]) != 1 {
		return nil, fmt.Errorf("provider egress coverage schema query returned an invalid shape")
	}
	tlsIntegrityArmed, err := strconv.ParseBool(schemaRows[0].str(0))
	if err != nil {
		return nil, fmt.Errorf("provider egress coverage returned invalid TLS-integrity arming state %q", schemaRows[0].str(0))
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
		if len(taskRows) == 0 {
			missing = append(missing, "durable ProviderEgressProbe tasks")
		}
		action := "Apply the append-only provider-egress migration from server commit 49b51eeb or later, then build and deploy a Taskworker artifact from an intentional checkout containing that commit. Let normal task initialization create the shards; do not insert, delete, or hand-edit pending_task rows."
		if tlsIntegrityArmed {
			action = "The append-only provider-egress schema is already armed. Build and deploy a Taskworker artifact from an intentional server checkout containing commit 49b51eeb, then let normal task initialization create the shards; do not repeat the migration or insert, delete, or hand-edit pending_task rows."
		}
		return []finding{{
			probeId: "pg/egress-coverage", tier: tierWarn,
			class: "egress-probe-unarmed", target: target, frame: "rollout", sustain: 2,
			symptom:   "The provider-egress pipeline is not fully armed: " + strings.Join(missing, " and ") + " are absent.",
			mechanism: "Provider scoring can only rely on egress evidence after the append-only integrity schema and the host-independent recurring task shards both exist. An empty task result is rollout absence, not proof that zero providers need measurement.",
			baseline:  "The TLS-integrity column exists and pending_task contains one internally consistent ProviderEgressProbe row for every configured shard.",
			observed:  fmt.Sprintf("tls_integrity_armed=%t provider_egress_task_rows=%d", tlsIntegrityArmed, len(taskRows)),
			evidence:  "Only schema presence and aggregate task-row count are exported; task IDs, client IDs, credentials, endpoints, and argument JSON remain private.",
			context:   "This is a software rollout/operational boundary, not a Proxy hardware-capacity alert. The generic task canary remains responsible for individual claim, timeout, and reschedule errors.",
			action:    action,
			verify:    "The schema is armed, every Taskworker is on the intended generation, all shard rows appear, and this signal observes fresh output or no due work for two cadences.",
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
		healthyFinding("pg/egress-coverage", tierWarn, "egress-probe-unarmed", target),
		healthyFinding("pg/egress-coverage", tierPage, "egress-probe-shards", target),
	}
	for _, snapshot := range activity {
		frame := fmt.Sprintf("shard-%d-of-%d", snapshot.shardIndex, geometry.shardCount)
		if snapshot.eligible > 0 && snapshot.fullDue > 0 && (snapshot.fullAgeSeconds < 0 || stallSeconds < snapshot.fullAgeSeconds) {
			findings = append(findings, egressCoverageStallFinding(
				target, frame, "full", snapshot.fullDue, snapshot.fullAgeSeconds,
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
		WITH shards AS (
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
		), snapshot AS (
		 SELECT s.shard_index,
		        count(e.client_id) AS eligible,
		        count(e.client_id) FILTER (WHERE
		          (pel.client_id IS NULL OR pel.observed_at < (now() AT TIME ZONE 'UTC') - interval '84 hours') AND
		          (pea.client_id IS NULL OR pea.attempt_at < (now() AT TIME ZONE 'UTC') - interval '6 hours')
		        ) AS full_due,
		        count(e.client_id) FILTER (WHERE
		          pbc.client_id IS NULL OR pbc.checked_at < (now() AT TIME ZONE 'UTC') - interval '90 minutes'
		        ) AS blackhole_due,
		        max(GREATEST(pel.observed_at, pea.attempt_at, peh.measured_at)) AS latest_full,
		        max(pbc.checked_at) AS latest_blackhole,
		        count(e.client_id) FILTER (WHERE pel.observed_at >= (now() AT TIME ZONE 'UTC') - interval '7 days') AS full_current,
		        count(e.client_id) FILTER (WHERE pbc.checked_at >= (now() AT TIME ZONE 'UTC') - interval '3 hours') AS blackhole_current
		 FROM shards s
		 LEFT JOIN eligible e USING (shard_index)
		 LEFT JOIN provider_egress_location pel USING (client_id)
		 LEFT JOIN provider_egress_probe_attempt pea USING (client_id)
		 LEFT JOIN provider_egress_health peh USING (client_id)
		 LEFT JOIN provider_blackhole_check pbc USING (client_id)
		 GROUP BY s.shard_index
		)
		SELECT shard_index::text, eligible::text, full_due::text, blackhole_due::text,
		       COALESCE(floor(extract(epoch FROM ((now() AT TIME ZONE 'UTC') - latest_full)))::bigint, -1)::text,
		       COALESCE(floor(extract(epoch FROM ((now() AT TIME ZONE 'UTC') - latest_blackhole)))::bigint, -1)::text,
		       full_current::text, blackhole_current::text
		FROM snapshot
		ORDER BY shard_index;
	`, shardCount, shardCount, shardCount, shardCount)
}

type egressCoverageSnapshot struct {
	shardIndex          int
	eligible            int64
	fullDue             int64
	blackholeDue        int64
	fullAgeSeconds      int64
	blackholeAgeSeconds int64
	fullCurrent         int64
	blackholeCurrent    int64
}

func parseEgressCoverageActivity(rows []pgRow, shardCount int) ([]egressCoverageSnapshot, error) {
	if len(rows) != shardCount {
		return nil, fmt.Errorf("provider egress activity returned %d shard rows, want %d", len(rows), shardCount)
	}
	snapshots := make([]egressCoverageSnapshot, 0, shardCount)
	seen := map[int]bool{}
	for _, row := range rows {
		if len(row) != 8 {
			return nil, fmt.Errorf("provider egress activity returned an invalid row shape")
		}
		values := make([]int64, 8)
		for i := range row {
			value, err := strconv.ParseInt(strings.TrimSpace(row.str(i)), 10, 64)
			if err != nil || (i != 4 && i != 5 && value < 0) || ((i == 4 || i == 5) && value < -1) {
				return nil, fmt.Errorf("provider egress activity returned invalid numeric field %d", i)
			}
			values[i] = value
		}
		shardIndex := int(values[0])
		if shardIndex < 0 || shardCount <= shardIndex || seen[shardIndex] {
			return nil, fmt.Errorf("provider egress activity returned invalid shard index %d", shardIndex)
		}
		seen[shardIndex] = true
		snapshots = append(snapshots, egressCoverageSnapshot{
			shardIndex: shardIndex, eligible: values[1], fullDue: values[2], blackholeDue: values[3],
			fullAgeSeconds: values[4], blackholeAgeSeconds: values[5], fullCurrent: values[6], blackholeCurrent: values[7],
		})
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].shardIndex < snapshots[j].shardIndex })
	return snapshots, nil
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
