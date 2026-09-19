package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §2.28 measures recent accepted-creation witnesses, route errors,
// demand-conditioned progress and total consistency, separately from §2.26.
func NewSignupLivenessSignal() Signal {
	return &signalAdapter{number: "2.28", key: "signup-liveness", name: "Current signup progress and total consistency", probe: signupLivenessProbe{}}
}

type signupLivenessProbe struct{}

func (signupLivenessProbe) id() string             { return "observability/signup-liveness" }
func (signupLivenessProbe) tier() string           { return tierWarn }
func (signupLivenessProbe) cadence() time.Duration { return 10 * time.Minute }

const signupLivenessQuery = `
/* monitor-signal-2.28-signup-liveness */
WITH clock AS MATERIALIZED (
 SELECT statement_timestamp() AT TIME ZONE 'UTC' AS now_utc,
        floor(extract(epoch FROM statement_timestamp()))::bigint AS observed_epoch
), events AS MATERIALIZED (
 SELECT event_time, event_type FROM audit_network_event CROSS JOIN clock
 WHERE event_time >= now_utc - interval '6 hours' AND event_time < now_utc
   AND event_type IN ('network_created', 'network_deleted')
)
SELECT (SELECT observed_epoch FROM clock), (SELECT count(*) FROM network),
 count(*) FILTER (WHERE event_type = 'network_created' AND event_time >= now_utc - interval '65 minutes' AND event_time < now_utc - interval '5 minutes'),
 count(*) FILTER (WHERE event_type = 'network_deleted' AND event_time >= now_utc - interval '65 minutes' AND event_time < now_utc - interval '5 minutes'),
 count(*) FILTER (WHERE event_time >= now_utc - interval '15 minutes'),
 count(*) FILTER (WHERE event_type = 'network_created'),
 count(*) FILTER (WHERE event_type = 'network_created' AND event_time >= now_utc - interval '70 minutes')
FROM events CROSS JOIN clock
`

type signupDurableSnapshot struct {
	at                              time.Time
	total, created, deleted, churn  int64
	createdSixHours, createdGuarded int64
}

// signupRouteMetricsUnavailableError is a bounded, semantic observation
// result: the exact route has no source series at the pinned evaluation time.
// It is not proof of zero traffic or a signup outage.
type signupRouteMetricsUnavailableError struct{}

func (*signupRouteMetricsUnavailableError) Error() string {
	return "signup route metrics are unavailable"
}

func parseSignupDurableSnapshot(rows []pgRow, now time.Time) (signupDurableSnapshot, error) {
	if len(rows) != 1 || len(rows[0]) != 7 {
		return signupDurableSnapshot{}, fmt.Errorf("signup durable query returned an invalid aggregate shape")
	}
	var values [7]int64
	for i := range values {
		value, err := parseStrictInt64(rows[0].str(i))
		if err != nil || value < 0 || value > 1<<53-1 {
			return signupDurableSnapshot{}, fmt.Errorf("signup durable query returned an invalid aggregate")
		}
		values[i] = value
	}
	if values[2] > values[6] || values[6] > values[5] {
		return signupDurableSnapshot{}, fmt.Errorf("signup durable creation windows are contradictory")
	}
	at := time.Unix(values[0], 0).UTC()
	if delta := now.Sub(at); delta < -30*time.Second || delta > 30*time.Second {
		return signupDurableSnapshot{}, fmt.Errorf("signup durable clock is outside the observation allowance")
	}
	return signupDurableSnapshot{at: at, total: values[1], created: values[2], deleted: values[3], churn: values[4], createdSixHours: values[5], createdGuarded: values[6]}, nil
}

func signupMetricQuery(environment string, demand bool) string {
	selector := `urnetwork_stats_total_networks{env=` + strconv.Quote(environment) + `,service="taskworker"}`
	fields := [][2]string{
		{"publishers", `count(` + selector + `)`},
		{"minimum", `min(` + selector + `)`},
		{"maximum", `max(` + selector + `)`},
		{"source_age", `time() - min(timestamp(` + selector + `))`},
	}
	if demand {
		selector = `urnetwork_http_requests_total{env=` + strconv.Quote(environment) + `,service="api",route="POST ^/auth/network-create$"}`
		// A handler can write a 2xx prefix and then terminate through a canceled
		// or aborted transport. Only the router's completed outcome is a response
		// the durable-audit cross-check may treat as successful.
		success := strings.TrimSuffix(selector, "}") + `,status=~"2..",outcome="completed"}`
		serverError := strings.TrimSuffix(selector, "}") + `,status=~"5.."}`
		fields = [][2]string{
			{"requests", `sum(increase(` + selector + `[1h] offset 5m))`},
			{"successes", `(sum(increase(` + success + `[1h] offset 5m)) or vector(0))`},
			{"server_errors", `(sum(increase(` + serverError + `[1h] offset 5m)) or vector(0))`},
			{"series", `count(` + selector + ` offset 5m)`},
			{"samples", `min(count_over_time(` + selector + `[1h] offset 5m))`},
			{"resets", `sum(resets(` + selector + `[1h] offset 5m))`},
			{"source_age", `time() - 300 - min(timestamp(` + selector + ` offset 5m))`},
		}
	}
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, `label_replace((`+field[1]+`), "monitor_signup", "`+field[0]+`", "", "")`)
	}
	return strings.Join(parts, " or ")
}

// Aggregate in Mimir before transport: neither series identities nor raw
// request labels are returned. Empty/warned/truncated/malformed results remain
// unknown; only absent status subsets of an observed route may be zero.
func observeSignupMetrics(ctx context.Context, env *probeEnv, at time.Time, demand bool) (map[string]float64, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("signup metrics have no configured service gateway")
	}
	queryUrl := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(signupMetricQuery(env.cfg.env, demand)) + "&time=" + strconv.FormatInt(at.Unix(), 10)
	out, _, err := shellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 65536 "+shellSingleQuote(queryUrl))
	if err != nil {
		return nil, err
	}
	return parseSignupMetrics(out, at, demand)
}

func parseSignupMetrics(raw string, at time.Time, demand bool) (map[string]float64, error) {
	if len(raw) > 65536 {
		return nil, fmt.Errorf("signup metric response exceeds its bound")
	}
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil || response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 {
		return nil, fmt.Errorf("signup metric response is incomplete or invalid")
	}
	fields := []string{"publishers", "minimum", "maximum", "source_age"}
	if demand {
		fields = []string{"requests", "successes", "server_errors", "series", "samples", "resets", "source_age"}
	}
	if len(response.Data.Result) != len(fields) {
		return nil, fmt.Errorf("signup metric response has missing or extra aggregates")
	}
	allowed := map[string]bool{}
	for _, field := range fields {
		allowed[field] = true
	}
	values := map[string]float64{}
	for _, row := range response.Data.Result {
		field := row.Metric["monitor_signup"]
		if !allowed[field] || len(row.Metric) != 1 {
			return nil, fmt.Errorf("signup metric response has an invalid aggregate label")
		}
		if _, duplicate := values[field]; duplicate {
			return nil, fmt.Errorf("signup metric response has a duplicate aggregate")
		}
		observed, value, err := mimirInstantValue(row.Value)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1<<53-1 || observed.Sub(at) < -time.Second || observed.Sub(at) > time.Second {
			return nil, fmt.Errorf("signup metric response has an invalid value or evaluation time")
		}
		values[field] = value
	}
	if values["source_age"] > 90 {
		return nil, fmt.Errorf("signup metric source samples are stale")
	}
	if demand {
		if values["series"] < 1 {
			return nil, &signupRouteMetricsUnavailableError{}
		}
		if values["samples"] < 2 || values["resets"] != 0 ||
			values["series"] != math.Trunc(values["series"]) || values["samples"] != math.Trunc(values["samples"]) ||
			values["successes"]+values["server_errors"] > values["requests"]+1e-6 {
			return nil, fmt.Errorf("signup demand is absent, reset, or contradictory")
		}
	} else if values["publishers"] < 1 || values["publishers"] != math.Trunc(values["publishers"]) ||
		values["minimum"] != math.Trunc(values["minimum"]) || values["maximum"] != math.Trunc(values["maximum"]) || values["minimum"] > values["maximum"] {
		return nil, fmt.Errorf("signup total publishers are absent or contradictory")
	}
	return values, nil
}

func (signupLivenessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := env.runner.pg(ctx, signupLivenessQuery)
	if err != nil {
		return nil, err
	}
	durable, err := parseSignupDurableSnapshot(rows, env.now().UTC())
	if err != nil {
		return nil, err
	}
	findings := []finding{signupWitnessFinding(durable)}
	for _, demand := range []bool{true, false} {
		metrics, err := observeSignupMetrics(ctx, env, durable.at, demand)
		if ctx.Err() != nil {
			return findings, ctx.Err()
		}
		if err != nil {
			component := "signup-liveness/published-total"
			if demand {
				component = "signup-liveness/route-demand"
			}
			findings = append(findings, cannotObserveFinding(component, err))
			continue
		}
		if demand {
			findings = append(findings, signupProgressFinding(durable, metrics), signupServerErrorsFinding(durable, metrics))
		} else {
			findings = append(findings, signupTotalFinding(durable, metrics))
		}
	}
	return findings, nil
}

func signupProgressFinding(durable signupDurableSnapshot, metrics map[string]float64) finding {
	if durable.createdGuarded > 0 || metrics["successes"] == 0 {
		return healthyFinding("observability/signup-liveness", tierWarn, "signup-no-durable-progress", "network-create")
	}
	return finding{
		probeId: "observability/signup-liveness", tier: tierWarn, class: "signup-no-durable-progress", target: "network-create", sustain: 2,
		symptom:   "Successful signup responses have no durable creation audit even with window-edge tolerance",
		mechanism: "A completed hour has a positive estimated 2xx count but no accepted-creation audit in an expanded seventy-minute window. This is a cross-source discrepancy, not proof that every request was valid or that a global outage occurred: missing/delayed audit writes and response misclassification remain competing explanations.",
		baseline:  "Any positive estimated 2xx count requires a creation witness; no request-volume floor suppresses a contradiction. The audit window extends five minutes before and after the completed metric hour, and the discrepancy must recur on two samples.",
		observed:  fmt.Sprintf("window_end_utc=%s window_seconds=3600 audit_created=%d audit_deleted=%d audit_created_guard_70m=%d requests_estimate=%.3f success_estimate=%.3f", durable.at.Add(-5*time.Minute).Format(time.RFC3339), durable.created, durable.deleted, durable.createdGuarded, metrics["requests"], metrics["successes"]),
		evidence:  "Primary PostgreSQL supplies only bounded creation/deletion counts. Mimir supplies fixed route aggregates for the completed one-hour window ending five minutes before the database clock; the audit guard spans the preceding seventy minutes. Prometheus increase is extrapolated, not an exact event count.",
		context:   "A flat total can reflect deletions balancing creations. Positive progress does not certify every auth branch or client, and a guarded witness is not a per-request join. Zero/low demand cannot prove current liveness; the independent six-hour witness and 5xx classes remain active. Missing/stale/reset metrics stay unknown.",
		action:    "Compare aggregate 2xx outcomes with durable accepted-creation audits and audit-write completion without retaining request bodies or identities. Check response classification and running API ancestry before attributing the discrepancy to infrastructure. Do not alter account policy or reset limiters from aggregate demand.",
		verify:    "After the authorized correction, observe two complete post-boundary one-hour windows with accepted creation audits under real demand and no unexplained route/audit discrepancy. Verify 400/409, unchanged 429/Retry-After, and successful signup controls independently; alert absence during low demand is insufficient.",
		playbook:  "SIGNALS.md §2.28 and §2.26",
	}
}

func signupWitnessFinding(durable signupDurableSnapshot) finding {
	if durable.createdSixHours > 0 {
		return healthyFinding("observability/signup-liveness", tierWarn, "signup-liveness-unproven", "network-create")
	}
	return finding{
		probeId: "observability/signup-liveness", tier: tierWarn, class: "signup-liveness-unproven", target: "network-create", sustain: 2,
		symptom:   "Current signup liveness has no recent durable accepted-creation witness",
		mechanism: "No network-created audit exists in the preceding six hours. This leaves current liveness unproven even when route demand is absent or low; it is an unknown capability boundary, not a signup outage. Lack of valid demand and incomplete audit observation remain alternatives to a broken creation path.",
		baseline:  "At least one durable accepted-creation audit in the bounded six-hour window is a recent progress witness, not a guarantee for every auth branch or request.",
		observed:  fmt.Sprintf("window_end_utc=%s witness_window_seconds=21600 audit_created_6h=%d", durable.at.Format(time.RFC3339), durable.createdSixHours),
		evidence:  "A bounded primary PostgreSQL query returns only the accepted-creation count and database clock; no account, network, contact, or request identifiers are emitted.",
		context:   "Expected 400 validation, 409 conflict, and 429 rate-limit refusals are not server failures and cannot establish a successful creation witness. Metric absence remains independently cannot-observe; a quiet route is not silently called healthy.",
		action:    "Establish recent accepted creation from natural traffic and compare independent route outcomes. Diagnose demand and audit completeness before claiming an outage. Do not create production accounts, change signup policy, or bypass rate limits without separate authorization.",
		verify:    "Require two ten-minute samples whose entire six-hour witness windows start after the correction boundary, or retain a separate aggregate proof of post-boundary accepted creation. Confirm route outcomes independently; a pre-boundary witness or absence of alerts alone does not prove recovery.",
		playbook:  "SIGNALS.md §2.28 and §2.26",
	}
}

func signupServerErrorsFinding(durable signupDurableSnapshot, metrics map[string]float64) finding {
	if metrics["server_errors"] == 0 {
		return healthyFinding("observability/signup-liveness", tierWarn, "signup-route-server-errors", "network-create")
	}
	return finding{
		probeId: "observability/signup-liveness", tier: tierWarn, class: "signup-route-server-errors", target: "network-create", sustain: 2,
		symptom:   "Signup returned server errors in the completed observation hour",
		mechanism: "The signup route has a positive estimated 5xx count, independently of successful or durable creations. Aggregate 5xx does not distinguish genuine internal failure, post-primary failure, or the known defect that reported expected client refusals as 500; successful traffic must not suppress this boundary.",
		baseline:  "No observed 5xx; any positive estimated count recurring on two samples warrants bounded classification. Expected 400/409/429 responses alone do not trigger this class.",
		observed:  fmt.Sprintf("window_end_utc=%s window_seconds=3600 requests_estimate=%.3f server_error_estimate=%.3f audit_created=%d", durable.at.Add(-5*time.Minute).Format(time.RFC3339), metrics["requests"], metrics["server_errors"], durable.created),
		evidence:  "Mimir supplies only fixed aggregate counters for the exact signup route and completed hour. Prometheus increase is extrapolated, not an exact event count; durable progress is independent context, not an error-suppression rule.",
		context:   "This is a route-error boundary, not evidence that all signup is broken. Counter resets, stale samples, malformed replies, and missing route series remain cannot-observe rather than zero errors.",
		action:    "Classify bounded failure categories and compare running API ancestry with the explicit-refusal status correction. Validation/terms/invalid contact should return 400 and completed account/name conflicts 409; preserve genuine or ambiguous internal failures as 500 and existing 429/Retry-After. No raw request bodies or identities are needed.",
		verify:    "After authorized API convergence, two complete post-boundary one-hour windows have zero observed 5xx with fresh non-reset route evidence. Verify accepted signup, 400/409 refusal, and unchanged 429/Retry-After controls independently; retain any remaining genuine internal failure.",
		playbook:  "SIGNALS.md §2.28",
	}
}

func signupTotalFinding(durable signupDurableSnapshot, metrics map[string]float64) finding {
	// Five-minute database refreshes and non-atomic publication can differ
	// legitimately. Use fifteen minutes of gross churn plus ten rows as the
	// explicit diagnostic tolerance, never a relative percentage of a million.
	allowance := float64(durable.churn) + 10
	if float64(durable.total)-allowance <= metrics["minimum"] && metrics["maximum"] <= float64(durable.total)+allowance {
		return healthyFinding("observability/signup-liveness", tierWarn, "signup-total-drift", "network-total")
	}
	return finding{
		probeId: "observability/signup-liveness", tier: tierWarn, class: "signup-total-drift", target: "network-total", sustain: 2,
		symptom:   "A fresh published network total differs from the primary inventory beyond the refresh/churn allowance",
		mechanism: "The network table is the current inventory; a freshly pushed stats gauge can still contain an older collector value. A publisher outside the explicit allowance indicates published-total inconsistency, not stalled signup or a particular retention, cache, or database cause.",
		baseline:  "Each observed publisher lies within primary total plus or minus fifteen-minute gross creation/deletion churn plus ten rows; source samples are at most 90 seconds old.",
		observed:  fmt.Sprintf("primary_total=%d observed_publishers=%.0f publisher_min=%.0f publisher_max=%.0f gross_churn_15m=%d allowance=%.0f", durable.total, metrics["publishers"], metrics["minimum"], metrics["maximum"], durable.churn, allowance),
		evidence:  "A single bounded PostgreSQL snapshot supplies current count and recent audit churn; Mimir returns only publisher count, min/max values, and oldest sample age at the same evaluation time. No network, source-address, host, block, or instance identifiers are emitted.",
		context:   "Fresh transport timestamps do not prove fresh database collection. The allowance is an operational diagnostic tolerance, not proof that all audit writes are complete or every desired publisher is present. Missing publishers remain the separate metrics/inventory coverage boundary.",
		action:    "Compare the running Taskworker stats collector and public feed aggregation with the primary inventory, and verify its five-minute database refresh and source revision. Diagnose stalled collection, mixed generations, and audit completeness before changing counts; never repair this by deleting network rows or treating a net-total plateau as an outage.",
		verify:    "After the authorized collector/source correction, two ten-minute samples remain within the documented primary/churn band, with fresh intended publishers and independently confirmed current signup progress.",
		playbook:  "SIGNALS.md §2.28 and §11.20",
	}
}
