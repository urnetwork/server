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

// Signal provider-connections implements SIGNALS.md §1.1a. Every Connect
// process exports urnetwork_connect_provider_connections, the providers
// connected to it, and the fleet sum is the provider supply every user's
// connection draws on. On 2026-10-19 PgBouncer starvation (§2.11a) failed
// Connect's startup readiness, warp killed and recreated its containers every
// two to three minutes, and the sum fell from 76k to 1.8k over 80 minutes
// while contract creation stayed far above §1.1's floor and its trailing-hour
// median followed the decline down. Nothing read this gauge.
//
// This pages when the best minute of the last five is below half the 24 h
// level, the 90th percentile of the sum at 5 minute steps. A percentile and
// not the maximum: the sum briefly overcounts around restarts and handoffs
// (87k for four minutes at 05:34Z on 2026-10-19, after that morning's edge
// reboot, against 76k), and a maximum would carry such a spike for a day.
// The sum stayed within about a third of its level through the week before,
// including a day at 55k, and providers reconnect within about three minutes
// of a full restart, so a planned restart stays above the line for at least
// one minute of the five. A collapse that outlasts about 21 hours lowers its
// own level and clears. A missing, stale or malformed series and too little
// history are unknown, never healthy. An environment whose 24 h level is
// below 1,000 providers is too small to judge.
func NewProviderConnectionsSignal() Signal {
	return &signalAdapter{
		number: "1.1a", key: "provider-connections", name: "Connected providers",
		probe: providerConnectionsProbe{},
	}
}

type providerConnectionsProbe struct{}

func (providerConnectionsProbe) id() string             { return "mimir/provider-connections" }
func (providerConnectionsProbe) tier() string           { return tierPage }
func (providerConnectionsProbe) cadence() time.Duration { return time.Minute }

const (
	// The collapse line, as a share of the 24 h level.
	providerConnectionsCollapseRatio = 0.5
	// Below this 24 h level the environment is too small to judge.
	providerConnectionsBaselineFloor = 1000.0
	// At least one hour of the 24 h baseline at its 5 minute step.
	providerConnectionsBaselineMinimumPoints = 12.0
	// Connect metrics are scraped every 15 s; a newest sample older than this
	// means the metric path stopped.
	providerConnectionsFreshness = 3 * time.Minute
)

// The fleet sum now, its best minute of the last five, its 24 h level and
// coverage, the newest sample's time, and the reporting process counts.
func providerConnectionsQuery(environment string) string {
	selector := fmt.Sprintf(`urnetwork_connect_provider_connections{env=%s}`, strconv.Quote(environment))
	sum := "sum(" + selector + ")"
	processes := "count(count by (host,block,instance) (" + selector + "))"
	tag := func(expression string, metric string) string {
		return fmt.Sprintf(`label_replace(%s,"monitor_metric",%s,"__name__",".*")`, expression, strconv.Quote(metric))
	}
	return strings.Join([]string{
		tag(sum, "current"),
		tag("max_over_time("+sum+"[5m:1m])", "best_5m"),
		tag("quantile_over_time(0.9,"+sum+"[24h:5m])", "p90_24h"),
		tag("count_over_time("+sum+"[24h:5m])", "points_24h"),
		tag("max(timestamp("+selector+"))", "sample_time"),
		tag(processes, "processes"),
		tag("max_over_time("+processes+"[24h:5m])", "max_processes_24h"),
	}, " or ")
}

// One evaluation of the fleet sum.
type providerConnectionsFrame struct {
	values map[string]float64
}

// Reads the tagged scalars. A malformed or duplicated sample makes the frame
// unobservable; a missing tag is reported by the caller's checks.
func parseProviderConnectionsFrame(response mimirInstantResponse, now time.Time) (*providerConnectionsFrame, string) {
	frame := &providerConnectionsFrame{values: map[string]float64{}}
	for _, series := range response.Data.Result {
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || now.Sub(observedAt) > providerConnectionsFreshness || now.Sub(observedAt) < -30*time.Second ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, "invalid_metric"
		}
		metric := series.Metric["monitor_metric"]
		switch metric {
		case "current", "best_5m", "p90_24h", "points_24h", "sample_time", "processes", "max_processes_24h":
		default:
			return nil, "unknown_metric_class"
		}
		if _, duplicate := frame.values[metric]; duplicate {
			return nil, "duplicate_metric"
		}
		frame.values[metric] = value
	}
	if _, ok := frame.values["current"]; !ok {
		return nil, "metric_missing"
	}
	for _, metric := range []string{"best_5m", "p90_24h", "points_24h", "sample_time", "processes", "max_processes_24h"} {
		if _, ok := frame.values[metric]; !ok {
			return nil, "incomplete_frame"
		}
	}
	sampledAt := time.Unix(0, int64(frame.values["sample_time"]*float64(time.Second)))
	if providerConnectionsFreshness < now.Sub(sampledAt) || now.Sub(sampledAt) < -30*time.Second {
		return nil, "stale_series"
	}
	if frame.values["points_24h"] < providerConnectionsBaselineMinimumPoints {
		return nil, "baseline_insufficient"
	}
	return frame, ""
}

func (self *providerConnectionsFrame) observed() string {
	ratio := 0.0
	if 0 < self.values["p90_24h"] {
		ratio = 100 * self.values["best_5m"] / self.values["p90_24h"]
	}
	return fmt.Sprintf(
		"connected_providers=%.0f best_5m=%.0f p90_24h=%.0f best_5m_of_p90=%.1f%% reporting_processes=%.0f max_processes_24h=%.0f",
		self.values["current"], self.values["best_5m"], self.values["p90_24h"], ratio,
		self.values["processes"], self.values["max_processes_24h"],
	)
}

func (providerConnectionsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("provider connections: no services host in inventory for the loopback Mimir query")
	}
	queryUrl := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(providerConnectionsQuery(env.cfg.env))
	output, gateway, err := shellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 20 --max-filesize 1048576 '"+queryUrl+"'")
	if err != nil {
		return nil, fmt.Errorf("provider connections: query Mimir: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("provider connections: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("provider connections: Mimir response was not a successful vector")
	}

	probeId := "mimir/provider-connections"
	target := env.cfg.env + "/connect-fleet"
	frame, visibilityReason := parseProviderConnectionsFrame(response, env.now().UTC())
	if visibilityReason != "" {
		return []finding{{
			probeId: probeId, tier: tierWarn, class: "provider-connections-unobservable",
			target: target, sustain: 2,
			symptom:   "The connected-provider gauge cannot be compared with its 24 h level.",
			mechanism: "Without a fresh fleet sum and at least an hour of history, a collapse of the provider supply is indistinguishable from a healthy fleet.",
			baseline:  "Every Connect process exports urnetwork_connect_provider_connections, the newest sample is under three minutes old, and the 24 h series has at least an hour of points.",
			observed:  "visibility_reason=" + visibilityReason + " metrics_gateway=" + gateway.name,
			evidence:  "Only a fixed structural reason is exported; raw samples are discarded.",
			context:   "A full Connect outage also leaves no series: §1.1's PostgreSQL contract-creation floor and Connect's accepted handshakes (§2.11a) tell the two apart. A metric renamed or relabeled by a Connect release looks the same.",
			action:    "Query `sum(urnetwork_connect_provider_connections)` and `up`-style Connect series in Mimir; if Connect processes are running, check Fluent Bit's Connect scrape on the edges, then recheck.",
			verify:    "The gauge returns a fresh sum with an hour of history for two cadences.",
			playbook:  "SIGNALS.md §1.1a",
		}}, nil
	}

	findings := []finding{healthyFinding(probeId, tierWarn, "provider-connections-unobservable", target)}
	level, best := frame.values["p90_24h"], frame.values["best_5m"]
	if level < providerConnectionsBaselineFloor || providerConnectionsCollapseRatio*level <= best {
		return append(findings, healthyFinding(probeId, tierPage, "provider-connections-collapse", target)), nil
	}
	return append(findings, finding{
		probeId: probeId, tier: tierPage, class: "provider-connections-collapse",
		target: target, sustain: 1,
		symptom: fmt.Sprintf(
			"Connected providers fell to %.0f; the best minute of the last five was %.0f, %.0f%% of the 24 h level %.0f (90th percentile)",
			frame.values["current"], best, 100*best/level, level,
		),
		mechanism: "Connect processes are failing to hold their providers: crashing or restart-looping processes, an unready dependency, or a broken ingress. On 2026-10-19 PgBouncer starvation (§2.11a) failed Connect's startup readiness (`error not ready: pg: timeout`), warp killed and recreated its containers every two to three minutes, and the sum fell from 76k to 1.8k while contract creation stayed above §1.1's floor.",
		baseline:  "The fleet sum stays within about a third of its 24 h level through the daily cycle and ordinary rollouts; providers reconnect within about three minutes of a full restart.",
		observed:  frame.observed() + " metrics_gateway=" + gateway.name,
		evidence:  "A Mimir instant query of the per-process gauge's fleet sum, its 5 minute best and its 24 h 90th percentile; no provider, client or address leaves Mimir.",
		context:   "A dying process leaves its last value for up to Mimir's 5 minute lookback, so the sum lags a crash by up to that long. A rollout that keeps half the providers away for five minutes is itself the outage. A planned restart of every Connect block recovers inside the window and does not page.",
		action:    "Read the Connect blocks' warp journals for `Deploy fail ... not ready` and their /status (§13.4); if readiness fails on PostgreSQL, follow §2.11a and §5.12 (hold deploys and restarts) before anything else, otherwise §5.1. Do not restart Connect while its readiness fails on a dependency: each start latches not ready and adds connections.",
		verify:    "The 5 minute best is back above half the 24 h level and keeps rising toward the prior level, and Connect accepted handshakes are near their baseline.",
		playbook:  "SIGNALS.md §1.1a and §5.1",
	}), nil
}
