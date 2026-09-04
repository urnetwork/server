package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"
)

const (
	missingOriginRange         = "5m"
	missingOriginWarnPerMinute = 500.0
	missingOriginFreshness     = 90 * time.Second

	missingOriginAggregateMetric = "aggregate"
	missingOriginDetailMetric    = "detail"
)

// SIGNALS.md §2.17 maps to signal_missing_origin.go and
// signal_missing_origin_test.go. It measures the lossless API counter for
// originally non-companion requests that entered companion settlement without
// a usable reverse origin, then consumes only reconciled fixed-vocabulary
// resolution/relationship/lifecycle detail. No customer identity leaves Mimir.
func NewMissingOriginSignal() Signal {
	return &signalAdapter{
		number: "2.17", key: "missing-origin", name: "Missing-origin return-path rate",
		probe: missingOriginProbe{},
	}
}

type missingOriginProbe struct{}

func (missingOriginProbe) id() string             { return "mimir/missing-origin" }
func (missingOriginProbe) tier() string           { return tierWarn }
func (missingOriginProbe) cadence() time.Duration { return time.Minute }

func missingOriginQuery(environment string) string {
	return fmt.Sprintf(
		`label_replace((sum(rate(urnetwork_connect_contract_failures_total{env=%s,cause="missing_companion_origin",companion="false"}[%s])) * 60),"monitor_metric","%s","__name__",".*") or label_replace((sum by (resolution,relationship,source_lifecycle,destination_lifecycle) (rate(urnetwork_connect_missing_origin_details_total{env=%s,request_companion="false"}[%s])) * 60),"monitor_metric","%s","__name__",".*")`,
		strconv.Quote(environment),
		missingOriginRange,
		missingOriginAggregateMetric,
		strconv.Quote(environment),
		missingOriginRange,
		missingOriginDetailMetric,
	)
}

type missingOriginDetailKey struct {
	resolution           string
	relationship         string
	sourceLifecycle      string
	destinationLifecycle string
}

type missingOriginDetailSummary struct {
	status       string
	reason       string
	series       int
	totalRate    float64
	dominant     missingOriginDetailKey
	dominantRate float64
}

func (missingOriginProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("missing origin: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(missingOriginQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("missing origin: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("missing origin: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"missing origin: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	aggregateSeries := []int{}
	for seriesIndex, series := range response.Data.Result {
		if series.Metric["monitor_metric"] == missingOriginAggregateMetric {
			aggregateSeries = append(aggregateSeries, seriesIndex)
		}
	}
	if len(aggregateSeries) != 1 {
		// CounterVec label families are created by traffic, not registration.
		// An absent vector therefore cannot distinguish a perfect zero from
		// missing API instrumentation or ingestion and must stay unknown.
		return nil, fmt.Errorf("missing origin: Mimir returned %d fallback aggregate series, want 1", len(aggregateSeries))
	}
	aggregate := response.Data.Result[aggregateSeries[0]]
	observedAt, rate, err := mimirInstantValue(aggregate.Value)
	if err != nil {
		return nil, fmt.Errorf("missing origin: parse fallback sample: %w", err)
	}
	now := env.now().UTC()
	age := now.Sub(observedAt)
	if age > missingOriginFreshness || age < -30*time.Second {
		return nil, fmt.Errorf("missing origin: stale fallback sample age=%s", age.Round(time.Second))
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
		return nil, fmt.Errorf("missing origin: invalid fallback rate %v", rate)
	}
	if rate <= missingOriginWarnPerMinute {
		return []finding{healthyFinding(
			"mimir/missing-origin", tierWarn, "missing-origin-rate", "api-fleet",
		)}, nil
	}

	detail := inspectMissingOriginDetails(response, observedAt, rate, now)
	detailObserved, detailEvidence, detailContext := missingOriginDetailNarrative(detail)

	return []finding{{
		probeId: "mimir/missing-origin", tier: tierWarn,
		class: "missing-origin-rate", target: "api-fleet", frame: "fallback-from-normal", sustain: 1,
		symptom: fmt.Sprintf(
			"missing companion-origin contract failures from non-companion requests are %.1f/min over five minutes",
			rate,
		),
		mechanism: "The wire request was non-companion, but contract resolution fell back to Stream/companion settlement because the destination did not currently advertise the relationship mode. The fallback then found no reverse origin. This can be a selected stale destination, a same-network return to a Stream-only client, a provider-mode transition, or a retained client window; the request bit alone does not identify which path produced it.",
		baseline: fmt.Sprintf(
			"The companion=false partition remains at or below %.0f/min; the 2026-09-02 healthy midday control was about 130-190/min.",
			missingOriginWarnPerMinute,
		),
		observed: fmt.Sprintf(
			"companion=false rate_per_minute=%.3f range=%s sample_time=%s metrics_gateway=%s %s",
			rate,
			missingOriginRange,
			observedAt.Format(time.RFC3339),
			metricHost.name,
			detailObserved,
		),
		evidence: "Mimir evaluates the five-minute rate of the bounded API counter across the fleet for companion=false and exports one aggregate rate. " + detailEvidence + " No customer, client, network, device, contract, destination, or API-process identifier enters the metric cohorts; the standard bounded metrics-gateway name remains operational context.",
		context:  "companion=false records the original wire bit, not the role of the request or the branch ultimately taken. resolveNonCompanionProvideMode can convert that request to companion fallback. Provider discovery is one producer, but provider return paths and same-network peers also create contracts. " + detailContext + " A 2026-09-02 identifier-free production cohort found the live pre-guard fault entirely in same-network returns from active top-level sources to already-inactive derived destinations; that dated result is a discriminator, not a permanent assumption about future incidents. Genuine companion=true traffic has a separate, substantially higher workload-dependent background and is context until its own healthy band is established.",
		action:   "First require §2.8, §2.9, §2.15, and §2.16 to be healthy and verify bounded score-cache samples contain only contractable active top-level providers, then run §2.20 and §2.18. If successful stale contracts remain nonzero or §2.18 is unobservable because API artifacts predate server commit c8dfe570, satisfy the selected artifact's migration prerequisite and deploy the lifecycle guard; do not wait out a legacy client cohort while the server still authorizes dead routes. After API convergence, use the bounded rejection and lifecycle/relationship dimensions to locate any stale producer, and let Connect-bearing clients age through their maximum client-window lifetime; do not infer endpoint roles from companion=false, print identifiers, edit Redis blobs, relax provider gates, or restart clients to manufacture recovery.",
		verify:   "§2.20 reports zero successful contracts to already-inactive destinations for two complete five-minute windows, §2.18 exposes both initialized partitions, the companion=false missing-origin rate remains below 500/min for the same windows after the deployed client-window lifetime, selection controls remain healthy, and an end-to-end provider route succeeds without manual state changes.",
		playbook: "SIGNALS.md §2.17 and §5.9",
	}}, nil
}

func inspectMissingOriginDetails(
	response mimirInstantResponse,
	aggregateObservedAt time.Time,
	aggregateRate float64,
	now time.Time,
) missingOriginDetailSummary {
	summary := missingOriginDetailSummary{status: "absent"}
	seen := map[missingOriginDetailKey]bool{}
	dominantKey := ""
	for _, series := range response.Data.Result {
		metricClass := series.Metric["monitor_metric"]
		if metricClass == missingOriginAggregateMetric {
			continue
		}
		if metricClass != missingOriginDetailMetric {
			return missingOriginDetailSummary{status: "ambiguous", reason: "unexpected_metric_class"}
		}
		summary.series++
		if len(series.Metric) != 5 {
			return missingOriginDetailSummary{status: "ambiguous", reason: "unexpected_detail_label_set", series: summary.series}
		}
		key := missingOriginDetailKey{
			resolution:           series.Metric["resolution"],
			relationship:         series.Metric["relationship"],
			sourceLifecycle:      series.Metric["source_lifecycle"],
			destinationLifecycle: series.Metric["destination_lifecycle"],
		}
		if !validMissingOriginDetailKey(key) {
			return missingOriginDetailSummary{status: "ambiguous", reason: "invalid_detail_labels", series: summary.series}
		}
		if seen[key] {
			return missingOriginDetailSummary{status: "ambiguous", reason: "duplicate_detail_series", series: summary.series}
		}
		seen[key] = true

		observedAt, detailRate, err := mimirInstantValue(series.Value)
		if err != nil {
			return missingOriginDetailSummary{status: "ambiguous", reason: "malformed_detail_sample", series: summary.series}
		}
		age := now.Sub(observedAt)
		if age > missingOriginFreshness || age < -30*time.Second {
			return missingOriginDetailSummary{status: "ambiguous", reason: "stale_detail_sample", series: summary.series}
		}
		if !observedAt.Equal(aggregateObservedAt) {
			return missingOriginDetailSummary{status: "ambiguous", reason: "detail_sample_time_skew", series: summary.series}
		}
		if math.IsNaN(detailRate) || math.IsInf(detailRate, 0) || detailRate < 0 {
			return missingOriginDetailSummary{status: "ambiguous", reason: "invalid_detail_rate", series: summary.series}
		}
		summary.totalRate += detailRate
		keyText := missingOriginDetailKeyText(key)
		if summary.dominantRate < detailRate ||
			(summary.dominantRate == detailRate && (dominantKey == "" || keyText < dominantKey)) {
			summary.dominant = key
			summary.dominantRate = detailRate
			dominantKey = keyText
		}
	}
	if summary.series == 0 {
		return summary
	}

	// Both counters increment in the same API failure path, but Prometheus can
	// scrape between those increments. Allow a small bounded rate tolerance so
	// that one scrape-boundary race is not called a mixed rollout; a missing API
	// generation still leaves a much larger deficit at incident rates.
	tolerance := math.Max(1, aggregateRate*0.02)
	difference := summary.totalRate - aggregateRate
	if math.Abs(difference) <= tolerance {
		summary.status = "complete"
		return summary
	}
	if difference < 0 {
		summary.status = "partial"
		summary.reason = "detail_rate_below_aggregate"
		return summary
	}
	summary.status = "ambiguous"
	summary.reason = "detail_rate_above_aggregate"
	return summary
}

func validMissingOriginDetailKey(key missingOriginDetailKey) bool {
	return missingOriginValueAllowed(key.resolution,
		"requested_companion", "stream_fallback", "network_normalized", "relationship", "rejected", "unknown") &&
		missingOriginValueAllowed(key.relationship, "network", "friends_family", "public", "unknown") &&
		missingOriginValueAllowed(key.sourceLifecycle,
			"missing", "active_top", "inactive_top", "active_derived", "inactive_derived", "control", "unknown") &&
		missingOriginValueAllowed(key.destinationLifecycle,
			"missing", "active_top", "inactive_top", "active_derived", "inactive_derived", "control", "unknown")
}

func missingOriginValueAllowed(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func missingOriginDetailKeyText(key missingOriginDetailKey) string {
	return key.resolution + "/" + key.relationship + "/" + key.sourceLifecycle + "/" + key.destinationLifecycle
}

func missingOriginDetailNarrative(summary missingOriginDetailSummary) (string, string, string) {
	switch summary.status {
	case "absent":
		return "detail_status=absent detail_series=0 detail_rate_per_minute=unknown",
			"The bounded resolution/relationship/lifecycle query returned no cohort series while the aggregate failure rate was nonzero, so missing detail is unavailable instrumentation rather than a measured zero.",
			"Absent detail is consistent with an API generation predating c8dfe570, a mixed rollout, or detail-series ingestion loss. Use the initialized §2.18 partitions and exact API artifacts to choose that boundary; do not attribute the aggregate to a route class until detail is complete."
	case "complete":
		share := 0.0
		if 0 < summary.totalRate {
			share = 100 * summary.dominantRate / summary.totalRate
		}
		return fmt.Sprintf(
				"detail_status=complete detail_series=%d detail_rate_per_minute=%.3f dominant_resolution=%s dominant_relationship=%s dominant_source_lifecycle=%s dominant_destination_lifecycle=%s dominant_rate_per_minute=%.3f dominant_share_percent=%.1f",
				summary.series,
				summary.totalRate,
				summary.dominant.resolution,
				summary.dominant.relationship,
				summary.dominant.sourceLifecycle,
				summary.dominant.destinationLifecycle,
				summary.dominantRate,
				share,
			),
			"Every returned detail label belongs to the producer's fixed vocabulary, cohort sample times match the aggregate, no cohort is duplicated, and the summed detail rate reconciles with the aggregate inside the documented scrape-boundary tolerance.",
			"The dominant detail cohort is safe causal context from the request-time database snapshot, not an endpoint identity. Use its joint resolution, relationship, and lifecycle shape to distinguish fallback/return traffic from other paths without inferring a customer or device."
	case "partial":
		return fmt.Sprintf(
				"detail_status=partial detail_series=%d detail_rate_per_minute=%.3f detail_error=%s",
				summary.series,
				summary.totalRate,
				summary.reason,
			),
			"The bounded detail series were structurally valid but their summed rate did not cover the aggregate failure rate.",
			"Partial detail indicates mixed API generations or incomplete metric ingestion. Converge the API fleet and wait a complete five-minute range before using any detail cohort for causal attribution."
	default:
		return fmt.Sprintf(
				"detail_status=ambiguous detail_series=%d detail_rate_per_minute=unknown detail_error=%s",
				summary.series,
				summary.reason,
			),
			"The bounded detail response could not be validated or reconciled; only a fixed structural reason is exported and raw labels or samples are discarded.",
			"Ambiguous detail is an observation-contract failure, not evidence for any customer, route, or lifecycle class. Repair the API generation or metric path before using the detail family for attribution; the independently valid aggregate rate remains actionable."
	}
}
