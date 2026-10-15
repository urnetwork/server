package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/model"
)

// SIGNALS.md §2.29 maps to signal_modern_gate_readiness.go and
// signal_modern_gate_readiness_test.go. The modern provider gate turns on by
// itself at its start (FP2FIX.md, owner decision 2026-10-15) while
// quality_speed_requires_modern_provider is absent from provider.yml, and
// takes every provider the capability probe has not shown to be modern out of
// Quality and Speed. Its evidence comes from a probe that may not have swept
// the fleet by then, so this signal reads the gate's shadow, what it would
// take out, from 72 hours before the start: WARN when it would take more than
// half of Quality or of Speed, PAGE from 24 hours before, and PAGE when a
// bucket falls by more than half after the start, until 18:00 UTC on the
// start's day while the kill switch can still be released. A fleet whose
// taskworkers do not all publish the gate, decide it differently, or
// schedule different starts WARNs before the earliest start and PAGEs from
// it. The start, and whether one is scheduled at all, comes from the
// taskworkers' own provider.yml as their stats collector publishes it, so
// postponing the gate or moving its start moves or silences the signal.
func NewModernGateReadinessSignal() Signal {
	return &signalAdapter{number: "2.29", key: "modern-gate-readiness", name: "Modern provider gate readiness", probe: modernGateReadinessProbe{}}
}

const (
	// how long before the start the shadow is read, and WARNs
	modernGateReadinessWarnLead = 72 * time.Hour
	// how long before the start a gutting shadow PAGEs
	modernGateReadinessPageLead = 24 * time.Hour
	// the least the watch after the start lasts; it runs on to
	// modernGateReadinessWatchUntilHour of the start's day
	modernGateReadinessFlipWatch = time.Hour
	// the hour, UTC, of the start's day until which a fallen bucket or a
	// disagreeing fleet PAGEs: the kill switch can be released until then
	// (on 2026-10-21 config changes freeze from 18:00Z)
	modernGateReadinessWatchUntilHour = 18
	// the share of a bucket the gate may take out, or a bucket may lose at
	// the flip, before the signal fires; exactly half does not
	modernGateReadinessMaxShare = 0.5
	// the oldest dashboard counts the shadow is judged on
	modernGateReadinessMaxCountsAge = 30 * time.Minute
	// the largest Mimir answer read
	modernGateReadinessMaxResponseBytes = 64 * 1024
	modernGateReadinessProbeId          = "mimir/modern-gate-readiness"
	modernGateReadinessTarget           = "modern-provider-gate"
)

// the native buckets the gate holds, in the order findings list them
var modernGateReadinessBuckets = []string{model.RankModeQuality, model.RankModeSpeed}

type modernGateReadinessProbe struct{}

func (modernGateReadinessProbe) id() string             { return modernGateReadinessProbeId }
func (modernGateReadinessProbe) tier() string           { return tierPage }
func (modernGateReadinessProbe) cadence() time.Duration { return 5 * time.Minute }

// What the signal reads from Mimir at one instant.
type modernGateReadinessObservation struct {
	// running taskworkers that publish the gate
	// (urnetwork_stats_provider_modern_gate_on), and whether it holds
	// Quality and Speed on every one of them (min) and on any (max)
	publishing int
	onEvery    bool
	onAny      bool
	// running taskworkers by their process series, zero when Mimir has none
	taskworkers int
	// the latest start the taskworkers schedule, nil while provider.yml sets
	// the switch on every one and no flip is scheduled
	start *time.Time
	// the earliest start they schedule, and the taskworkers that schedule one
	earliestStart *time.Time
	scheduling    int
	// when the dashboard counts' source read started, zero when unknown
	countsStarted time.Time
	// bucket, then the gate's outcome, to the providers the other rules admit
	shadow map[string]map[string]float64
	// bucket to its providers now
	buckets map[string]float64
	// bucket to its providers in the last sample before the start, read only
	// during the watch after it
	bucketsBefore map[string]float64
}

// One instant query's parts, each a fixed label value, so a single round
// trip reads the fleet's agreement on the gate, the schedule, the shadow,
// the buckets and the counts' age. The gate and its start are read per
// running taskworker, against the running taskworker processes: count, and
// min against max, so a taskworker without them, or on an older config, is
// seen.
func modernGateReadinessQuery(environment string) string {
	selector := "env=" + strconv.Quote(environment)
	taskworkerSelector := selector + `,job="taskworker"`
	part := func(expression string, name string) string {
		return "label_replace(" + expression + `,"monitor_modern_gate_part",` + strconv.Quote(name) + `,"","")`
	}
	return strings.Join([]string{
		part("count(urnetwork_stats_provider_modern_gate_on{"+taskworkerSelector+"})", "on_count"),
		part("min(urnetwork_stats_provider_modern_gate_on{"+taskworkerSelector+"})", "on_min"),
		part("max(urnetwork_stats_provider_modern_gate_on{"+taskworkerSelector+"})", "on_max"),
		part("count(process_start_time_seconds{"+taskworkerSelector+"})", "taskworkers"),
		part("count(urnetwork_stats_provider_modern_gate_start_seconds{"+taskworkerSelector+"})", "start_count"),
		part("min(urnetwork_stats_provider_modern_gate_start_seconds{"+taskworkerSelector+"})", "start_min"),
		part("max(urnetwork_stats_provider_modern_gate_start_seconds{"+taskworkerSelector+"})", "start"),
		part("max(urnetwork_stats_provider_egress_source_started_seconds{"+selector+"})", "counts_started"),
		part("max by (bucket, outcome) (urnetwork_stats_provider_modern_gate_shadow{"+selector+`,bucket=~"quality|speed"})`, "shadow"),
		part(modernGateReadinessBucketExpression(environment), "bucket"),
	}, " or ")
}

// Each native bucket's providers as the providers dashboard sums them: every
// taskworker publishes the same shared snapshot, so the largest of each
// index, summed.
func modernGateReadinessBucketExpression(environment string) string {
	return `sum by (bucket) (max by (bucket, index) (urnetwork_stats_provider_egress_index{env=` + strconv.Quote(environment) + `,bucket=~"quality|speed"}))`
}

func (modernGateReadinessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	now := env.now()
	metricHosts := env.cfg.hostsWithRole("services")
	query := func(expression string, at time.Time) (map[string][]modernGateReadinessSeries, error) {
		if len(metricHosts) == 0 {
			return nil, fmt.Errorf("modern gate readiness: no services host is configured for the loopback Mimir query")
		}
		command := "curl -fsS --max-time 15 --max-filesize " + strconv.Itoa(modernGateReadinessMaxResponseBytes) +
			" --data-urlencode " + shellSingleQuote("query="+expression) +
			" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(at.Unix(), 10)) +
			" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
		out, _, err := shellFirstServiceGateway(ctx, env.runner, metricHosts, nil, command)
		if err != nil {
			return nil, fmt.Errorf("modern gate readiness: query Mimir through service gateways: %w", err)
		}
		return parseModernGateReadinessResponse(out, at)
	}

	observation, err := func() (modernGateReadinessObservation, error) {
		parts, err := query(modernGateReadinessQuery(env.cfg.env), now)
		if err != nil {
			return modernGateReadinessObservation{}, err
		}
		return newModernGateReadinessObservation(parts)
	}()
	if err != nil {
		// without the schedule only the code's default start places the
		// windows; outside them a failed read stays quiet
		if modernGateReadinessInWindow(model.DefaultQualitySpeedModernProviderStart(), now) {
			return nil, err
		}
		return nil, nil
	}
	if observation.start != nil && !now.Before(*observation.start) && now.Before(modernGateReadinessWatchEnd(*observation.start)) {
		// the buckets in the last sample before the start
		beforeParts, err := query(`label_replace(`+modernGateReadinessBucketExpression(env.cfg.env)+`,"monitor_modern_gate_part","bucket","","")`, observation.start.Add(-time.Second))
		if err != nil {
			return nil, err
		}
		before, err := newModernGateReadinessObservation(beforeParts)
		if err != nil {
			return nil, err
		}
		observation.bucketsBefore = before.buckets
	}
	return evaluateModernGateReadiness(observation, now)
}

// Whether now falls in the windows the signal judges around start: from the
// warning lead before it to the end of the watch after it.
func modernGateReadinessInWindow(start time.Time, now time.Time) bool {
	return !now.Before(start.Add(-modernGateReadinessWarnLead)) && now.Before(modernGateReadinessWatchEnd(start))
}

// The end of the watch after start: modernGateReadinessWatchUntilHour UTC on
// the start's day, or modernGateReadinessFlipWatch after the start when that
// is later.
func modernGateReadinessWatchEnd(start time.Time) time.Time {
	day := start.UTC()
	end := time.Date(day.Year(), day.Month(), day.Day(), modernGateReadinessWatchUntilHour, 0, 0, 0, time.UTC)
	if least := start.Add(modernGateReadinessFlipWatch); end.Before(least) {
		return least
	}
	return end
}

// One series of a response: its labels and value.
type modernGateReadinessSeries struct {
	labels map[string]string
	value  float64
}

// The response's series by part. Fixed error text only: response labels and
// bodies never become findings.
func parseModernGateReadinessResponse(raw string, at time.Time) (map[string][]modernGateReadinessSeries, error) {
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
	}
	if modernGateReadinessMaxResponseBytes < len(raw) || json.Unmarshal([]byte(raw), &response) != nil {
		return nil, fmt.Errorf("modern gate readiness: undecodable Mimir response")
	}
	if response.Status != "success" || response.Data.ResultType != "vector" || 0 < len(response.Warnings) {
		return nil, fmt.Errorf("modern gate readiness: Mimir status=%q result_type=%q warnings=%d", response.Status, response.Data.ResultType, len(response.Warnings))
	}
	parts := map[string][]modernGateReadinessSeries{}
	for _, series := range response.Data.Result {
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || observedAt.Unix() != at.Unix() || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("modern gate readiness: invalid sample")
		}
		part := series.Metric["monitor_modern_gate_part"]
		parts[part] = append(parts[part], modernGateReadinessSeries{labels: series.Metric, value: value})
	}
	return parts, nil
}

// The observation the parts give. A part present more than once, or a label
// outside the gate's buckets and outcomes, is an invalid response; a part
// absent is left unknown, judged only where it is needed.
func newModernGateReadinessObservation(parts map[string][]modernGateReadinessSeries) (modernGateReadinessObservation, error) {
	observation := modernGateReadinessObservation{
		shadow:  map[string]map[string]float64{},
		buckets: map[string]float64{},
	}
	invalid := func(part string) (modernGateReadinessObservation, error) {
		return modernGateReadinessObservation{}, fmt.Errorf("modern gate readiness: invalid %s series", part)
	}
	for part, series := range parts {
		switch part {
		case "on_count", "on_min", "on_max", "taskworkers", "start_count", "start_min", "start", "counts_started":
			if len(series) != 1 {
				return invalid(part)
			}
			value := series[0].value
			switch part {
			case "on_count":
				observation.publishing = int(value)
			case "on_min":
				observation.onEvery = 1 <= value
			case "on_max":
				observation.onAny = 1 <= value
			case "taskworkers":
				observation.taskworkers = int(value)
			case "start_count":
				observation.scheduling = int(value)
			case "start_min":
				earliestStart := time.Unix(int64(value), 0).UTC()
				observation.earliestStart = &earliestStart
			case "start":
				start := time.Unix(int64(value), 0).UTC()
				observation.start = &start
			case "counts_started":
				observation.countsStarted = unixFloatTime(value)
			}
		case "shadow":
			for _, one := range series {
				bucket, outcome := one.labels["bucket"], one.labels["outcome"]
				if !slices.Contains(modernGateReadinessBuckets, bucket) || !slices.Contains(model.ProviderModernOutcomes, outcome) {
					return invalid(part)
				}
				outcomes := observation.shadow[bucket]
				if outcomes == nil {
					outcomes = map[string]float64{}
					observation.shadow[bucket] = outcomes
				}
				if _, duplicate := outcomes[outcome]; duplicate {
					return invalid(part)
				}
				outcomes[outcome] = one.value
			}
		case "bucket":
			for _, one := range series {
				bucket := one.labels["bucket"]
				if _, duplicate := observation.buckets[bucket]; duplicate || !slices.Contains(modernGateReadinessBuckets, bucket) {
					return invalid(part)
				}
				observation.buckets[bucket] = one.value
			}
		default:
			return invalid("unknown")
		}
	}
	return observation, nil
}

// The signal's verdict at now, a pure function of the observation. Before
// the warning lead, after the watch, and while provider.yml schedules no
// flip, it is healthy whatever the gauges say. An error is a visibility
// failure: the fleet, the shadow, the counts' age, or the buckets before the
// start could not be read where the verdict needs them. Where some of the
// verdict could be read, its findings stand beside a cannot-observe finding
// for the rest.
func evaluateModernGateReadiness(observation modernGateReadinessObservation, now time.Time) ([]finding, error) {
	healthy := []finding{healthyFinding(modernGateReadinessProbeId, tierPage, "modern-gate-not-ready", modernGateReadinessTarget)}
	if observation.publishing == 0 {
		// the taskworkers do not run the code that flips the gate, or their
		// stats collector is down: the code's default start places the windows
		start := model.DefaultQualitySpeedModernProviderStart()
		if modernGateReadinessInWindow(start, now) {
			return nil, fmt.Errorf("modern gate readiness: no taskworker publishes urnetwork_stats_provider_modern_gate_on, so the automatic flip at %s is unconfirmed", start.Format(time.RFC3339))
		}
		return healthy, nil
	}
	if observation.start == nil {
		// provider.yml sets the switch on every taskworker: no flip is scheduled
		return healthy, nil
	}
	// the latest start places the shadow's windows and the watch after it;
	// the earliest, on a taskworker that missed a moved start, opens the
	// fleet's window sooner
	start := *observation.start
	earliest := start
	if observation.earliestStart != nil && observation.earliestStart.Before(start) {
		earliest = *observation.earliestStart
	}
	if now.Before(earliest.Add(-modernGateReadinessWarnLead)) || !now.Before(modernGateReadinessWatchEnd(start)) {
		return healthy, nil
	}
	if observation.taskworkers == 0 {
		return nil, fmt.Errorf("modern gate readiness: Mimir holds no running taskworker process series, so the fleet's agreement on the gate cannot be judged")
	}

	findings := []finding{}
	// every running taskworker publishes the gate and decides it alike, and
	// while a start is ahead every one schedules the same start
	disagrees := observation.publishing < observation.taskworkers || observation.onEvery != observation.onAny
	if now.Before(start) && (observation.scheduling < observation.publishing || earliest.Before(start)) {
		disagrees = true
	}
	if disagrees {
		tier := tierWarn
		if !now.Before(earliest) {
			tier = tierPage
		}
		findings = append(findings, modernGateFleetDisagreesFinding(observation, earliest, start, now, tier))
	}
	// what the verdict below cannot read
	var unobserved error
	switch {
	case now.Before(start.Add(-modernGateReadinessWarnLead)):
		// only an earlier start opens the window this early
	case now.Before(start):
		if observation.countsStarted.IsZero() || modernGateReadinessMaxCountsAge < now.Sub(observation.countsStarted) {
			unobserved = fmt.Errorf("modern gate readiness: the dashboard counts are older than %s (urnetwork_stats_provider_egress_source_started_seconds), so the shadow cannot be judged", modernGateReadinessMaxCountsAge)
			break
		}
		tier := tierWarn
		if !now.Before(start.Add(-modernGateReadinessPageLead)) {
			tier = tierPage
		}
		for _, bucket := range modernGateReadinessBuckets {
			outcomes := observation.shadow[bucket]
			if len(outcomes) != len(model.ProviderModernOutcomes) {
				unobserved = errors.Join(unobserved, fmt.Errorf("modern gate readiness: the %s shadow does not carry every outcome", bucket))
				continue
			}
			admitted := 0.0
			for _, count := range outcomes {
				admitted += count
			}
			removed := admitted - outcomes[model.ProviderModernOutcomeModern]
			if 0 < admitted && modernGateReadinessMaxShare*admitted < removed {
				findings = append(findings, modernGateNotReadyFinding(bucket, outcomes, admitted, removed, start, now, tier))
			}
		}
	case observation.onEvery && !observation.countsStarted.Before(start):
		// during the watch after the start: only counts read after it, by a
		// fleet that has flipped, show the flip, so earlier ones are no
		// verdict yet. A gate off everywhere, the kill switch, ends the watch,
		// and one on in only some taskworkers is the disagreement above
		for _, bucket := range modernGateReadinessBuckets {
			before, beforeOk := observation.bucketsBefore[bucket]
			after, afterOk := observation.buckets[bucket]
			if !beforeOk || !afterOk {
				unobserved = errors.Join(unobserved, fmt.Errorf("modern gate readiness: no %s bucket sample before the start or now", bucket))
				continue
			}
			if 0 < before && (before-after) > modernGateReadinessMaxShare*before {
				findings = append(findings, modernGateFlipDropFinding(bucket, before, after, observation.shadow[bucket], start, now))
			}
		}
	}
	if unobserved != nil {
		if len(findings) == 0 {
			return nil, unobserved
		}
		findings = append(findings, cannotObserveFinding(modernGateReadinessTarget, unobserved))
	}
	if len(findings) == 0 {
		return healthy, nil
	}
	return findings, nil
}

// The outcomes other than modern, as the gate's shadow names them.
func modernGateReadinessOutcomesText(outcomes map[string]float64) string {
	texts := []string{}
	for _, outcome := range model.ProviderModernOutcomes {
		if outcome != model.ProviderModernOutcomeModern {
			texts = append(texts, fmt.Sprintf("%s=%.0f", outcome, outcomes[outcome]))
		}
	}
	return strings.Join(texts, " ")
}

// What the provider.yml overrides do, the action every finding names.
const modernGateReadinessOverrides = "set `quality_speed_requires_modern_provider: false` in provider.yml to keep the gate off (the kill switch; the next score export restores the buckets), or move the flip with `quality_speed_modern_provider_start: <RFC 3339 instant or date>`. Either is a config release through the config-updater and a restart of every taskworker and API block, since a running container keeps the config it was created with (capprobe/README.md, \"Switching it on\", has the same procedure), and it postpones the flip only when done before the start. A switch given without a value, or a start that cannot be read, keeps the gate off and is logged, so confirm the moved start on `urnetwork_stats_provider_modern_gate_start_seconds` or with `bringyourctl provider inspect` after the restart"

func modernGateNotReadyFinding(bucket string, outcomes map[string]float64, admitted float64, removed float64, start time.Time, now time.Time, tier string) finding {
	share := removed / admitted
	return finding{
		probeId: modernGateReadinessProbeId, tier: tier, class: "modern-gate-not-ready",
		target: modernGateReadinessTarget, frame: bucket, sustain: 2,
		symptom:   fmt.Sprintf("The modern provider gate turns on at %s and would take %.0f%% of the %s bucket out: %.0f of the %.0f providers the other rules admit are not shown to be modern", start.Format(time.RFC3339), 100*share, bucket, removed, admitted),
		mechanism: "With quality_speed_requires_modern_provider absent from provider.yml, every pass from the start takes a provider whose capability-probe evidence is not modern (not_post_quantum, no_resizable_windows, no_evidence or stale_evidence) out of Quality and Speed under not_modern, keeping it Online. The shadow gauge counts exactly those providers at every dashboard refresh, whether or not the gate is on. no_evidence dominates while the capability probe (server/capprobe) is switched off, as it ships, or has not swept the eligible providers.",
		baseline:  "From 72 hours before the start, at most half of each native bucket's admitted providers lack modern evidence. WARN from 72 hours before the start, PAGE from 24 hours before; exactly half does not fire.",
		observed:  fmt.Sprintf("bucket=%s admitted=%.0f modern=%.0f removed=%.0f removed_share=%.3f %s start=%s lead=%s", bucket, admitted, outcomes[model.ProviderModernOutcomeModern], removed, share, modernGateReadinessOutcomesText(outcomes), start.Format(time.RFC3339), start.Sub(now).Round(time.Minute)),
		evidence:  "max by (bucket, outcome) of urnetwork_stats_provider_modern_gate_shadow from the shared dashboard snapshot every taskworker publishes, with urnetwork_stats_provider_modern_gate_start_seconds from the taskworkers' own provider.yml; counts no older than 30 minutes by urnetwork_stats_provider_egress_source_started_seconds.",
		context:   "The gate is bucket membership only: probes, explicit client ids and force_minimum still reach a provider it takes out, and a Quality or Speed answer borrows from Online once both native buckets run short (§2.19b backfill sustained). The dashboard counts cover connected valid public providers, the population of the score export's Online pool.",
		action:    "Decide before the start. To postpone, " + modernGateReadinessOverrides + ". To let it flip, make the evidence cover the buckets first: switch the capability probe on (`enabled: true` in capability_probe.yml, a config release and a taskworker restart, server/capprobe/README.md) and let it sweep the eligible providers until no_evidence is small, then confirm the remaining not-modern providers are understood.",
		verify:    "The shadow's not-modern share is at most half in both buckets on two consecutive reads, or the start gauge disappears (the switch is set) or moves the start beyond 72 hours.",
		playbook:  "SIGNALS.md §2.29 and FP2FIX.md (modern provider gate)",
	}
}

// earliest and start are the earliest and the latest start the taskworkers
// schedule; the tier is WARN before the earliest and PAGE from it.
func modernGateFleetDisagreesFinding(observation modernGateReadinessObservation, earliest time.Time, start time.Time, now time.Time, tier string) finding {
	onIn := "none of them"
	switch {
	case observation.onEvery:
		onIn = "all of them"
	case observation.onAny:
		onIn = "some of them"
	}
	when := "before its start at " + earliest.Format(time.RFC3339)
	if !now.Before(earliest) {
		when = "since its start at " + earliest.Format(time.RFC3339)
	}
	starts := earliest.Format(time.RFC3339)
	if earliest.Before(start) {
		starts = "from " + starts + " to " + start.Format(time.RFC3339)
	}
	boolValue := func(value bool) int {
		if value {
			return 1
		}
		return 0
	}
	return finding{
		probeId: modernGateReadinessProbeId, tier: tier, class: "modern-gate-fleet-disagrees",
		target: modernGateReadinessTarget, sustain: 2,
		symptom:   fmt.Sprintf("The taskworkers disagree on the modern provider gate %s: of %d running, %d publish the gate, it is on in %s, and %d schedule a start (%s)", when, observation.taskworkers, observation.publishing, onIn, observation.scheduling, starts),
		mechanism: "Each taskworker decides the gate from its own code and config. One running code without the automatic start publishes no gate and never flips; one created before a config release that set the switch or moved the start decides the old way, flipping when the others do not, or at another time. The score export a taskworker runs publishes the buckets FindProviders2 answers from, so where they disagree Quality and Speed follow whichever taskworker ran the last pass, and the dashboard counts the taskworker that filled the last snapshot.",
		baseline:  "Every running taskworker publishes urnetwork_stats_provider_modern_gate_on and they agree: its count equals the running taskworker processes, its min equals its max, and while a start is ahead every one publishes the same urnetwork_stats_provider_modern_gate_start_seconds. WARN from 72 hours before the earliest start, PAGE from it until 18:00 UTC on the latest start's day.",
		observed:  fmt.Sprintf("taskworkers=%d publishing=%d gate_on_min=%d gate_on_max=%d scheduling=%d earliest_start=%s start=%s now_minus_earliest_start=%s", observation.taskworkers, observation.publishing, boolValue(observation.onEvery), boolValue(observation.onAny), observation.scheduling, earliest.Format(time.RFC3339), start.Format(time.RFC3339), now.Sub(earliest).Round(time.Minute)),
		evidence:  "count, min and max of urnetwork_stats_provider_modern_gate_on, count, min and max of urnetwork_stats_provider_modern_gate_start_seconds, and the count of process_start_time_seconds, for job taskworker; no host or process identity leaves Mimir.",
		context:   "A new taskworker publishes the gate within a minute or two of its start, which the two-read sustain absorbs. A config release that sets the switch or moves the start makes the fleet disagree until its last taskworker has restarted and the drained processes' series have gone stale, up to five minutes after each drain, so the kill switch's own rollout after the start can page until it completes. API processes decide the gate for their own readers but publish no gauge, so this does not see them.",
		action:    "Find the taskworkers whose urnetwork_stats_provider_modern_gate_on or urnetwork_stats_provider_modern_gate_start_seconds is missing or differs (by host and block), confirm each runs the build with the automatic start and its host publishes the current config (WARP_CONFIG_VERSION), and restart or redeploy those that differ. To keep the gate off fleet-wide instead, " + modernGateReadinessOverrides + ".",
		verify:    "The count of urnetwork_stats_provider_modern_gate_on equals the running taskworkers, its min equals its max, and the starts agree, on two consecutive reads.",
		playbook:  "SIGNALS.md §2.29 and FP2FIX.md (modern provider gate)",
	}
}

func modernGateFlipDropFinding(bucket string, before float64, after float64, outcomes map[string]float64, start time.Time, now time.Time) finding {
	share := (before - after) / before
	return finding{
		probeId: modernGateReadinessProbeId, tier: tierPage, class: "modern-gate-flip-drop",
		target: modernGateReadinessTarget, frame: bucket, sustain: 1,
		symptom:   fmt.Sprintf("The %s bucket fell from %.0f to %.0f providers (%.0f%%) since the modern provider gate turned on at %s", bucket, before, after, 100*share, start.Format(time.RFC3339)),
		mechanism: "From the start every pass takes a provider whose capability-probe evidence is not modern out of Quality and Speed under not_modern. The dashboard counts flip at the start, and the next score export publishes the smaller buckets that FindProviders2 answers from; users of the bucket are served by the remaining providers, then by backfill from Online.",
		baseline:  "Until 18:00 UTC on the start's day, and at least an hour after the start, each native bucket keeps at least half the providers it had in the last sample before the start. A bucket that recovers, or a gate turned off everywhere, clears it.",
		observed:  fmt.Sprintf("bucket=%s before=%.0f now=%.0f fell_share=%.3f shadow: %s start=%s since=%s", bucket, before, after, share, modernGateReadinessOutcomesText(outcomes), start.Format(time.RFC3339), now.Sub(start).Round(time.Minute)),
		evidence:  "sum by (bucket) (max by (bucket, index) (urnetwork_stats_provider_egress_index)) at a second before the start and now, from counts whose source read started after the start; the shadow names the outcomes the gate takes out.",
		context:   "A drop that the shadow does not explain (not-modern outcomes well below the drop) has another cause, such as a URL probe outage (§2.19, §2.23); the gate is then not the thing to turn off.",
		action:    "If the shadow explains the drop, turn the gate off: " + modernGateReadinessOverrides + ". Then repair the capability evidence (no_evidence, stale_evidence) before turning it on again.",
		verify:    "The next score export after the config release and restart restores the bucket (urnetwork_stats_provider_excluded{reason=\"not_modern\"} returns to zero), and §2.19b backfill subsides.",
		playbook:  "SIGNALS.md §2.29 and FP2FIX.md (modern provider gate)",
	}
}
