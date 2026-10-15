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

// Signal origin-wait implements SIGNALS.md §2.17a. It distinguishes repeated
// authoritative database lookups from failed advisory Redis notifications.
func NewOriginWaitSignal() Signal {
	return &signalAdapter{
		number: "2.17a", key: "origin-wait", name: "Companion-origin wait amplification",
		probe: originWaitProbe{},
	}
}

type originWaitProbe struct{}

func (originWaitProbe) id() string             { return "mimir/origin-wait" }
func (originWaitProbe) tier() string           { return tierWarn }
func (originWaitProbe) cadence() time.Duration { return time.Minute }

const originWaitFreshness = 90 * time.Second

var originWaitSources = []string{"initial", "event", "fallback", "deadline"}
var originWakeSources = []string{"event", "fallback", "deadline", "cancel"}
var originNotificationEvents = []string{
	"enqueued", "published", "publish_failed", "queue_full", "owner_closed", "unowned",
	"subscribed", "reconnected", "subscription_failed", "received", "invalid_message", "registration_declined",
}

func originWaitQuery(environment string) string {
	env := strconv.Quote(environment)
	parts := []string{}
	for _, metric := range []struct{ name, class, dimension string }{
		{"urnetwork_connect_companion_origin_lookups_total", "lookup", "source"},
		{"urnetwork_connect_companion_origin_wait_wakes_total", "wake", "source"},
		{"urnetwork_contract_origin_notifications_total", "notification", "event"},
		{"urnetwork_connect_companion_origin_lookups_per_request_count", "request", ""},
	} {
		selector := fmt.Sprintf(`%s{env=%s}`, metric.name, env)
		fresh := fmt.Sprintf(`(rate(%s[5m]) and (timestamp(%s) >= time() - 90))`, selector, selector)
		if metric.dimension == "" {
			parts = append(parts, fmt.Sprintf(`label_replace(sum(%s),"monitor_metric",%s,"__name__",".*")`, fresh, strconv.Quote(metric.class)))
		} else {
			parts = append(parts, fmt.Sprintf(`label_replace(sum by (%s) (%s),"monitor_metric",%s,"__name__",".*")`, metric.dimension, fresh, strconv.Quote(metric.class)))
		}
	}
	return strings.Join(parts, " or ")
}

type originWaitFrame struct {
	lookup, wake, notification map[string]float64
	requests                   float64
}

func parseOriginWaitFrame(response mimirInstantResponse, now time.Time) (originWaitFrame, string) {
	frame := originWaitFrame{
		lookup: map[string]float64{}, wake: map[string]float64{}, notification: map[string]float64{},
	}
	seenRequests := false
	for _, series := range response.Data.Result {
		kind := series.Metric["monitor_metric"]
		var values map[string]float64
		var dimension string
		var allowed []string
		switch kind {
		case "lookup":
			values, dimension, allowed = frame.lookup, "source", originWaitSources
		case "wake":
			values, dimension, allowed = frame.wake, "source", originWakeSources
		case "notification":
			values, dimension, allowed = frame.notification, "event", originNotificationEvents
		case "request":
			if seenRequests || len(series.Metric) != 1 {
				return originWaitFrame{}, "duplicate_or_extra_request_series"
			}
			seenRequests = true
		default:
			return originWaitFrame{}, "unknown_metric_class"
		}
		if dimension != "" {
			label := series.Metric[dimension]
			if len(series.Metric) != 2 || !missingOriginValueAllowed(label, allowed...) {
				return originWaitFrame{}, "unknown_metric_label"
			}
			if _, exists := values[label]; exists {
				return originWaitFrame{}, "duplicate_metric_series"
			}
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || now.Sub(observedAt) > originWaitFreshness || now.Sub(observedAt) < -30*time.Second ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return originWaitFrame{}, "invalid_or_stale_metric"
		}
		if dimension != "" {
			values[series.Metric[dimension]] = value
		} else {
			frame.requests = value
		}
	}
	if !seenRequests || len(frame.lookup) != len(originWaitSources) ||
		len(frame.wake) != len(originWakeSources) || len(frame.notification) != len(originNotificationEvents) {
		return originWaitFrame{}, "incomplete_metric_family"
	}
	if frame.requests == 0 {
		for _, rate := range frame.lookup {
			if rate > 0 {
				return originWaitFrame{}, "lookup_without_completed_request_rate"
			}
		}
	}
	return frame, ""
}

func (originWaitProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("origin wait: no services host in inventory")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(originWaitQuery(env.cfg.env))
	output, gateway, err := shellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 4194304 '"+queryURL+"'")
	if err != nil {
		return nil, fmt.Errorf("origin wait: query Mimir: %w", err)
	}
	if len(output) > 4<<20 {
		return nil, fmt.Errorf("origin wait: oversized metric response")
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("origin wait: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("origin wait: Mimir response was not a successful vector")
	}
	frame, visibilityReason := parseOriginWaitFrame(response, env.now().UTC())
	target := env.cfg.env + "/api-fleet"
	if visibilityReason != "" {
		return []finding{{
			probeId: "mimir/origin-wait", tier: tierWarn, class: "origin-wait-unobservable",
			target: target, sustain: 2,
			symptom:   "The companion-origin wait and notification path lacks a complete fresh metric family.",
			mechanism: "A missing or mixed-version metric cannot prove zero database polling or healthy Redis event delivery.",
			baseline:  "Every fixed lookup, wake, and notification counter child plus the request histogram count is source-fresh and uniquely labeled.",
			observed:  "visibility_reason=" + visibilityReason,
			evidence:  "Only a fixed structural reason is exported; raw metric labels and samples are discarded.",
			context:   "This is expected until every API and Connect origin writer has the notification instrumentation and a complete five-minute scrape window. A partial fleet can undercount even if this family is complete; independently attest running artifact generations.",
			action:    "Converge instrumented artifacts and metrics ingestion, then recheck the complete fixed-cardinality family without treating absence as zero.",
			verify:    "Every source-fresh family remains complete for two cadences after exact artifact convergence.",
			playbook:  "SIGNALS.md §2.17a",
		}}, nil
	}
	findings := []finding{healthyFinding("mimir/origin-wait", tierWarn, "origin-wait-unobservable", target)}
	lookupRate := 0.0
	for _, rate := range frame.lookup {
		lookupRate += rate
	}
	lookupPerRequest := 0.0
	if frame.requests > 0 {
		lookupPerRequest = lookupRate / frame.requests
	}
	fallbackPerMinute := 60 * (frame.lookup["fallback"] + frame.lookup["deadline"])
	if fallbackPerMinute >= 500 && lookupPerRequest >= 3 {
		findings = append(findings, finding{
			probeId: "mimir/origin-wait", tier: tierWarn, class: "origin-wait-db-amplification",
			target: target, sustain: 2,
			symptom:   "Missing companion origins are amplifying database lookups through timed retries.",
			mechanism: "Each authoritative lookup can query both plain and chained origin indexes; a request that waits to deadline can multiply PostgreSQL work even when no origin ever appears.",
			baseline:  "Fallback/deadline lookup rate remains below 500/min or complete requests average fewer than three authoritative lookups.",
			observed: fmt.Sprintf("fallback_plus_deadline_lookups_per_minute=%.1f lookups_per_request=%.2f request_rate_per_minute=%.1f event_lookup_rate_per_minute=%.1f metrics_gateway=%s",
				fallbackPerMinute, lookupPerRequest, 60*frame.requests, 60*frame.lookup["event"], gateway.name),
			evidence: "Source-fresh, fixed-label process counters and request histogram count; no pair or request identifiers are exported.",
			context:  "This establishes repeated work, not that the request was impossible or that Redis failure caused it. Mixed old/new artifacts can undercount, so corroborate with §2.17 ownership cohorts, PostgreSQL statement deltas, and exact artifact inventory.",
			action:   "Distinguish retryable origin races from proven terminal requests. Verify post-commit notification delivery and event-assisted wakeups, then repair the owning request path without shortening legitimate race protection or suppressing failed-provider evidence.",
			verify:   "After artifact convergence, fallback amplification and PostgreSQL companion-origin call rate fall for two five-minute windows while successful cold-start contracts and provider coverage do not regress.",
			playbook: "SIGNALS.md §2.17a and §2.17",
		})
	} else {
		findings = append(findings, healthyFinding("mimir/origin-wait", tierWarn, "origin-wait-db-amplification", target))
	}
	lossPerMinute := 60 * (frame.notification["queue_full"] + frame.notification["publish_failed"] +
		frame.notification["subscription_failed"] + frame.notification["registration_declined"] +
		frame.notification["invalid_message"])
	if lossPerMinute >= 1 {
		findings = append(findings, finding{
			probeId: "mimir/origin-wait", tier: tierWarn, class: "origin-notification-loss",
			target: target, sustain: 2,
			symptom:   "Contract-origin notifications are being dropped, rejected, or disconnected.",
			mechanism: "Event loss forces waiting requests onto bounded PostgreSQL fallback reads; it cannot change committed contract correctness but can restore the former database-load multiplier.",
			baseline:  "Queue-full, publish-failed, subscription-failed, registration-declined, and invalid-message rates remain below one event/minute combined.",
			observed: fmt.Sprintf("loss_events_per_minute=%.1f queue_full_per_minute=%.1f publish_failed_per_minute=%.1f subscription_failed_per_minute=%.1f registration_declined_per_minute=%.1f invalid_message_per_minute=%.1f metrics_gateway=%s",
				lossPerMinute, 60*frame.notification["queue_full"], 60*frame.notification["publish_failed"],
				60*frame.notification["subscription_failed"], 60*frame.notification["registration_declined"],
				60*frame.notification["invalid_message"], gateway.name),
			evidence: "Fixed event classes from source-fresh Redis notification counters; no channel payload, pair hash, or identity is exported.",
			context:  "A transient reconnect can be recovered by subscribe-ack recheck and deadline fallback; this alert is a performance and visibility boundary, not proof of lost contracts. Owner-closed during normal drain and unowned test/non-router callers are not counted as delivery loss.",
			action:   "Inspect exact Redis client health, publisher queue pressure, owner lifecycle, and mixed artifact versions. Preserve the bounded PostgreSQL fallback; do not disable the origin race wait or increase Redis connections per request.",
			verify:   "Loss rate returns below one/minute and event wakeups occur under real origin races while PostgreSQL retry amplification falls for two five-minute windows.",
			playbook: "SIGNALS.md §2.17a",
		})
	} else {
		findings = append(findings, healthyFinding("mimir/origin-wait", tierWarn, "origin-notification-loss", target))
	}
	return findings, nil
}
