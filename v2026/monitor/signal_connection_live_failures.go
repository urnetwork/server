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

// Signal connection-live-failures implements SIGNALS.md §2.16b. A live Connect
// announcement keeps its transport through a failed sync read, measurement
// write or verify egress lease write and retries it, counting each failure in
// urnetwork_connect_connection_sync_read_failures_total and
// urnetwork_connect_connection_worker_failures_total{operation}. This signal
// warns when one block's operation fails in every 2 minute window of the
// range, above a floor that one failing connection stays below. An absent
// counter is §2.16a's: the same processes export the reconcile gauge, and its
// unobservable class covers a fleet that does not export them. No
// connection, client or handler identifier leaves Mimir; host, block and
// operation are the bounded labels.
func NewConnectionLiveFailuresSignal() Signal {
	return &signalAdapter{
		number: "2.16b", key: "connection-live-failures", name: "Live connection announcement failures",
		probe: connectionLiveFailuresProbe{},
	}
}

type connectionLiveFailuresProbe struct{}

func (connectionLiveFailuresProbe) id() string             { return "mimir/connection-live-failures" }
func (connectionLiveFailuresProbe) tier() string           { return tierWarn }
func (connectionLiveFailuresProbe) cadence() time.Duration { return time.Minute }

const (
	connectionLiveFailuresRange = "15m"
	// One connection retries a failing operation at most every 2 minutes, so
	// it alone fails about 8 times in the range.
	connectionLiveFailuresFloor     = 30.0
	connectionLiveFailuresFreshness = 5 * time.Minute
)

// Per host, block and operation: the failures in the fewest-failure 2 minute
// window of the range (positive when every window failed), and in the range.
func connectionLiveFailuresQuery(environment string) string {
	env := strconv.Quote(environment)
	syncRead := fmt.Sprintf(`urnetwork_connect_connection_sync_read_failures_total{env=%s}`, env)
	worker := fmt.Sprintf(`urnetwork_connect_connection_worker_failures_total{env=%s}`, env)
	return strings.Join([]string{
		fmt.Sprintf(`label_replace(label_replace(min_over_time((sum by (host,block) (increase(%s[2m])))[%s:1m]),"operation","sync_read","__name__",".*"),"monitor_metric","window_min","__name__",".*")`, syncRead, connectionLiveFailuresRange),
		fmt.Sprintf(`label_replace(label_replace(sum by (host,block) (increase(%s[%s])),"operation","sync_read","__name__",".*"),"monitor_metric","total","__name__",".*")`, syncRead, connectionLiveFailuresRange),
		fmt.Sprintf(`label_replace(min_over_time((sum by (host,block,operation) (increase(%s[2m])))[%s:1m]),"monitor_metric","window_min","__name__",".*")`, worker, connectionLiveFailuresRange),
		fmt.Sprintf(`label_replace(sum by (host,block,operation) (increase(%s[%s])),"monitor_metric","total","__name__",".*")`, worker, connectionLiveFailuresRange),
	}, " or ")
}

// One block operation over the range.
type connectionLiveFailuresOperation struct {
	name         string
	windowMin    float64
	hasWindowMin bool
	total        float64
}

// Groups the evaluation by block operation. A sample that is malformed, stale
// or negative makes the whole frame unobservable rather than a measured zero.
func parseConnectionLiveFailuresFrame(response mimirInstantResponse, now time.Time) ([]*connectionLiveFailuresOperation, string) {
	operations := map[string]*connectionLiveFailuresOperation{}
	for _, series := range response.Data.Result {
		host, block, operation := series.Metric["host"], series.Metric["block"], series.Metric["operation"]
		if host == "" || block == "" || operation == "" {
			return nil, "unlabeled_series"
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || now.Sub(observedAt) > connectionLiveFailuresFreshness || now.Sub(observedAt) < -30*time.Second ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, "invalid_or_stale_metric"
		}
		name := host + "/" + block + " " + operation
		state, ok := operations[name]
		if !ok {
			state = &connectionLiveFailuresOperation{name: name}
			operations[name] = state
		}
		switch series.Metric["monitor_metric"] {
		case "window_min":
			state.windowMin, state.hasWindowMin = value, true
		case "total":
			state.total = value
		default:
			return nil, "unknown_metric_class"
		}
	}
	ordered := []*connectionLiveFailuresOperation{}
	for _, state := range operations {
		ordered = append(ordered, state)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].name < ordered[j].name
	})
	return ordered, ""
}

// The operations' values, at most eight, most failures first.
func connectionLiveFailuresObserved(operations []*connectionLiveFailuresOperation) string {
	ordered := append([]*connectionLiveFailuresOperation(nil), operations...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].total > ordered[j].total
	})
	parts := []string{}
	for i, state := range ordered {
		if i == 8 {
			parts = append(parts, fmt.Sprintf("and %d more", len(ordered)-8))
			break
		}
		parts = append(parts, fmt.Sprintf("%s failures_%s=%.0f window_min=%.0f",
			state.name, connectionLiveFailuresRange, state.total, state.windowMin))
	}
	return strings.Join(parts, "; ")
}

func (connectionLiveFailuresProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("connection live failures: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(connectionLiveFailuresQuery(env.cfg.env))
	output, gateway, err := shellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 4194304 '"+queryURL+"'")
	if err != nil {
		return nil, fmt.Errorf("connection live failures: query Mimir: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("connection live failures: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("connection live failures: Mimir response was not a successful vector")
	}

	target := env.cfg.env + "/connect-fleet"
	operations, visibilityReason := parseConnectionLiveFailuresFrame(response, env.now().UTC())
	if visibilityReason != "" {
		return []finding{{
			probeId: "mimir/connection-live-failures", tier: tierWarn, class: "connection-live-failures-unobservable",
			target: target, sustain: 2,
			symptom:   "The Connect live failure counters returned samples that cannot be read.",
			mechanism: "Without readable counters, a block whose sync reads or worker writes keep failing is indistinguishable from a healthy one.",
			baseline:  "Every sample is labeled by host, block and operation, fresh, and non-negative.",
			observed:  "visibility_reason=" + visibilityReason + " metrics_gateway=" + gateway.name,
			evidence:  "Only a fixed structural reason is exported; raw samples are discarded.",
			context:   "An absent counter is not this class: §2.16a's unobservable class covers a Connect fleet that does not export its reconcile metrics.",
			action:    "Check the Mimir query in SIGNALS.md §2.16b against the Connect scrape labels, then recheck.",
			verify:    "The counters return readable samples for two cadences.",
			playbook:  "SIGNALS.md §2.16b",
		}}, nil
	}

	findings := []finding{healthyFinding("mimir/connection-live-failures", tierWarn, "connection-live-failures-unobservable", target)}
	sustained := []*connectionLiveFailuresOperation{}
	for _, state := range operations {
		if state.hasWindowMin && 0 < state.windowMin && connectionLiveFailuresFloor <= state.total {
			sustained = append(sustained, state)
		}
	}
	if len(sustained) == 0 {
		return append(findings, healthyFinding("mimir/connection-live-failures", tierWarn, "connection-live-failures-sustained", target)), nil
	}
	return append(findings, finding{
		probeId: "mimir/connection-live-failures", tier: tierWarn, class: "connection-live-failures-sustained",
		target: target, sustain: 2,
		symptom: fmt.Sprintf(
			"%d Connect block operation(s) failed in every 2 minute window of the last %s, at least %.0f times each",
			len(sustained), connectionLiveFailuresRange, connectionLiveFailuresFloor,
		),
		mechanism: "A live announcement keeps its transport through a failed sync read, measurement write or verify egress lease write, and retries it with a backoff of at most 2 minutes. Failures in every window mean the block's PostgreSQL or Redis path keeps failing: its providers' mode changes and reliability stats stop (sync_read), its connections' measurements stop reaching the reliability writer (measurement_write), or its verify egress leases lapse after their ttl (verify_egress_lease). Before 2026-10-14 a failed sync read or lease write tore the transport down instead.",
		baseline:  "Live failures are isolated: no block operation fails in every window, or only one connection does.",
		observed:  connectionLiveFailuresObserved(sustained) + " metrics_gateway=" + gateway.name,
		evidence:  "Mimir evaluates the per-block failure counters by operation over the range; no identifier leaves Mimir.",
		context:   "A database outage or a PgBouncer stall fails the three operations together, and their final disconnects too (§2.16a retires those rows). One connection with a corrupted Redis key fails alone and stays below the floor.",
		action:    "Read the block's Connect log for the unconditional line \"[t]live connection failures retried with the transport kept\" (at most one a minute) for the latest error, then restore the PostgreSQL path (§2.11 PgBouncer pool-path stalls, §2.2 wait events) or Redis (§3). Do not restart the block for these failures alone: the transports are kept, and a restart reconnects every connection.",
		verify:    "No block operation fails in every window for two cadences.",
		playbook:  "SIGNALS.md §2.16b",
	}), nil
}
