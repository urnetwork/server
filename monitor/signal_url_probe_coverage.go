package monitor

// SIGNALS.md §2.19f measures the URL-only rolling quota, separately from the
// legacy full/blackhole counters. A fleet census is global, not per worker;
// source identity and shard-owner coverage must be proved before aggregation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

const urlProbeCoverageFreshness = 3 * time.Minute
const urlProbeCoverageResponseMax = 2 * 1024 * 1024
const urlProbeCoverageCapability = 2

var urlProbeFleetStates = [...]string{
	"eligible", "due", "complete", "quota_complete", "secure_complete", "overdue",
	"runs_needed", "successes_needed", "security_pending", "security_unknown_targets",
	"warming", "uninitialized",
}

func NewUrlProbeCoverageSignal() Signal {
	return &signalAdapter{
		number: "2.19f", key: "url-probe-coverage", name: "Rolling provider URL-probe coverage",
		probe: urlProbeCoverageProbe{loadDesired: loadUrlProbeCoverageDesired},
	}
}

type urlProbeCoverageProbe struct {
	loadDesired func() (urlProbeCoverageDesired, error)
}

func (urlProbeCoverageProbe) id() string             { return "runtime/url-probe-coverage" }
func (urlProbeCoverageProbe) tier() string           { return tierWarn }
func (urlProbeCoverageProbe) cadence() time.Duration { return 5 * time.Minute }

type urlProbeCoverageDesired struct {
	enabled    bool
	shardCount int
}

// Desired shards come from configuration, never from whichever series survived.
// An explicit URL workflow avoids silently certifying a legacy producer.
func loadUrlProbeCoverageDesired() (urlProbeCoverageDesired, error) {
	resource, err := server.Config.SimpleResource("provider_egress_probe.yml")
	if err != nil {
		return urlProbeCoverageDesired{}, errors.New("URL scheduler configuration unavailable")
	}
	return parseUrlProbeCoverageDesired(resource.UnmarshalYamlE)
}

func parseUrlProbeCoverageDesired(unmarshal func(any) error) (urlProbeCoverageDesired, error) {
	var raw struct {
		Enabled               *bool                           `yaml:"enabled"`
		ShardCount            int                             `yaml:"shard_count"`
		UrlProbeResultVersion int                             `yaml:"url_probe_result_version"`
		UrlProbe              *egressCoverageDesiredBatchYAML `yaml:"url_probe"`
	}
	if err := unmarshal(&raw); err != nil || raw.Enabled == nil {
		return urlProbeCoverageDesired{}, errors.New("URL scheduler enabled state unavailable")
	}
	if !*raw.Enabled {
		return urlProbeCoverageDesired{}, nil
	}
	if raw.ShardCount < 1 || raw.ShardCount > 256 || raw.UrlProbe == nil ||
		raw.UrlProbeResultVersion != egresshealth.UrlProbePolicyVersion {
		return urlProbeCoverageDesired{}, errors.New("explicit URL scheduler geometry unavailable")
	}
	batch := raw.UrlProbe.args()
	if batch.Limit < 1 || batch.Concurrency < 1 || batch.Concurrency > batch.Limit || batch.ProbeTimeoutSeconds < 1 ||
		batch.AllDestinations || batch.Bandwidth || batch.IpEchoTimeoutSeconds != 0 || len(raw.UrlProbe.Unknown) != 0 {
		return urlProbeCoverageDesired{}, errors.New("URL scheduler worker geometry invalid")
	}
	return urlProbeCoverageDesired{enabled: true, shardCount: raw.ShardCount}, nil
}

// Keep exact process identity in memory only. Alerts expose aggregate counts.
type urlProbeCoverageProcess struct {
	host, block, instance string
	values                map[string]float64
	invalid               bool
	cohortInvalid         bool
}

func urlProbeCoverageQuery(environment string) string {
	base := fmt.Sprintf(`env=%s,job="taskworker"`, strconv.Quote(environment))
	parts := []string{}
	add := func(name, expression string) {
		parts = append(parts, fmt.Sprintf(`label_replace((%s),"monitor_metric",%s,"job",".*")`, expression, strconv.Quote(name)))
	}
	for _, metric := range []struct{ name, selector string }{
		{"start", "process_start_time_seconds{" + base + "}"},
		{"configured", "urnetwork_url_probe_configured_shards{" + base + "}"},
		{"capability", "urnetwork_url_probe_capability{" + base + "}"},
		{"heartbeat", "urnetwork_url_probe_shard_observed_timestamp_seconds{" + base + "}"},
		{"fleet", "urnetwork_url_probe_fleet{" + base + "}"},
		{"observed", "urnetwork_url_probe_fleet_observed_timestamp_seconds{" + base + "}"},
		{"oldest", "urnetwork_url_probe_oldest_due_seconds{" + base + "}"},
		{"cohort_started", "urnetwork_url_probe_cohort_started_timestamp_seconds{" + base + "}"},
		{"cohort", "urnetwork_url_probe_admission_cohort{" + base + "}"},
		{"cohort_contract", "urnetwork_url_probe_admission_cohort_contract{" + base + "}"},
	} {
		add(metric.name, metric.selector)
		add(metric.name+"_time", "timestamp("+metric.selector+")")
	}
	for _, outcome := range []string{"success", "error"} {
		counter := "urnetwork_url_probe_outcomes_total{" + base + ",outcome=" + strconv.Quote(outcome) + "}"
		add(outcome, "increase("+counter+"[1h])")
		add(outcome+"_time", "timestamp("+counter+")")
		add(outcome+"_samples", "count_over_time("+counter+"[1h])")
		add(outcome+"_early", "count_over_time("+counter+"[5m] offset 55m)")
	}
	return strings.Join(parts, " or ")
}

func (self urlProbeCoverageProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := env.now()
	observation := newUrlProbeCoverageObservation(env.cfg.env, now)
	env.urlProbeCoverageObservation = &observation
	desired, err := self.loadDesired()
	if err != nil {
		observation.CensusReason = "desired_configuration_unavailable"
		return []finding{urlProbeCoverageUnknown("desired URL scheduler configuration unavailable or legacy")}, nil
	}
	if !desired.enabled {
		observation.CensusReason = "workflow_disabled"
		return nil, nil
	}
	expected := map[string]bool{}
	for _, host := range env.cfg.logServiceHosts["taskworker"] {
		for _, block := range env.cfg.logServiceBlocks["taskworker"] {
			expected[host+"\x00"+block] = true
		}
	}
	gateways := env.cfg.hostsWithRole("services")
	if len(expected) == 0 || len(gateways) == 0 {
		observation.CensusReason = "inventory_unavailable"
		return []finding{urlProbeCoverageUnknown("taskworker inventory or services gateway unavailable")}, nil
	}
	command := "curl -fsS --max-time 15 --max-filesize 2097152 --data-urlencode " +
		shellSingleQuote("query="+urlProbeCoverageQuery(env.cfg.env)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	out, _, err := shellFirstServiceGateway(ctx, env.runner, gateways, nil, command)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		observation.CensusReason = "transport_unavailable"
		return []finding{urlProbeCoverageUnknown("bounded Mimir observation unavailable")}, nil
	}
	now = env.now()
	observation.ObservedAt = now
	processes, err := parseUrlProbeCoverage(out, env.cfg.env, now)
	if err != nil {
		observation.CensusReason = "response_invalid"
		return []finding{urlProbeCoverageUnknown("malformed, partial, or ambiguous Mimir response")}, nil
	}
	return evaluateUrlProbeCoverageObservation(processes, expected, desired.shardCount, now, &observation), nil
}

// Recognize only fixed metric families, finite states and bounded shard labels.
// Duplicate cells, warnings and nonfinite values cannot become healthy zeros.
func parseUrlProbeCoverage(payload, environment string, now time.Time) ([]*urlProbeCoverageProcess, error) {
	if len(payload) > urlProbeCoverageResponseMax {
		return nil, errors.New("URL coverage response exceeds limit")
	}
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(payload), &response); err != nil || response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 {
		return nil, errors.New("invalid URL coverage response")
	}
	byProcess := map[string]*urlProbeCoverageProcess{}
	for _, series := range response.Data.Result {
		labels := series.Metric
		if labels["env"] != environment || labels["job"] != "taskworker" || labels["host"] == "" || labels["block"] == "" || labels["instance"] == "" {
			return nil, errors.New("invalid URL coverage identity")
		}
		key := labels["host"] + "\x00" + labels["block"] + "\x00" + labels["instance"]
		process := byProcess[key]
		if process == nil {
			process = &urlProbeCoverageProcess{host: labels["host"], block: labels["block"], instance: labels["instance"], values: map[string]float64{}}
			byProcess[key] = process
		}
		name := labels["monitor_metric"]
		cohortMetric := name == "cohort" || name == "cohort_time" || name == "cohort_contract" || name == "cohort_contract_time"
		invalidate := func() {
			if cohortMetric {
				process.cohortInvalid = true
			} else {
				process.invalid = true
			}
		}
		switch name {
		case "start", "start_time", "configured", "configured_time", "capability", "capability_time", "observed", "observed_time", "oldest", "oldest_time", "cohort_started", "cohort_started_time", "success", "success_time", "success_samples", "success_early", "error", "error_time", "error_samples", "error_early":
		case "cohort_contract", "cohort_contract_time":
		case "cohort", "cohort_time":
			if !urlProbeAdmissionCohortLabelValid(labels["cohort"], labels["state"]) {
				invalidate()
			}
			name += ":" + labels["cohort"] + ":" + labels["state"]
		case "fleet", "fleet_time":
			valid := false
			for _, state := range urlProbeFleetStates {
				valid = valid || labels["state"] == state
			}
			if !valid {
				process.invalid = true
			}
			name += ":" + labels["state"]
		case "heartbeat", "heartbeat_time":
			shard, err := strconv.Atoi(labels["shard"])
			if err != nil || shard < 0 || shard >= 256 || strconv.Itoa(shard) != labels["shard"] {
				process.invalid = true
			}
			name += ":" + labels["shard"]
		default:
			process.invalid = true
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || now.Sub(observedAt) > urlProbeCoverageFreshness || observedAt.Sub(now) > 30*time.Second {
			invalidate()
			continue
		}
		if _, exists := process.values[name]; exists {
			invalidate()
		}
		process.values[name] = value
	}
	processes := []*urlProbeCoverageProcess{}
	for _, process := range byProcess {
		processes = append(processes, process)
	}
	return processes, nil
}

func urlProbeCoverageFresh(value float64, now time.Time) bool {
	return value > 0 && float64(now.Unix())-value <= urlProbeCoverageFreshness.Seconds() && value-float64(now.Unix()) <= 30
}

// One host/block replacement cannot borrow old counters or census cells.
func currentUrlProbeProcesses(processes []*urlProbeCoverageProcess, expected map[string]bool, now time.Time) map[string]*urlProbeCoverageProcess {
	current := map[string]*urlProbeCoverageProcess{}
	ambiguous := map[string]bool{}
	unknownStart := map[string]bool{}
	for _, process := range processes {
		slot := process.host + "\x00" + process.block
		if !expected[slot] {
			continue
		}
		// Range-only results prove historical activity, not a live owner.
		// Any instant field or malformed process still fails closed below.
		rangeOnly := !process.invalid && len(process.values) > 0
		for name := range process.values {
			switch name {
			case "success", "success_samples", "success_early", "error", "error_samples", "error_early":
			default:
				rangeOnly = false
			}
		}
		if rangeOnly {
			continue
		}
		start := process.values["start"]
		if start <= 0 || !urlProbeCoverageFresh(process.values["start_time"], now) || start > float64(now.Unix()+30) {
			unknownStart[slot] = true
			continue
		}
		old := current[slot]
		if old == nil || old.values["start"] < start {
			current[slot] = process
			ambiguous[slot] = false
		} else if start == old.values["start"] && process.instance != old.instance {
			ambiguous[slot] = true
		}
	}
	for slot, process := range current {
		if ambiguous[slot] || unknownStart[slot] || process.invalid {
			delete(current, slot)
		}
	}
	return current
}

func urlProbeCoverageCensusValid(process *urlProbeCoverageProcess, now time.Time) bool {
	return diagnoseUrlProbeCoverageCensus(process, now).reason == urlProbeCensusOK
}

// A complete global census can prove a deficit even if unrelated worker
// metrics are absent. Hourly capacity needs all current sources and ranges;
// missing sources must not become a misleading low-rate capacity verdict.
func evaluateUrlProbeCoverage(processes []*urlProbeCoverageProcess, expected map[string]bool, shardCount int, now time.Time) []finding {
	observation := newUrlProbeCoverageObservation("", now)
	return evaluateUrlProbeCoverageObservation(processes, expected, shardCount, now, &observation)
}

// The alert reducer and healthy observer use the same selected owner and cells.
func evaluateUrlProbeCoverageObservation(processes []*urlProbeCoverageProcess, expected map[string]bool, shardCount int, now time.Time, observation *UrlProbeCoverageObservation) []finding {
	current := currentUrlProbeProcesses(processes, expected, now)
	owners := make([][]*urlProbeCoverageProcess, shardCount)
	unexpectedShard := false
	for _, process := range current {
		values := process.values
		if values["capability"] != urlProbeCoverageCapability || !urlProbeCoverageFresh(values["capability_time"], now) || values["configured"] != float64(shardCount) || !urlProbeCoverageFresh(values["configured_time"], now) {
			continue
		}
		for name, value := range values {
			if strings.HasPrefix(name, "heartbeat:") && urlProbeCoverageFresh(value, now) {
				shard, err := strconv.Atoi(strings.TrimPrefix(name, "heartbeat:"))
				unexpectedShard = unexpectedShard || err != nil || shard >= shardCount
			}
		}
		for shard := range owners {
			key := strconv.Itoa(shard)
			if urlProbeCoverageFresh(values["heartbeat:"+key], now) && urlProbeCoverageFresh(values["heartbeat_time:"+key], now) && values["heartbeat:"+key] >= values["start"] {
				owners[shard] = append(owners[shard], process)
			}
		}
	}
	findings := []finding{}
	gaps := []string{}
	defer func() {
		sort.Strings(gaps)
		observation.Gaps = append([]string{}, gaps...)
		observation.SourceCoverageComplete = observation.CensusReason == "ok" && len(gaps) == 0
	}()
	if unexpectedShard {
		gaps = append(gaps, "unexpected_active_shard")
	}
	for shard, candidates := range owners {
		if len(candidates) != 1 {
			gaps = append(gaps, fmt.Sprintf("shard_%d_owners=%d", shard, len(candidates)))
		}
	}
	if len(owners) == 0 || len(owners[0]) != 1 {
		observation.CensusReason = "owner_unavailable"
		gaps = append(gaps, "fresh_coherent_global_census_unavailable")
		gaps = append(gaps, "source=census-owner census_reason=owner_unavailable")
		return []finding{urlProbeCoverageUnknown(strings.Join(gaps, " "))}
	}
	census := diagnoseUrlProbeCoverageCensus(owners[0][0], now)
	observation.CensusReason = urlProbeCensusReasonLabels[census.reason]
	if census.reason != urlProbeCensusOK {
		gaps = append(gaps, "fresh_coherent_global_census_unavailable", census.projection())
		return []finding{urlProbeCoverageUnknown(strings.Join(gaps, " "))}
	}
	values := owners[0][0].values
	eligible := values["fleet:eligible"]
	complete := values["fleet:secure_complete"]
	observed := fmt.Sprintf("eligible=%.0f quota_complete=%.0f secure_complete=%.0f due=%.0f overdue=%.0f warming=%.0f uninitialized=%.0f runs_needed=%.0f security_pending=%.0f security_unknown_targets=%.0f oldest_due_seconds=%.1f",
		eligible, values["fleet:quota_complete"], complete, values["fleet:due"], values["fleet:overdue"], values["fleet:warming"], values["fleet:uninitialized"], values["fleet:runs_needed"], values["fleet:security_pending"], values["fleet:security_unknown_targets"], values["oldest"])
	observed += " " + census.projection()
	cohort := diagnoseUrlProbeAdmissionCohort(owners[0][0])
	observation.recordCensus(owners[0][0], cohort)
	observed += " " + cohort.projection(values)
	if !cohort.valid {
		gaps = append(gaps, "mature_cohort_unobservable")
	} else if values["cohort:age_unknown:eligible"] > 0 {
		gaps = append(gaps, "admission_age_unknown_blocks_whole_fleet_verdict")
	}
	if cohort.valid && values["cohort:mature:eligible"] > values["cohort:mature:quota_complete"] {
		tier, sustain := tierWarn, 1
		mature := values["cohort:mature:eligible"]
		if mature-values["cohort:mature:quota_complete"] >= 0.10*mature {
			tier, sustain = tierPage, 2
		}
		findings = append(findings, urlProbeCoverageFinding("url-probe-coverage-deficit", tier, sustain, observed,
			"Providers first admitted at least four hours ago lack ten accepted measured URL runs, success or failure, in the rolling four-hour window.",
			"Every known mature provider should meet the rolling ten-run quota. WARN for any mature deficit; PAGE when at least 10% of the mature cohort is deficient, sustained twice. Known newcomers remain visible as warming; unknown first-admission age cannot establish whole-fleet coverage."))
	}
	if values["fleet:security_pending"] > 0 {
		findings = append(findings, urlProbeCoverageFinding("url-probe-security-pending", tierWarn, 1, observed,
			"Currently eligible providers have unresolved TLS exceptions, independently of measured-run quota or first-admission age.",
			"No unresolved TLS exceptions; ten measured outcomes and a mature-quota percentage cannot clear security quarantine."))
	}
	if values["fleet:security_unknown_targets"] > 0 {
		findings = append(findings, urlProbeCoverageFinding("url-probe-security-recovery-unknown", tierWarn, 1, observed,
			"Legacy TLS quarantine lacks a trustworthy destination for a same-URL recovery check.",
			"No unresolved legacy security exception with unknown URL identity; unrelated successes or elapsed time cannot clear one."))
	}
	runsPerHour := 0.0
	rateComplete := len(current) == len(expected)
	for _, process := range current {
		values := process.values
		if values["capability"] != urlProbeCoverageCapability || !urlProbeCoverageFresh(values["capability_time"], now) || float64(now.Unix())-values["start"] < time.Hour.Seconds() {
			rateComplete = false
			continue
		}
		for _, outcome := range []string{"success", "error"} {
			runs, present := values[outcome]
			if !present || !urlProbeCoverageFresh(values[outcome+"_time"], now) || values[outcome+"_samples"] < 30 || values[outcome+"_early"] < 1 {
				rateComplete = false
				continue
			}
			runsPerHour += runs
		}
	}
	if eligible > 0 && rateComplete && len(gaps) == 0 {
		required := 10 * eligible / 4
		if runsPerHour < required {
			projection := "unbounded"
			if runsPerHour > 0 {
				projection = fmt.Sprintf("%.2f", 10*eligible/runsPerHour)
			}
			findings = append(findings, urlProbeCoverageFinding("url-probe-throughput-deficit", tierWarn, 2,
				fmt.Sprintf("%s acknowledged_measured_runs_per_hour=%.1f required_measured_runs_per_hour=%.1f projected_quota_hours=%s", observed, runsPerHour, required, projection),
				"Acknowledged measured URL throughput, success plus error, is below the necessary rate for the rolling fleet target.",
				"At least 10 times the eligible provider count per four hours in accepted measured URL runs; aggregate acknowledgement rate is necessary but cannot prove durable unique coverage or per-provider fairness."))
		}
	} else if eligible > 0 && !rateComplete {
		gaps = append(gaps, "hourly_measured_run_ranges_or_expected_process_coverage_incomplete")
	}
	if len(gaps) > 0 {
		sort.Strings(gaps)
		findings = append(findings, urlProbeCoverageUnknown(strings.Join(gaps, " ")+" "+observed))
	}
	return findings
}

func urlProbeCoverageFinding(class, tier string, sustain int, observed, symptom, baseline string) finding {
	return finding{
		probeId: "runtime/url-probe-coverage", tier: tier, class: class,
		target: "provider-url-fleet", frame: "rolling-four-hours", sustain: sustain,
		symptom: symptom, baseline: baseline, observed: observed,
		mechanism: "The URL workflow serves the same reliability and ARIN-risk eligible cohort as FP2. Accepted measured success and failure, not attempted requests, setup-only turns or legacy full/blackhole counters, replenish its rolling quota; TLS recovery remains independent of content quality.",
		evidence:  "One atomic global census from the uniquely observed shard-zero owner; process identities, underlying scrape times, and durable observation time are checked independently. Global fleet gauges are never summed across processes.",
		context:   "Providers first admitted less than four hours ago have a warming interval; unknown first-admission age is separate. The known mature ratio is N/A for an empty cohort, and even 100% cannot establish whole-fleet coverage while any age is unknown. Ordinary URL failure does not independently exclude online fallback; quality's 4/5 success ratio and TLS security are separate. Hourly rate is a forecast and does not prove unique provider coverage. Missing/ambiguous sources remain unobservable. These thresholds are alerts, not hard service limits.",
		action:    "Compare fixed-slot due selection and oldest-work fairness, private tunnel/DNS/HTTP stages, accepted receipt rate, and PG CPU/query work. Check URL catalog compatibility and per-URL TLS recovery. Do not relabel local setup errors as provider failures or weaken the common reliability/risk/security gates.",
		verify:    "Require fresh unique owners for every configured shard, coherent complete censuses across two cadences, and rolling per-provider quota recovery. Confirm FP2 online backfill and normal PG CPU independently; a fast aggregate rate or ten historical runs is insufficient.",
		playbook:  "SIGNALS.md §2.19f",
	}
}

func urlProbeCoverageUnknown(observed string) finding {
	return urlProbeCoverageFinding("url-probe-coverage-unobservable", tierWarn, 1, observed,
		"The current URL-probe coverage or throughput cannot be fully observed.",
		"Explicit desired URL scheduler geometry, one fresh owner per shard, a coherent source-owned census, and complete current-process hourly counter ranges.")
}
