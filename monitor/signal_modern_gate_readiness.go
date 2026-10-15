package monitor

import (
	"context"
	"encoding/json"
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
// bucket falls by more than half within an hour after the start. The start,
// and whether one is scheduled at all, comes from the taskworkers' own
// provider.yml as their stats collector publishes it, so postponing the gate
// or moving its start moves or silences the signal.
func NewModernGateReadinessSignal() Signal {
	return &signalAdapter{number: "2.29", key: "modern-gate-readiness", name: "Modern provider gate readiness", probe: modernGateReadinessProbe{}}
}

const (
	// how long before the start the shadow is read, and WARNs
	modernGateReadinessWarnLead = 72 * time.Hour
	// how long before the start a gutting shadow PAGEs
	modernGateReadinessPageLead = 24 * time.Hour
	// how long after the start a fallen bucket PAGEs
	modernGateReadinessFlipWatch = time.Hour
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
	// some taskworker publishes the gate's schedule
	published bool
	// the gate holds Quality and Speed on some taskworker
	on bool
	// the scheduled start, nil while provider.yml sets the switch and no flip
	// is scheduled
	start *time.Time
	// when the dashboard counts' source read started, zero when unknown
	countsStarted time.Time
	// bucket, then the gate's outcome, to the providers the other rules admit
	shadow map[string]map[string]float64
	// bucket to its providers now
	buckets map[string]float64
	// bucket to its providers in the last sample before the start, read only
	// within the hour after it
	bucketsBefore map[string]float64
}

// One instant query's parts, each a fixed label value, so a single round
// trip reads the schedule, the shadow, the buckets and the counts' age.
func modernGateReadinessQuery(environment string) string {
	selector := "env=" + strconv.Quote(environment)
	part := func(expression string, name string) string {
		return "label_replace(" + expression + `,"monitor_modern_gate_part",` + strconv.Quote(name) + `,"","")`
	}
	return strings.Join([]string{
		part("max(urnetwork_stats_provider_modern_gate_on{"+selector+"})", "on"),
		part("max(urnetwork_stats_provider_modern_gate_start_seconds{"+selector+"})", "start"),
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
	if observation.start != nil && !now.Before(*observation.start) && now.Before(observation.start.Add(modernGateReadinessFlipWatch)) {
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
// warning lead before it to the watch after it.
func modernGateReadinessInWindow(start time.Time, now time.Time) bool {
	return !now.Before(start.Add(-modernGateReadinessWarnLead)) && now.Before(start.Add(modernGateReadinessFlipWatch))
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
		case "on", "start", "counts_started":
			if len(series) != 1 {
				return invalid(part)
			}
			value := series[0].value
			switch part {
			case "on":
				observation.published = true
				observation.on = 1 <= value
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
// failure: the shadow, the counts' age, or the buckets before the start
// could not be read where the verdict needs them.
func evaluateModernGateReadiness(observation modernGateReadinessObservation, now time.Time) ([]finding, error) {
	healthy := []finding{healthyFinding(modernGateReadinessProbeId, tierPage, "modern-gate-not-ready", modernGateReadinessTarget)}
	if !observation.published {
		// the taskworkers do not run the code that flips the gate, or their
		// stats collector is down: the code's default start places the windows
		start := model.DefaultQualitySpeedModernProviderStart()
		if modernGateReadinessInWindow(start, now) {
			return nil, fmt.Errorf("modern gate readiness: no taskworker publishes urnetwork_stats_provider_modern_gate_on, so the automatic flip at %s is unconfirmed", start.Format(time.RFC3339))
		}
		return healthy, nil
	}
	if observation.start == nil {
		// provider.yml sets the switch: no flip is scheduled
		return healthy, nil
	}
	start := *observation.start
	if !modernGateReadinessInWindow(start, now) {
		return healthy, nil
	}

	if now.Before(start) {
		if observation.countsStarted.IsZero() || modernGateReadinessMaxCountsAge < now.Sub(observation.countsStarted) {
			return nil, fmt.Errorf("modern gate readiness: the dashboard counts are older than %s (urnetwork_stats_provider_egress_source_started_seconds), so the shadow cannot be judged", modernGateReadinessMaxCountsAge)
		}
		tier := tierWarn
		if !now.Before(start.Add(-modernGateReadinessPageLead)) {
			tier = tierPage
		}
		findings := []finding{}
		for _, bucket := range modernGateReadinessBuckets {
			outcomes := observation.shadow[bucket]
			if len(outcomes) != len(model.ProviderModernOutcomes) {
				return nil, fmt.Errorf("modern gate readiness: the %s shadow does not carry every outcome", bucket)
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
		if len(findings) == 0 {
			return healthy, nil
		}
		return findings, nil
	}

	// within the hour after the start: only counts read after it, by
	// taskworkers that have flipped, show the flip, so earlier ones are no
	// verdict yet
	if !observation.on || observation.countsStarted.Before(start) {
		return healthy, nil
	}
	findings := []finding{}
	for _, bucket := range modernGateReadinessBuckets {
		before, ok := observation.bucketsBefore[bucket]
		if !ok {
			return nil, fmt.Errorf("modern gate readiness: no %s bucket sample before the start", bucket)
		}
		after, ok := observation.buckets[bucket]
		if !ok {
			return nil, fmt.Errorf("modern gate readiness: no %s bucket sample now", bucket)
		}
		if 0 < before && (before-after) > modernGateReadinessMaxShare*before {
			findings = append(findings, modernGateFlipDropFinding(bucket, before, after, observation.shadow[bucket], start, now))
		}
	}
	if len(findings) == 0 {
		return []finding{healthyFinding(modernGateReadinessProbeId, tierPage, "modern-gate-flip-drop", modernGateReadinessTarget)}, nil
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
const modernGateReadinessOverrides = "set `quality_speed_requires_modern_provider: false` in provider.yml to keep the gate off (the kill switch; the next score export restores the buckets), or move the flip with `quality_speed_modern_provider_start: <RFC 3339 instant>`. Either is a config release through the config-updater and a restart of every taskworker and API block, since a running container keeps the config it was created with (capprobe/README.md, \"Switching it on\", has the same procedure), and it postpones the flip only when done before the start. A malformed start keeps the code's default, so confirm the moved start on `urnetwork_stats_provider_modern_gate_start_seconds` or with `bringyourctl provider inspect` after the restart"

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

func modernGateFlipDropFinding(bucket string, before float64, after float64, outcomes map[string]float64, start time.Time, now time.Time) finding {
	share := (before - after) / before
	return finding{
		probeId: modernGateReadinessProbeId, tier: tierPage, class: "modern-gate-flip-drop",
		target: modernGateReadinessTarget, frame: bucket, sustain: 1,
		symptom:   fmt.Sprintf("The %s bucket fell from %.0f to %.0f providers (%.0f%%) within an hour of the modern provider gate turning on at %s", bucket, before, after, 100*share, start.Format(time.RFC3339)),
		mechanism: "From the start every pass takes a provider whose capability-probe evidence is not modern out of Quality and Speed under not_modern. The dashboard counts flip at the start, and the next score export publishes the smaller buckets that FindProviders2 answers from; users of the bucket are served by the remaining providers, then by backfill from Online.",
		baseline:  "Within the hour after the start, each native bucket keeps at least half the providers it had in the last sample before the start.",
		observed:  fmt.Sprintf("bucket=%s before=%.0f now=%.0f fell_share=%.3f shadow: %s start=%s since=%s", bucket, before, after, share, modernGateReadinessOutcomesText(outcomes), start.Format(time.RFC3339), now.Sub(start).Round(time.Minute)),
		evidence:  "sum by (bucket) (max by (bucket, index) (urnetwork_stats_provider_egress_index)) at a second before the start and now, from counts whose source read started after the start; the shadow names the outcomes the gate takes out.",
		context:   "A drop that the shadow does not explain (not-modern outcomes well below the drop) has another cause, such as a URL probe outage (§2.19, §2.23); the gate is then not the thing to turn off.",
		action:    "If the shadow explains the drop, turn the gate off: " + modernGateReadinessOverrides + ". Then repair the capability evidence (no_evidence, stale_evidence) before turning it on again.",
		verify:    "The next score export after the config release and restart restores the bucket (urnetwork_stats_provider_excluded{reason=\"not_modern\"} returns to zero), and §2.19b backfill subsides.",
		playbook:  "SIGNALS.md §2.29 and FP2FIX.md (modern provider gate)",
	}
}
