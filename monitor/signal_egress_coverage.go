package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/server"
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

type egressCoverageProbe struct {
	// The per-probe seam keeps synthetic observations independent of the
	// workstation's active Config resource. Production reloads it each cadence.
	loadDesiredConfig func() egressCoverageDesiredConfig
}

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
	settings                egressCoverageConfig
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

type egressCoverageDesiredConfig struct {
	present       bool
	enabled       bool
	invalidReason string
	settings      egressCoverageConfig
}

// Positive/default-dependent settings must be explicit before the monitor
// compares them. In particular, absent config is not the Taskworker's built-in
// four-slot geometry, and an omitted full-bandwidth flag is not false. The
// current zero-valued optional batch modes retain their serialization defaults.
type egressCoverageDesiredBatchYAML struct {
	Limit                   int            `yaml:"limit"`
	Concurrency             int            `yaml:"concurrency"`
	ProbeTimeoutSeconds     int            `yaml:"probe_timeout_seconds"`
	AllDestinations         bool           `yaml:"all_destinations"`
	Bandwidth               *bool          `yaml:"bandwidth"`
	BandwidthTimeoutSeconds *int           `yaml:"bandwidth_timeout_seconds"`
	Unknown                 map[string]any `yaml:",inline"`
}

func (batch egressCoverageDesiredBatchYAML) args() egressCoverageBatchArgs {
	args := egressCoverageBatchArgs{
		Limit: batch.Limit, Concurrency: batch.Concurrency,
		ProbeTimeoutSeconds: batch.ProbeTimeoutSeconds, AllDestinations: batch.AllDestinations,
	}
	if batch.Bandwidth != nil {
		args.Bandwidth = *batch.Bandwidth
	}
	if batch.BandwidthTimeoutSeconds != nil {
		args.BandwidthTimeoutSeconds = *batch.BandwidthTimeoutSeconds
	}
	return args
}

type egressCoverageDesiredYAML struct {
	Enabled          *bool                          `yaml:"enabled"`
	ShardCount       int                            `yaml:"shard_count"`
	IdleDelaySeconds int                            `yaml:"idle_delay_seconds"`
	MaxTimeSeconds   int                            `yaml:"max_time_seconds"`
	Full             egressCoverageDesiredBatchYAML `yaml:"full"`
	Blackhole        egressCoverageDesiredBatchYAML `yaml:"blackhole"`
	APIURL           string                         `yaml:"api_url"`
	PlatformURL      string                         `yaml:"platform_url"`
	PublicAPIURL     *string                        `yaml:"public_api_url"`
	BandwidthCDNURL  *string                        `yaml:"bandwidth_cdn_url"`
	Unknown          map[string]any                 `yaml:",inline"`
}

func loadEgressCoverageDesiredConfig() egressCoverageDesiredConfig {
	resource, err := server.Config.SimpleResource("provider_egress_probe.yml")
	if err != nil {
		if errors.Is(err, server.ErrResourceNotFound) {
			// This resource is optional. Do not manufacture desired settings from
			// either source defaults or the rows being checked, or claim a match.
			return egressCoverageDesiredConfig{}
		}
		return egressCoverageDesiredConfig{present: true, invalidReason: "resource-unavailable"}
	}
	return inspectEgressCoverageDesiredConfig(resource.UnmarshalYamlE)
}

func inspectEgressCoverageDesiredConfig(load func(any) error) egressCoverageDesiredConfig {
	desired := egressCoverageDesiredConfig{present: true}
	var raw egressCoverageDesiredYAML
	if err := load(&raw); err != nil {
		desired.invalidReason = "resource-unreadable-or-malformed"
		return desired
	}
	if raw.Enabled == nil {
		desired.invalidReason = "enabled-state-unavailable"
		return desired
	}
	desired.enabled = *raw.Enabled
	if !desired.enabled {
		// Taskworker deliberately does not validate inactive execution settings.
		return desired
	}
	if len(raw.Unknown)+len(raw.Full.Unknown)+len(raw.Blackhole.Unknown) > 0 {
		desired.invalidReason = "unknown-execution-setting"
		return desired
	}
	if raw.Full.Bandwidth == nil || raw.Full.BandwidthTimeoutSeconds == nil ||
		raw.PublicAPIURL == nil || raw.BandwidthCDNURL == nil {
		desired.invalidReason = "default-dependent-execution-settings"
		return desired
	}
	desired.settings = egressCoverageConfig{
		shardCount: raw.ShardCount, idleDelaySeconds: raw.IdleDelaySeconds, maxTimeSeconds: raw.MaxTimeSeconds,
		full: raw.Full.args(), blackhole: raw.Blackhole.args(),
		apiURL: raw.APIURL, platformURL: raw.PlatformURL,
		publicAPIURL: *raw.PublicAPIURL, bandwidthCDNURL: *raw.BandwidthCDNURL,
	}
	if !validEgressCoverageConfig(desired.settings) {
		desired.invalidReason = "invalid-or-incomplete-execution-settings"
	}
	return desired
}

func validEgressCoverageConfig(config egressCoverageConfig) bool {
	if !(1 <= config.shardCount && config.shardCount <= 256 &&
		0 < config.idleDelaySeconds && 0 < config.maxTimeSeconds &&
		validEgressCoverageBatchArgs(config.full) && validEgressCoverageBatchArgs(config.blackhole) &&
		strings.TrimSpace(config.apiURL) != "" && strings.TrimSpace(config.platformURL) != "") {
		return false
	}
	probeTimeout := time.Duration(config.full.ProbeTimeoutSeconds) * time.Second
	return fleetprobe.EgressHealthOptions(probeTimeout, config.full.AllDestinations).PerRequestTimeout >= egresshealth.DefaultPerRequestTimeout
}

const egressCoverageConfigConvergenceAction = "Deploy config-updater first and verify that the desired configuration version is completely published; then deploy Taskworker so every executor mounts that completed version. Let successful ProviderEgressProbe post-steps replace the four-or-configured-count durable snapshots, or normal disabled-task cleanup retire them when disabled. Failed executions retry their old arguments, and an old-config worker can recreate stale settings. Do not insert, delete, or hand-edit pending_task rows."

func egressCoverageConfigFindings(target string, desired egressCoverageDesiredConfig, rowCount int, geometry egressCoverageGeometry, geometryErr error) []finding {
	if !desired.present {
		return nil
	}
	invalidReason := desired.invalidReason
	if invalidReason == "" && desired.enabled && rowCount > 0 && geometryErr != nil {
		invalidReason = "durable-geometry-unavailable"
	}
	if invalidReason != "" {
		return []finding{{
			probeId: "pg/egress-coverage", tier: tierWarn,
			class: "egress-probe-config-unobservable", target: target, frame: "desired-config", sustain: 2,
			symptom:   "Desired provider-egress configuration cannot be compared with one complete durable execution snapshot.",
			mechanism: "A present but unreadable, malformed, incomplete, or unknown desired setting, or mixed durable geometry, cannot establish either agreement or drift. Built-in defaults must not be mistaken for the active desired configuration.",
			baseline:  "An optional resource may be absent; when present, explicit enablement and complete observable execution settings can be compared with internally coherent durable rows.",
			observed:  fmt.Sprintf("desired_resource_present=true config_observation=%s provider_egress_task_rows=%d", invalidReason, rowCount),
			evidence:  "Only a fixed structural reason and aggregate row count are exported; parser errors, resource paths, endpoints, raw YAML/JSON, credentials and task identities are never rendered.",
			context:   "This is an observation gap, not proof of configuration drift or recovery. Existing schema, shard, liveness, fairness and measured-rate findings remain independent.",
			action:    "Restore the active provider_egress_probe.yml resource and its complete execution-setting contract, or converge mixed durable rows through the normal deployment/post-step path. Never copy runtime rows into desired state to silence the comparison. " + egressCoverageConfigConvergenceAction,
			verify:    "The resource becomes observable and the complete durable geometry is available; then verify explicit desired-versus-durable agreement for two cadences without suppressing independent capacity findings.",
			playbook:  "SIGNALS.md §2.19",
		}}
	}
	findings := []finding{healthyFinding("pg/egress-coverage", tierWarn, "egress-probe-config-unobservable", target)}
	changes := []string{}
	if desired.enabled != (rowCount > 0) {
		changes = append(changes, "enabled")
	}
	observed := fmt.Sprintf("desired_resource_present=true desired_enabled=%t durable_tasks_present=%t provider_egress_task_rows=%d", desired.enabled, rowCount > 0, rowCount)
	if desired.enabled {
		observed += " " + egressCoverageSafeSettings("desired", desired.settings)
		if rowCount > 0 {
			changes = append(changes, egressCoverageConfigChanges(desired.settings, geometry.settings)...)
			observed += " " + egressCoverageSafeSettings("durable", geometry.settings)
		}
	}
	if len(changes) == 0 {
		return append(findings, healthyFinding("pg/egress-coverage", tierWarn, "egress-probe-config-drift", target))
	}
	return append(findings, finding{
		probeId: "pg/egress-coverage", tier: tierWarn,
		class: "egress-probe-config-drift", target: target, frame: "desired-config", sustain: 2,
		symptom:   "The active desired provider-egress configuration differs from the durable task execution settings.",
		mechanism: "Each RunOnce shard stores an immutable argument snapshot. Reinitialization merges scheduling metadata, not args_json; a successful post-step reloads the worker's mounted configuration for its successor. A clean config checkout alone does not update mounted versions or retrying durable rows.",
		baseline:  "Explicit disabled state has no recurring probe rows; enabled state has one complete common durable geometry matching every desired execution setting.",
		observed:  observed + " mismatched_fields=" + strings.Join(changes, ","),
		evidence:  "Only enablement, aggregate counts, safe execution scalars and fixed mismatch names are exported. Endpoint equality is compared in memory; endpoint values, credentials, raw YAML/JSON and task identities never enter the alert.",
		context:   "The monitor's active resource is desired state, not proof of any worker's mounted configuration or executable capability. A mixed or malformed durable snapshot remains a separate shard PAGE and cannot be called converged. Measured-rate capacity remains independently authoritative.",
		action:    egressCoverageConfigConvergenceAction,
		verify:    "Prove the completed config version and independent-drain Taskworker artifact on every executor, then observe all configured shards adopt matching successor settings (or retire when explicitly disabled) for two cadences. For capacity changes, only after complete convergence begin two three-hour verdict lifetimes of measured-rate/coverage verification with more than 25% PostgreSQL headroom and healthy PgBouncer, API and Taskworker CPU/memory controls.",
		playbook:  "SIGNALS.md §2.19, §2.23, and §2.24",
	})
}

func egressCoverageConfigChanges(desired, durable egressCoverageConfig) []string {
	changes := []string{}
	note := func(name string, want, have any) {
		if want != have {
			changes = append(changes, name)
		}
	}
	note("shard_count", desired.shardCount, durable.shardCount)
	note("idle_delay_seconds", desired.idleDelaySeconds, durable.idleDelaySeconds)
	note("max_time_seconds", desired.maxTimeSeconds, durable.maxTimeSeconds)
	for _, batch := range []struct {
		name       string
		want, have egressCoverageBatchArgs
	}{{"full", desired.full, durable.full}, {"blackhole", desired.blackhole, durable.blackhole}} {
		note(batch.name+".limit", batch.want.Limit, batch.have.Limit)
		note(batch.name+".concurrency", batch.want.Concurrency, batch.have.Concurrency)
		note(batch.name+".probe_timeout_seconds", batch.want.ProbeTimeoutSeconds, batch.have.ProbeTimeoutSeconds)
		note(batch.name+".all_destinations", batch.want.AllDestinations, batch.have.AllDestinations)
		note(batch.name+".bandwidth", batch.want.Bandwidth, batch.have.Bandwidth)
		note(batch.name+".bandwidth_timeout_seconds", batch.want.BandwidthTimeoutSeconds, batch.have.BandwidthTimeoutSeconds)
	}
	// Names are fixed; values are deliberately excluded even from diagnostics.
	note("api_url", desired.apiURL, durable.apiURL)
	note("platform_url", desired.platformURL, durable.platformURL)
	note("public_api_url", desired.publicAPIURL, durable.publicAPIURL)
	note("bandwidth_cdn_url", desired.bandwidthCDNURL, durable.bandwidthCDNURL)
	return changes
}

func egressCoverageSafeSettings(prefix string, config egressCoverageConfig) string {
	fields := []string{fmt.Sprintf("%s_shards=%d %s_idle_delay_seconds=%d %s_max_time_seconds=%d", prefix, config.shardCount, prefix, config.idleDelaySeconds, prefix, config.maxTimeSeconds)}
	for _, batch := range []struct {
		name string
		args egressCoverageBatchArgs
	}{{"full", config.full}, {"blackhole", config.blackhole}} {
		name := prefix + "_" + batch.name
		fields = append(fields, fmt.Sprintf("%s_limit=%d %s_concurrency_per_shard=%d %s_probe_timeout_seconds=%d %s_all_destinations=%t %s_bandwidth=%t %s_bandwidth_timeout_seconds=%d", name, batch.args.Limit, name, batch.args.Concurrency, name, batch.args.ProbeTimeoutSeconds, name, batch.args.AllDestinations, name, batch.args.Bandwidth, name, batch.args.BandwidthTimeoutSeconds))
	}
	return strings.Join(fields, " ")
}

func (p egressCoverageProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
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
	loadDesired := p.loadDesiredConfig
	if loadDesired == nil {
		loadDesired = loadEgressCoverageDesiredConfig
	}
	desired := loadDesired()
	geometry, geometryErr := inspectEgressCoverageTasks(taskRows)
	configFindings := egressCoverageConfigFindings(target, desired, len(taskRows), geometry, geometryErr)
	if desired.present && desired.invalidReason == "" && !desired.enabled && len(taskRows) == 0 {
		// Explicit disablement makes zero rows intentional, not an unarmed
		// rollout. Lingering rows still pass through shard integrity and activity
		// checks until normal disabled-task cleanup has actually retired them.
		return configFindings, nil
	}
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
		return append(configFindings, finding{
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
		}), nil
	}

	if geometryErr != nil {
		return append(configFindings, finding{
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
		}), nil
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
	findings := append(configFindings,
		healthyFinding("pg/egress-coverage", tierPage, "egress-probe-shards", target),
	)
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
				snapshot.eligible, snapshot.fullCurrent, stallSeconds, snapshot.deferredCurrentDarkDue,
			))
		} else {
			findings = append(findings, healthyFinding("pg/egress-coverage", tierPage, "egress-full-stalled", target))
		}
		if snapshot.eligible > 0 && snapshot.blackholeDue > 0 && (snapshot.blackholeAgeSeconds < 0 || stallSeconds < snapshot.blackholeAgeSeconds) {
			findings = append(findings, egressCoverageStallFinding(
				target, frame, "blackhole", snapshot.blackholeDue, snapshot.blackholeAgeSeconds,
				snapshot.eligible, snapshot.blackholeCurrent, stallSeconds, 0,
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
		if !validEgressCoverageConfig(config) || args.ShardIndex < 0 || config.shardCount <= args.ShardIndex {
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
			geometry.settings = config
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

// Deadline feasibility depends on every cumulative deadline prefix, not the
// complete category count compared with only its oldest row. All timestamps and
// prefix counts stay in PostgreSQL; output remains three scalars per shard.
const egressCoverageDeadlineCTEs = `,
		deadline_counts AS (
		 SELECT c.shard_index, c.urgent_lane,
		        CASE c.urgent_lane
		          WHEN 'stale-location' THEN c.observed_at + interval '7 days'
		          WHEN 'stale-health' THEN c.measured_at + interval '24 hours'
		          WHEN 'missing-health' THEN c.observed_at + interval '24 hours'
		        END AS deadline_at,
		        count(*) AS due_at_deadline
		 FROM classified c
		 WHERE c.urgent_lane IN ('stale-location', 'stale-health', 'missing-health')
		   AND c.attempt_due AND NOT c.current_dark
		 GROUP BY c.shard_index, c.urgent_lane, deadline_at
		), deadline_prefixes AS (
		 SELECT shard_index, urgent_lane, deadline_at,
		        sum(due_at_deadline) OVER (
		          PARTITION BY shard_index, urgent_lane ORDER BY deadline_at
		          ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		        ) AS prefix_due
		 FROM deadline_counts
		), deadline_slack AS (
		 SELECT p.shard_index,
		        min(floor(extract(epoch FROM (p.deadline_at - s.now_utc)) -
		          p.prefix_due * 3600::numeric / NULLIF(s.full_attempted_last_hour, 0))::bigint)
		          FILTER (WHERE p.urgent_lane = 'stale-location') AS stale_location_deadline_slack_seconds,
		        min(floor(extract(epoch FROM (p.deadline_at - s.now_utc)) -
		          p.prefix_due * 3600::numeric / NULLIF(s.full_attempted_last_hour, 0))::bigint)
		          FILTER (WHERE p.urgent_lane = 'stale-health') AS stale_health_deadline_slack_seconds,
		        min(floor(extract(epoch FROM (p.deadline_at - s.now_utc)) -
		          p.prefix_due * 3600::numeric / NULLIF(s.full_attempted_last_hour, 0))::bigint)
		          FILTER (WHERE p.urgent_lane = 'missing-health') AS missing_health_deadline_slack_seconds
		 FROM deadline_prefixes p
		 JOIN snapshot s USING (shard_index)
		 GROUP BY p.shard_index
		)`

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
		        COALESCE(
		          pbc.ok = false AND
		          pbc.checked_at >= lifecycle_clock.now_utc - interval '3 hours',
		          false
		        ) AS current_dark,
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
		        count(c.client_id) FILTER (WHERE c.no_location AND c.attempt_due AND NOT c.current_dark) AS no_location_due,
		        count(c.client_id) FILTER (WHERE c.urgent_lane = 'stale-location' AND c.attempt_due AND NOT c.current_dark) AS stale_location_due,
		        count(c.client_id) FILTER (WHERE c.urgent_lane = 'stale-health' AND c.attempt_due AND NOT c.current_dark) AS stale_health_due,
		        count(c.client_id) FILTER (WHERE
		          c.urgent_lane = 'stale-location' AND c.attempt_due AND
		          NOT c.current_dark AND
		          c.observed_at < c.now_utc - interval '7 days'
		        ) AS stale_location_expired_due,
		        count(c.client_id) FILTER (WHERE
		          c.urgent_lane = 'stale-health' AND c.attempt_due AND
		          NOT c.current_dark AND
		          c.measured_at < c.now_utc - interval '24 hours'
		        ) AS stale_health_expired_due,
		        count(c.client_id) FILTER (WHERE
		          c.checked_at IS NULL OR c.checked_at < c.now_utc - interval '90 minutes'
		        ) AS blackhole_due,
		        max(GREATEST(c.observed_at, c.attempt_at, c.measured_at))
		          FILTER (WHERE NOT c.current_dark) AS latest_full,
		        max(c.checked_at) AS latest_blackhole,
		        count(c.client_id) FILTER (WHERE c.observed_at >= c.now_utc - interval '7 days') AS full_current,
		        count(c.client_id) FILTER (WHERE c.checked_at >= c.now_utc - interval '3 hours') AS blackhole_current,
		        count(c.client_id) FILTER (WHERE c.attempt_at >= c.now_utc - interval '1 hour') AS full_attempted_last_hour,
		        count(c.client_id) FILTER (WHERE c.checked_at >= c.now_utc - interval '1 hour') AS blackhole_checked_last_hour,
		        min(c.observed_at) FILTER (WHERE c.urgent_lane = 'stale-location' AND c.attempt_due AND NOT c.current_dark) AS oldest_stale_location,
		        min(c.measured_at) FILTER (WHERE c.urgent_lane = 'stale-health' AND c.attempt_due AND NOT c.current_dark) AS oldest_stale_health,
		        count(c.client_id) FILTER (WHERE c.urgent_lane = 'missing-health' AND c.attempt_due AND NOT c.current_dark) AS missing_health_due,
		        count(c.client_id) FILTER (WHERE
		          c.urgent_lane = 'missing-health' AND c.attempt_due AND
		          NOT c.current_dark AND
		          c.observed_at < c.now_utc - interval '24 hours'
		        ) AS missing_health_expired_due,
		        min(c.observed_at) FILTER (WHERE c.urgent_lane = 'missing-health' AND c.attempt_due AND NOT c.current_dark) AS oldest_missing_health_anchor,
		        count(c.client_id) FILTER (WHERE
		          c.current_dark AND c.attempt_due AND c.urgent_lane <> ''
		        ) AS deferred_current_dark_due,
		        max(c.now_utc) AS now_utc
		 FROM shards s
		 LEFT JOIN classified c USING (shard_index)
		 GROUP BY s.shard_index
		)%s
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
		       COALESCE(floor(extract(epoch FROM (now_utc - oldest_missing_health_anchor)))::bigint, -1)::text,
		       deferred_current_dark_due::text,
		       COALESCE(stale_location_deadline_slack_seconds::text, 'unavailable'),
		       COALESCE(stale_health_deadline_slack_seconds::text, 'unavailable'),
		       COALESCE(missing_health_deadline_slack_seconds::text, 'unavailable')
		FROM snapshot
		LEFT JOIN deadline_slack USING (shard_index)
		ORDER BY shard_index;
	`, shardCount, shardCount, shardCount, shardCount, egressCoverageDeadlineCTEs)
}

type egressCoverageSnapshot struct {
	shardIndex                        int
	eligible                          int64
	noLocationDue                     int64
	staleLocationDue                  int64
	staleHealthDue                    int64
	staleLocationExpiredDue           int64
	staleHealthExpiredDue             int64
	blackholeDue                      int64
	fullAgeSeconds                    int64
	blackholeAgeSeconds               int64
	fullCurrent                       int64
	blackholeCurrent                  int64
	fullAttemptsLastHour              int64
	blackholeLastHour                 int64
	staleLocationOldestAgeSeconds     int64
	staleHealthOldestAgeSeconds       int64
	missingHealthDue                  int64
	missingHealthExpiredDue           int64
	missingHealthOldestAgeSeconds     int64
	deferredCurrentDarkDue            int64
	staleLocationDeadlineSlackSeconds *int64
	staleHealthDeadlineSlackSeconds   *int64
	missingHealthDeadlineSlackSeconds *int64
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
		if len(row) != 23 {
			return nil, fmt.Errorf("provider egress activity returned an invalid row shape")
		}
		values := make([]int64, 20)
		for i := range values {
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
			deferredCurrentDarkDue: values[19],
		}
		if snapshot.noLocationDue > snapshot.eligible || snapshot.staleLocationDue > snapshot.eligible ||
			snapshot.staleHealthDue > snapshot.eligible || snapshot.missingHealthDue > snapshot.eligible ||
			snapshot.fullDue() > snapshot.eligible ||
			snapshot.deferredCurrentDarkDue > snapshot.eligible ||
			snapshot.fullDue()+snapshot.deferredCurrentDarkDue > snapshot.eligible ||
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
		for i, category := range []struct {
			due   int64
			slack **int64
		}{
			{snapshot.staleLocationDue, &snapshot.staleLocationDeadlineSlackSeconds},
			{snapshot.staleHealthDue, &snapshot.staleHealthDeadlineSlackSeconds},
			{snapshot.missingHealthDue, &snapshot.missingHealthDeadlineSlackSeconds},
		} {
			field := strings.TrimSpace(row.str(20 + i))
			expectSlack := category.due > 0 && snapshot.fullAttemptsLastHour > 0
			if field == "unavailable" {
				if expectSlack {
					return nil, fmt.Errorf("provider egress activity omitted deadline-prefix slack for category %d", i)
				}
				continue
			}
			value, err := strconv.ParseInt(field, 10, 64)
			if err != nil || !expectSlack {
				return nil, fmt.Errorf("provider egress activity returned invalid deadline-prefix slack for category %d", i)
			}
			*category.slack = &value
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
	reservedEvidence := "independent_drain_overlap_possible=false full_reserved_blackhole_concurrency_per_shard=unavailable full_reserved_total_blackhole_concurrency=unavailable full_reserved_timeout_ceiling_per_hour=unavailable"
	if geometry.fullConcurrency < geometry.blackholeConcurrency {
		reservedPerShard := int64(geometry.blackholeConcurrency - geometry.fullConcurrency)
		reservedTotal := int64(geometry.shardCount) * reservedPerShard
		reservedEvidence = fmt.Sprintf("independent_drain_overlap_possible=true full_reserved_blackhole_concurrency_per_shard=%d full_reserved_total_blackhole_concurrency=%d full_reserved_timeout_ceiling_per_hour=%d", reservedPerShard, reservedTotal, reservedTotal*int64(time.Hour/time.Second)/probeTimeoutSeconds)
	}
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
			"eligible=%d current=%d current_percent=%.1f checked_last_hour=%d required_per_hour=%d projected_sweep=%s verdict_max_age=%s configured_shards=%d configured_blackhole_concurrency_per_shard=%d configured_total_blackhole_concurrency=%d blackhole_probe_timeout_seconds=%d blackhole_only_timeout_ceiling_per_hour=%d blackhole_only_deadline_minimum_concurrency=%d configured_full_limit_per_shard=%d configured_full_concurrency_per_shard=%d full_probe_timeout_seconds=%d %s",
			eligible, current, coveragePercent, checkedLastHour, requiredPerHour,
			(time.Duration(projectedSweepSeconds) * time.Second).Round(time.Second), model.ProviderBlackholeCheckMaxAge,
			geometry.shardCount, geometry.blackholeConcurrency, configuredBlackholeConcurrency,
			probeTimeoutSeconds, blackholeOnlyTimeoutCeilingPerHour, blackholeOnlyDeadlineMinimumConcurrency,
			geometry.fullLimit, geometry.fullConcurrency, geometry.fullTimeoutSeconds,
			reservedEvidence,
		),
		evidence: "The query counts one latest row per eligible provider inside PostgreSQL and joins those aggregate rates only to the complete common execution geometry parsed from the durable task arguments. Provider, network, task, endpoint, and failure identities never leave the database.",
		context:  "This is a software execution-capacity and negative-evidence lifecycle boundary, not proof that Proxy hosts need more active-client hardware. A common timeout cohort can consume the full blackhole deadline and depress throughput. On an independent-drain artifact, while both queues are due, Full.Concurrency is reserved from Blackhole.Concurrency, so the full_reserved figures model the smaller effective blackhole pool without adding full slots to the configured peak. When full concurrency is not smaller, overlap is unavailable rather than a negative or zero throughput claim. These are conditional sizing models, not runtime capability or queue-activity attestations. The blackhole-only timeout rate is not a whole-task ceiling when a running artifact serializes full work in the same shard; measured throughput remains authoritative because full-probe residence, setup, teardown, fast successes, and mixed failure latencies change the realized rate.",
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
	var eligible, current, due, attemptedLastHour, deferredCurrentDarkDue int64
	for _, snapshot := range snapshots {
		eligible += snapshot.eligible
		current += snapshot.fullCurrent
		due += snapshot.fullDue()
		attemptedLastHour += snapshot.fullAttemptsLastHour
		deferredCurrentDarkDue += snapshot.deferredCurrentDarkDue
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
		mechanism: "The durable shards are producing attempts, but their aggregate gross rate is insufficient to serve the current non-dark due population inside the existing seven-day evidence lifetime. A current explicit blackhole failure is deferred to the independent cheap recovery queue instead of spending a full slot on a tunnel already proven unable to carry any destination. Deadline ordering prevents stale location, stale health, or missing health from being starved by the unlocated lane; it cannot manufacture the remaining probe throughput.",
		baseline:  fmt.Sprintf("With incomplete full coverage, the measured gross attempt rate is at least %d unique providers/hour, so the complete due population fits inside %s.", requiredPerHour, model.ProviderEgressLocationMaxAge),
		observed: fmt.Sprintf(
			"eligible=%d current=%d current_percent=%.1f due=%d deferred_current_dark_due=%d attempted_last_hour=%d required_per_hour=%d projected_drain=%s location_max_age=%s configured_shards=%d configured_full_limit_per_shard=%d configured_full_concurrency_per_shard=%d configured_total_full_concurrency=%d full_probe_timeout_seconds=%d",
			eligible, current, coveragePercent, due, deferredCurrentDarkDue, attemptedLastHour, requiredPerHour,
			(time.Duration(projectedSweepSeconds) * time.Second).Round(time.Second), model.ProviderEgressLocationMaxAge,
			geometry.shardCount, geometry.fullLimit, geometry.fullConcurrency, configuredFullConcurrency, geometry.fullTimeoutSeconds,
		),
		evidence: "PostgreSQL counts one latest attempt and one latest evidence row per eligible provider, partitions the mutually exclusive non-dark due categories inside the normalized shard hash, and exports aggregate counts only. Current-dark rows remain visible only as one deferred aggregate. Provider, network, task, endpoint, and failure identities never leave the database.",
		context:  "This is gross full-probe execution capacity, not proof of any one failure mechanism or a license to increase concurrency without resource gates. deferred_current_dark_due is desired scheduler-state accounting and becomes evidence of deployed suppression only after the API artifact converges; before then it is a candidate suppression opportunity, not proof of running queue behavior. The gross attempt rate deliberately includes both successes and failures, including old-deployment attempts against rows now dark, for rollout comparability. The repository still has no product decision for a maximum retry interval or a capacity allocation between first attempts and retries, so this finding does not claim complete unlocated-lane fairness.",
		action:   "First prove that every API artifact contains the current-dark full-queue exclusion, then converge the bounded deadline scheduler and independent blackhole drain and measure the resulting full rate. If the projection still exceeds seven days after the one-hour gross-rate rollout window clears, capacity-test a full-probe geometry or latency repair against PostgreSQL/PgBouncer, API, Taskworker, and Proxy headroom. Do not invent fixed lane weights, suppress retries, lengthen evidence lifetimes, or raise concurrency solely from this aggregate.",
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
			name                 string
			due                  int64
			expiredDue           int64
			oldestAgeSeconds     int64
			maxAge               time.Duration
			deadlineSlackSeconds *int64
		}{
			{
				name: "stale-location", due: snapshot.staleLocationDue,
				expiredDue:           snapshot.staleLocationExpiredDue,
				oldestAgeSeconds:     snapshot.staleLocationOldestAgeSeconds,
				maxAge:               model.ProviderEgressLocationMaxAge,
				deadlineSlackSeconds: snapshot.staleLocationDeadlineSlackSeconds,
			},
			{
				name: "stale-health", due: snapshot.staleHealthDue,
				expiredDue:           snapshot.staleHealthExpiredDue,
				oldestAgeSeconds:     snapshot.staleHealthOldestAgeSeconds,
				maxAge:               model.ProviderEgressHealthMaxAge,
				deadlineSlackSeconds: snapshot.staleHealthDeadlineSlackSeconds,
			},
			{
				name: "missing-health", due: snapshot.missingHealthDue,
				expiredDue:           snapshot.missingHealthExpiredDue,
				oldestAgeSeconds:     snapshot.missingHealthOldestAgeSeconds,
				maxAge:               model.ProviderEgressHealthMaxAge,
				deadlineSlackSeconds: snapshot.missingHealthDeadlineSlackSeconds,
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
			deadlineAtRisk := category.deadlineSlackSeconds != nil && *category.deadlineSlackSeconds < 0
			deadlineMissed := 0 < category.expiredDue
			if !deadlineMissed && !deadlineAtRisk {
				continue
			}

			projectedText := "unavailable_zero_gross_attempts"
			if 0 <= projectedSeconds {
				projectedText = (time.Duration(projectedSeconds) * time.Second).Round(time.Second).String()
			}
			slackText := "unavailable_zero_gross_attempts"
			if category.deadlineSlackSeconds != nil {
				slackText = strconv.FormatInt(*category.deadlineSlackSeconds, 10)
			}
			frame := fmt.Sprintf("shard-%d-of-%d/%s", snapshot.shardIndex, geometry.shardCount, category.name)
			findings = append(findings, finding{
				probeId: "pg/egress-coverage", tier: tierPage,
				class: "egress-full-fairness", target: target, frame: frame, sustain: 2,
				symptom: fmt.Sprintf(
					"Full-probe category %s has %d due providers in %s, including %d past its absolute deadline; minimum measured-rate deadline-prefix slack is %s seconds.",
					category.name, category.due, frame, category.expiredDue, slackText,
				),
				mechanism: "At least one due row has crossed its absolute deadline, or a cumulative deadline prefix has negative slack when every gross full-probe attempt is assigned to this category at the measured last-hour rate. The projection uses each actual deadline, not the whole category count against only its oldest row. A forecast shortfall is conditional on unchanged throughput; it does not prove an unavoidable future miss or scheduler starvation. Location and existing-health rows use their hard evidence expiries; missing health uses location observed_at plus the existing 24-hour health lifetime and does not claim that health evidence ever existed. The corrected scheduler merges bounded timestamp-indexed location and health heads plus an output-bounded anti-health head by absolute deadline before filling from unlocated work.",
				baseline: fmt.Sprintf(
					"No due %s row has crossed its %s category deadline, and when gross attempts are measurable every cumulative deadline prefix has nonnegative slack at that rate.",
					category.name, category.maxAge,
				),
				observed: fmt.Sprintf(
					"frame=%s category=%s due=%d expired_due=%d gross_full_attempted_last_hour=%d oldest_deadline_anchor_age=%s remaining_deadline_window=%s optimistic_all_capacity_drain=%s minimum_deadline_prefix_slack_seconds=%s deadline_missed=%t deadline_at_risk=%t all_shards_unlocated_can_fill_batch=%t configured_full_limit_per_shard=%d",
					frame, category.name, category.due, category.expiredDue, snapshot.fullAttemptsLastHour,
					(time.Duration(category.oldestAgeSeconds) * time.Second).Round(time.Second),
					(time.Duration(remainingSeconds) * time.Second).Round(time.Second), projectedText, slackText,
					deadlineMissed, deadlineAtRisk, allShardsUnlocatedSaturated, geometry.fullLimit,
				),
				evidence: "The query separates no-location from present-location work and assigns each urgent row to its earliest-deadline lane, with exact location/health ties stable in favor of location. Within each shard/category it groups equal deadlines, accumulates due counts through each deadline, and returns only the minimum slack plus due, expired, oldest-age, and success-inclusive gross-attempt aggregates. Whole-category drain and oldest remaining time are context, not the at-risk predicate. No provider or task identifier leaves PostgreSQL.",
				context:  "The all-shards-unlocated shape is diagnostic only: it proves starvation when joined to a running API artifact with fixed pass precedence, but it is not itself a post-EDF failure. Nonnegative per-category slack does not certify the simultaneous cross-category schedule or future successful outcomes: each category is given all gross capacity for this necessary-condition screen. First attempts and retries still share the unlocated lane, whose only defined contract is a six-hour minimum retry backoff; maximum retry delay or an allocation remains an explicit product decision.",
				action:   "Establish the running API scheduler behavior. If it retains fixed pass precedence and every unlocated head can fill the batch, apply the ordered health index migration and deploy the bounded EDF API correction; deploy the selective attempt cleanup with Taskworker. If EDF is already running, diagnose full capacity and category outcomes without inventing weights or deleting evidence.",
				verify:   "After API and Taskworker convergence, require expired_due=0 and nonnegative measured-rate minimum deadline-prefix slack for each urgent category for two samples; an unavailable zero-rate projection is not a healthy forecast. Preserve stale-health evidence through its 12-hour due-to-expiry window, keep missing-health at zero or advancing after attempt backoff, and complete a seven-day full sweep. Separately retain the open product decision and capacity gate for first attempts versus retries.",
				playbook: "SIGNALS.md §2.19, §2.23, and §2.24",
			})
		}
	}
	return findings
}

func egressCoverageStallFinding(target, frame, kind string, due, age, eligible, current, stallSeconds, deferredCurrentDarkDue int64) finding {
	class := "egress-" + kind + "-stalled"
	evidenceName := kind + " probe"
	mechanism := "The durable shard exists, but due providers are not reaching a persisted probe outcome. Hash-local evidence prevents activity in healthy sibling shards from hiding a stalled slice of the fleet."
	observedSuffix := ""
	evidence := "Counts and ages are aggregated inside the shard's normalized PostgreSQL hash partition; no provider or task identifier leaves the database."
	context := "This is a software execution or operational rollout failure. It does not establish a Proxy memory/hardware ceiling, and raising provider capacity cannot make a non-advancing task persist evidence."
	if kind == "blackhole" {
		evidenceName = "blackhole check"
	} else {
		evidenceName = "full probe from a provider without a current dark verdict"
		mechanism = "The durable shard exists, but due providers without a current dark verdict are not reaching a persisted probe outcome. Its newest-activity clock excludes current-dark rows, so old-deployment attempts against those deferred candidates cannot mask a stalled corrected queue in the same shard. Hash-local evidence still prevents activity in healthy sibling shards from hiding the failure."
		observedSuffix = fmt.Sprintf(" deferred_current_dark_due=%d", deferredCurrentDarkDue)
		evidence = "Counts and ages are aggregated inside the shard's normalized PostgreSQL hash partition. Current-dark candidates and their full activity are excluded from the due and newest-activity clock and retained only as one deferred aggregate; no provider or task identifier leaves the database."
		context = "This is a software execution or operational rollout failure. deferred_current_dark_due is desired scheduler-state accounting and proves deployed suppression only after the API artifact converges; before then it is a candidate suppression opportunity. It does not establish a Proxy memory/hardware ceiling, and raising provider capacity cannot make a non-advancing task persist evidence."
	}
	ageText := "never"
	if age >= 0 {
		ageText = (time.Duration(age) * time.Second).Round(time.Second).String()
	}
	return finding{
		probeId: "pg/egress-coverage", tier: tierPage,
		class: class, target: target, frame: frame, sustain: 2,
		symptom:   fmt.Sprintf("Provider-egress %s has %d due candidates in %s but its newest aggregate evidence is %s old.", kind, due, frame, ageText),
		mechanism: mechanism,
		baseline:  fmt.Sprintf("When a shard has due work, its newest %s evidence is no older than max_time + idle_delay + one monitor cadence (%s).", evidenceName, (time.Duration(stallSeconds) * time.Second).String()),
		observed:  fmt.Sprintf("frame=%s eligible=%d due=%d current=%d newest_evidence_age=%s derived_stall_bound=%s%s", frame, eligible, due, current, ageText, (time.Duration(stallSeconds) * time.Second).String(), observedSuffix),
		evidence:  evidence,
		context:   context,
		action:    "Correlate the shard frame with ProviderEgressProbe task errors and bounded Taskworker logs. Repair authentication, API reachability, task claim, or probe execution as the evidence identifies; converge the intended Taskworker generation. Do not delete provider evidence or manually rewrite the recurring task.",
		verify:    "The affected shard's newest evidence advances inside the derived bound for two cadences, or its due count drains to zero, while generic task canaries remain healthy.",
		playbook:  "SIGNALS.md §2.19, §1.2, and §8.9",
	}
}
