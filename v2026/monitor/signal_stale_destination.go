package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	staleDestinationRange         = "5m"
	staleDestinationWarnPerMinute = 50.0
	staleDestinationFreshness     = 90 * time.Second

	staleDestinationAggregateMetric = "aggregate"
	staleDestinationDetailMetric    = "detail"
)

// SIGNALS.md §2.18 maps to signal_stale_destination.go and
// signal_stale_destination_test.go. It measures the API lifecycle guard that
// prevents a selected inactive identity from becoming a successful contract.
func NewStaleDestinationSignal() Signal {
	return &signalAdapter{
		number: "2.18", key: "stale-destination", name: "Stale contract destination rejection rate",
		probe: staleDestinationProbe{},
	}
}

type staleDestinationProbe struct{}

func (staleDestinationProbe) id() string             { return "mimir/stale-destination" }
func (staleDestinationProbe) tier() string           { return tierWarn }
func (staleDestinationProbe) cadence() time.Duration { return time.Minute }

func staleDestinationQuery(environment string) string {
	return fmt.Sprintf(
		`label_replace((sum by (companion) (rate(urnetwork_connect_contract_failures_total{env=%s,cause="inactive_destination"}[%s])) * 60),"monitor_metric","%s","__name__",".*") or label_replace((sum by (request_companion,sender_role,resolution,relationship,source_lifecycle,destination_lifecycle) (rate(urnetwork_connect_inactive_destination_details_total{env=%s}[%s])) * 60),"monitor_metric","%s","__name__",".*")`,
		strconv.Quote(environment),
		staleDestinationRange,
		staleDestinationAggregateMetric,
		strconv.Quote(environment),
		staleDestinationRange,
		staleDestinationDetailMetric,
	)
}

type staleDestinationDetailKey struct {
	requestCompanion     string
	senderRole           string
	resolution           string
	relationship         string
	sourceLifecycle      string
	destinationLifecycle string
}

type staleDestinationDetailSummary struct {
	status       string
	reason       string
	series       int
	totalRate    float64
	dominant     staleDestinationDetailKey
	dominantRate float64
	roleRates    map[string]float64
}

func (staleDestinationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("stale destination: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(staleDestinationQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("stale destination: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("stale destination: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"stale destination: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	rates := map[string]float64{}
	observedTimes := []time.Time{}
	for _, series := range response.Data.Result {
		if series.Metric["monitor_metric"] != staleDestinationAggregateMetric {
			continue
		}
		if len(series.Metric) != 2 {
			return nil, fmt.Errorf("stale destination: unexpected aggregate label set")
		}
		companion := series.Metric["companion"]
		if companion != "false" && companion != "true" {
			return nil, fmt.Errorf("stale destination: unexpected companion partition %q", companion)
		}
		if _, duplicate := rates[companion]; duplicate {
			return nil, fmt.Errorf("stale destination: duplicate companion partition %q", companion)
		}
		observedAt, rate, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("stale destination: parse companion=%s sample: %w", companion, err)
		}
		age := env.now().UTC().Sub(observedAt)
		if age > staleDestinationFreshness || age < -30*time.Second {
			return nil, fmt.Errorf("stale destination: stale companion=%s sample age=%s", companion, age.Round(time.Second))
		}
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			return nil, fmt.Errorf("stale destination: invalid companion=%s rate %v", companion, rate)
		}
		rates[companion] = rate
		observedTimes = append(observedTimes, observedAt)
	}
	missing := []string{}
	for _, companion := range []string{"false", "true"} {
		if _, ok := rates[companion]; !ok {
			missing = append(missing, companion)
		}
	}
	if len(missing) > 0 {
		present := make([]string, 0, len(rates))
		for companion := range rates {
			present = append(present, companion)
		}
		sort.Strings(present)
		return []finding{staleDestinationInstrumentationFinding(missing, present, metricHost.name)}, nil
	}
	sort.Slice(observedTimes, func(i, j int) bool { return observedTimes[i].Before(observedTimes[j]) })
	if !observedTimes[0].Equal(observedTimes[len(observedTimes)-1]) {
		return nil, fmt.Errorf(
			"stale destination: partition sample times differ: %s",
			strings.Join([]string{
				observedTimes[0].Format(time.RFC3339Nano),
				observedTimes[len(observedTimes)-1].Format(time.RFC3339Nano),
			}, " and "),
		)
	}

	totalRate := rates["false"] + rates["true"]
	if totalRate <= staleDestinationWarnPerMinute {
		return []finding{healthyFinding(
			"mimir/stale-destination", tierWarn, "stale-destination-rate", "api-fleet",
		)}, nil
	}
	detail := inspectStaleDestinationDetails(response, observedTimes[0], totalRate, env.now().UTC())
	detailObserved, detailEvidence, detailContext := staleDestinationDetailNarrative(detail)

	return []finding{{
		probeId: "mimir/stale-destination", tier: tierWarn,
		class: "stale-destination-rate", target: "api-fleet", frame: "lifecycle-rejection", sustain: 1,
		symptom: fmt.Sprintf(
			"API rejected inactive contract destinations at %.1f/min over five minutes",
			totalRate,
		),
		mechanism: "The active-only contract guard found the requested destination missing or inactive before provide-mode selection or at the final write boundary. This prevents the previous failure mode in which a stale Redis provide advertisement could authorize a successful contract to an identity that could no longer receive it.",
		baseline: fmt.Sprintf(
			"The aggregate inactive-destination rejection rate stays at or below %.0f/min, both companion partitions remain explicitly observable, and successful contracts to destinations already inactive at creation remain zero.",
			staleDestinationWarnPerMinute,
		),
		observed: fmt.Sprintf(
			"rate_per_minute=%.3f companion_false_rate=%.3f companion_true_rate=%.3f range=%s sample_time=%s metrics_gateway=%s %s",
			totalRate,
			rates["false"],
			rates["true"],
			staleDestinationRange,
			observedTimes[0].Format(time.RFC3339),
			metricHost.name,
			detailObserved,
		),
		evidence: "The API owns both counter families at the rejection boundary and initializes the two aggregate companion labels to zero. " + detailEvidence + " No customer, client, network, device, contract, destination, artifact version, or API-process identifier enters the metric cohorts.",
		context:  "These are prevented stale contracts, not successful routes and not a hardware-capacity signal. The API guard protects correctness immediately, but a sender can keep retrying the same dead exit. " + detailContext + " A Connect-bearing client that understands ContractError_Reliability retires only the emitting window channel and refills through the existing selection path.",
		action:   "First require every API artifact to contain server commit c8dfe570 and every affected Connect-bearing client artifact to contain Connect commit 5b33c91. To attribute a persistent rate, require an API artifact containing the inactive-destination detail consumer and require every Connect-bearing requester to contain Connect commit f8b1b60, which adds the optional sender-role producer. Use only a complete reconciled detail family: a concrete client/server value identifies the sender's sequence lane, while absent or unknown remains unattributed and never proves an old artifact. If the rate remains above the boundary after two complete five-minute windows and the maximum deployed client-window lifetime, check §2.8, §2.9, §2.15, and §2.16 and use the joint sender-role, resolution, relationship, and lifecycle cohort to locate the stale request lane. Do not delete Redis provide keys, weaken lifecycle checks, lengthen contract timeouts, or restart clients to manufacture recovery.",
		verify:   "Every API instance exports both initialized aggregate partitions; successful contracts to already-inactive destinations remain zero; detail is either explicitly unavailable or fully reconciled rather than partially attributed; the aggregate rejection rate stays at or below 50/min for two complete five-minute windows; and a Reliability result removes only its emitting exit before the window refills.",
		playbook: "SIGNALS.md §2.18 and §5.9",
	}}, nil
}

func inspectStaleDestinationDetails(
	response mimirInstantResponse,
	aggregateObservedAt time.Time,
	aggregateRate float64,
	now time.Time,
) staleDestinationDetailSummary {
	summary := staleDestinationDetailSummary{
		status:    "absent",
		roleRates: map[string]float64{},
	}
	seen := map[staleDestinationDetailKey]bool{}
	dominantKey := ""
	for _, series := range response.Data.Result {
		metricClass := series.Metric["monitor_metric"]
		if metricClass == staleDestinationAggregateMetric {
			continue
		}
		if metricClass != staleDestinationDetailMetric {
			return staleDestinationDetailSummary{status: "ambiguous", reason: "unexpected_metric_class"}
		}
		summary.series++
		if len(series.Metric) != 7 {
			return staleDestinationDetailSummary{status: "ambiguous", reason: "unexpected_detail_label_set", series: summary.series}
		}
		key := staleDestinationDetailKey{
			requestCompanion:     series.Metric["request_companion"],
			senderRole:           series.Metric["sender_role"],
			resolution:           series.Metric["resolution"],
			relationship:         series.Metric["relationship"],
			sourceLifecycle:      series.Metric["source_lifecycle"],
			destinationLifecycle: series.Metric["destination_lifecycle"],
		}
		if !validStaleDestinationDetailKey(key) {
			return staleDestinationDetailSummary{status: "ambiguous", reason: "invalid_detail_labels", series: summary.series}
		}
		if seen[key] {
			return staleDestinationDetailSummary{status: "ambiguous", reason: "duplicate_detail_series", series: summary.series}
		}
		seen[key] = true

		observedAt, detailRate, err := mimirInstantValue(series.Value)
		if err != nil {
			return staleDestinationDetailSummary{status: "ambiguous", reason: "malformed_detail_sample", series: summary.series}
		}
		age := now.Sub(observedAt)
		if age > staleDestinationFreshness || age < -30*time.Second {
			return staleDestinationDetailSummary{status: "ambiguous", reason: "stale_detail_sample", series: summary.series}
		}
		if !observedAt.Equal(aggregateObservedAt) {
			return staleDestinationDetailSummary{status: "ambiguous", reason: "detail_sample_time_skew", series: summary.series}
		}
		if math.IsNaN(detailRate) || math.IsInf(detailRate, 0) || detailRate < 0 {
			return staleDestinationDetailSummary{status: "ambiguous", reason: "invalid_detail_rate", series: summary.series}
		}
		summary.totalRate += detailRate
		summary.roleRates[key.senderRole] += detailRate
		keyText := staleDestinationDetailKeyText(key)
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

	// The aggregate and detail counters increment in the same failure path, but
	// a scrape can fall between those increments. A bounded tolerance accepts
	// that boundary without letting a mixed API rollout appear complete.
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

func validStaleDestinationDetailKey(key staleDestinationDetailKey) bool {
	return staleDestinationValueAllowed(key.requestCompanion, "false", "true") &&
		staleDestinationValueAllowed(key.senderRole, "client", "server", "absent", "unknown") &&
		staleDestinationValueAllowed(key.resolution,
			"requested_companion", "stream_fallback", "network_normalized", "relationship", "rejected", "unknown") &&
		staleDestinationValueAllowed(key.relationship, "network", "friends_family", "public", "unknown") &&
		staleDestinationValueAllowed(key.sourceLifecycle,
			"missing", "active_top", "inactive_top", "active_derived", "inactive_derived", "control", "unknown") &&
		staleDestinationValueAllowed(key.destinationLifecycle,
			"missing", "active_top", "inactive_top", "active_derived", "inactive_derived", "control", "unknown")
}

func staleDestinationValueAllowed(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func staleDestinationDetailKeyText(key staleDestinationDetailKey) string {
	return strings.Join([]string{
		key.requestCompanion,
		key.senderRole,
		key.resolution,
		key.relationship,
		key.sourceLifecycle,
		key.destinationLifecycle,
	}, "/")
}

func staleDestinationDetailNarrative(summary staleDestinationDetailSummary) (string, string, string) {
	switch summary.status {
	case "absent":
		return "detail_status=absent detail_series=0 detail_rate_per_minute=unknown",
			"The bounded joint sender-role/resolution/relationship/lifecycle query returned no cohort while the aggregate was nonzero, so detail is unavailable capability evidence rather than a measured zero.",
			"Absent detail is consistent with an API generation predating the detail consumer or detail-series ingestion loss. It cannot identify a request lane, caller, or artifact."
	case "complete":
		share := 0.0
		if 0 < summary.totalRate {
			share = 100 * summary.dominantRate / summary.totalRate
		}
		return fmt.Sprintf(
				"detail_status=complete detail_series=%d detail_rate_per_minute=%.3f sender_client_rate_per_minute=%.3f sender_server_rate_per_minute=%.3f sender_absent_rate_per_minute=%.3f sender_unknown_rate_per_minute=%.3f dominant_request_companion=%s dominant_sender_role=%s dominant_resolution=%s dominant_relationship=%s dominant_source_lifecycle=%s dominant_destination_lifecycle=%s dominant_rate_per_minute=%.3f dominant_share_percent=%.1f",
				summary.series,
				summary.totalRate,
				summary.roleRates["client"],
				summary.roleRates["server"],
				summary.roleRates["absent"],
				summary.roleRates["unknown"],
				summary.dominant.requestCompanion,
				summary.dominant.senderRole,
				summary.dominant.resolution,
				summary.dominant.relationship,
				summary.dominant.sourceLifecycle,
				summary.dominant.destinationLifecycle,
				summary.dominantRate,
				share,
			),
			"Every detail label belongs to a fixed producer vocabulary, sample times match the aggregate, no cohort is duplicated, and the summed detail rate reconciles with the aggregate inside the scrape-boundary tolerance.",
			"A concrete sender role proves only the reported ContractKey sequence lane and presence of the additive capability. Interpret it jointly with request companion, resolution, relationship, and lifecycle; absent is unavailable capability, unknown is an explicit malformed or future value, and neither identifies an old client or product artifact."
	case "partial":
		return fmt.Sprintf(
				"detail_status=partial detail_series=%d detail_rate_per_minute=%.3f detail_error=%s",
				summary.series,
				summary.totalRate,
				summary.reason,
			),
			"The fixed detail cohorts were structurally valid but their summed rate did not cover the aggregate failure rate.",
			"Partial detail is mixed API instrumentation or incomplete ingestion. It cannot identify a sender lane, caller, or artifact and must not be read as zero."
	default:
		return fmt.Sprintf(
				"detail_status=ambiguous detail_series=%d detail_rate_per_minute=unknown detail_error=%s",
				summary.series,
				summary.reason,
			),
			"The detail response failed fixed-label, sample, or rate reconciliation; only a bounded structural reason is exported and raw labels or samples are discarded.",
			"Ambiguous detail cannot identify a lane, caller, or artifact. Retain the independently valid aggregate alert while repairing or converging the observation path."
	}
}

func staleDestinationInstrumentationFinding(missing, present []string, metricHost string) finding {
	presentText := "none"
	if len(present) > 0 {
		presentText = strings.Join(present, ",")
	}
	return finding{
		probeId: "mimir/stale-destination", tier: tierWarn,
		class: "stale-destination-instrumentation", target: "api-fleet", frame: "initialized-partitions", sustain: 2,
		symptom:   "The API fleet does not expose every initialized stale-destination metric partition",
		mechanism: "The Mimir query completed, but its vector omitted one or both fixed companion partitions. The corrected API initializes both CounterVec children at process startup; absence therefore means the current API rollout predates that instrumentation, the newest processes have not yet accumulated a full rate window, or the scrape/remote-write path discarded the series.",
		baseline:  "A successful query returns exactly one fresh, same-timestamp rate for companion=false and companion=true, including explicit zeroes when no inactive destination was rejected.",
		observed: fmt.Sprintf(
			"missing_companion_partitions=%s present_companion_partitions=%s range=%s metrics_gateway=%s",
			strings.Join(missing, ","), presentText, staleDestinationRange, metricHost,
		),
		evidence: "The loopback Mimir endpoint returned a schema-valid vector, so this is a metric-generation or ingestion boundary rather than a failed host connection. No client, network, contract, or destination identifier enters the query or alert.",
		context:  "Missing instrumentation is UNKNOWN, not a healthy zero and not proof of a rejection-rate incident. API protects contract correctness; Connect-bearing clients separately consume the Reliability result to retire only the emitting route.",
		action:   "Compare every running API artifact and process start with server commit c8dfe570. If any predates it, deploy the corrected API fleet and wait one complete five-minute rate window after the last process converges. If every artifact contains the commit and the window has elapsed, inspect CounterVec initialization, scrape freshness, remote-write acceptance, and companion-label retention. Do not restart metrics, fabricate zeroes, or treat a Connect/Proxy rollout as restoring an API-owned series.",
		verify:   "Every API process contains c8dfe570; after one full five-minute warmup, both initialized partitions are present on two consecutive signal runs and the probe reports either an explicit healthy rate or the concrete stale-destination-rate class.",
		playbook: "SIGNALS.md §2.18 and §8.12",
	}
}
