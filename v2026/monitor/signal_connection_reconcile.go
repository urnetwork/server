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

// Signal connection-reconcile implements SIGNALS.md §2.16a. §2.16 finds open
// connection rows whose handler is gone; it cannot see a row whose process
// still heartbeats its handler but no longer holds the connection. Each
// Connect handler's reconcile exports how many such rows its last pass found
// (urnetwork_connect_connection_reconcile_unheld) and retires them. This
// signal reads that gauge with the reconcile's retirements and pass results
// per block, and warns when unheld rows stay while nothing retires them, or
// when the reconcile fails: every pass raises, or retires fail and none
// succeeds. A failed retire fails only its row and its pass completes, so the
// pass results alone miss it. No connection, client or handler identifier
// leaves Mimir; host and block names are the bounded scrape labels.
func NewConnectionReconcileSignal() Signal {
	return &signalAdapter{
		number: "2.16a", key: "connection-reconcile", name: "Live handler connection reconcile",
		probe: connectionReconcileProbe{},
	}
}

type connectionReconcileProbe struct{}

func (connectionReconcileProbe) id() string             { return "mimir/connection-reconcile" }
func (connectionReconcileProbe) tier() string           { return tierWarn }
func (connectionReconcileProbe) cadence() time.Duration { return time.Minute }

const (
	connectionReconcileRange = "15m"
	// one full retire batch; a transient race leaves a few rows for one pass
	connectionReconcileStuckFloor = 256.0
	connectionReconcileFreshness  = 5 * time.Minute
)

// The unheld gauge and the reconcile's retirements, pass results and unretired
// rows by reason over the range, each per host and block, in one evaluation.
func connectionReconcileQuery(environment string) string {
	env := strconv.Quote(environment)
	parts := []string{
		fmt.Sprintf(`label_replace(max by (host,block) (urnetwork_connect_connection_reconcile_unheld{env=%s}),"monitor_metric","unheld","__name__",".*")`, env),
		fmt.Sprintf(`label_replace(sum by (host,block) (increase(urnetwork_connect_connection_reconcile_retired_total{env=%s}[%s])),"monitor_metric","retired","__name__",".*")`, env, connectionReconcileRange),
	}
	for _, result := range []string{"ok", "error"} {
		parts = append(parts, fmt.Sprintf(
			`label_replace(sum by (host,block) (increase(urnetwork_connect_connection_reconcile_passes_total{env=%s,result=%s}[%s])),"monitor_metric",%s,"__name__",".*")`,
			env, strconv.Quote(result), connectionReconcileRange, strconv.Quote(result+"_passes"),
		))
	}
	for _, reason := range []string{"failed", "busy"} {
		parts = append(parts, fmt.Sprintf(
			`label_replace(sum by (host,block) (increase(urnetwork_connect_connection_reconcile_unretired_total{env=%s,reason=%s}[%s])),"monitor_metric",%s,"__name__",".*")`,
			env, strconv.Quote(reason), connectionReconcileRange, strconv.Quote(reason+"_retires"),
		))
	}
	return strings.Join(parts, " or ")
}

// One block's reconcile over the range.
type connectionReconcileBlock struct {
	name        string
	unheld      float64
	hasUnheld   bool
	retired     float64
	okPasses    float64
	errorPasses float64
	// absent before the reconcile counted its unretired rows
	failedRetires float64
	busyRetires   float64
}

// Groups the evaluation by block. A sample that is malformed, stale or
// negative makes the whole frame unobservable rather than a measured zero.
func parseConnectionReconcileFrame(response mimirInstantResponse, now time.Time) ([]*connectionReconcileBlock, string) {
	blocks := map[string]*connectionReconcileBlock{}
	for _, series := range response.Data.Result {
		host, block := series.Metric["host"], series.Metric["block"]
		if host == "" || block == "" {
			return nil, "unlabeled_series"
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || now.Sub(observedAt) > connectionReconcileFreshness || now.Sub(observedAt) < -30*time.Second ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, "invalid_or_stale_metric"
		}
		name := host + "/" + block
		state, ok := blocks[name]
		if !ok {
			state = &connectionReconcileBlock{name: name}
			blocks[name] = state
		}
		switch series.Metric["monitor_metric"] {
		case "unheld":
			state.unheld, state.hasUnheld = value, true
		case "retired":
			state.retired = value
		case "ok_passes":
			state.okPasses = value
		case "error_passes":
			state.errorPasses = value
		case "failed_retires":
			state.failedRetires = value
		case "busy_retires":
			state.busyRetires = value
		default:
			return nil, "unknown_metric_class"
		}
	}
	ordered := []*connectionReconcileBlock{}
	for _, state := range blocks {
		if state.hasUnheld {
			ordered = append(ordered, state)
		}
	}
	if len(ordered) == 0 {
		return nil, "no_reconcile_gauge"
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].name < ordered[j].name
	})
	return ordered, ""
}

// The blocks' values, at most eight, largest unheld first.
func connectionReconcileObserved(blocks []*connectionReconcileBlock) string {
	ordered := append([]*connectionReconcileBlock(nil), blocks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].unheld > ordered[j].unheld
	})
	parts := []string{}
	for i, state := range ordered {
		if i == 8 {
			parts = append(parts, fmt.Sprintf("and %d more", len(ordered)-8))
			break
		}
		parts = append(parts, fmt.Sprintf("%s unheld=%.0f retired_%s=%.0f ok_passes=%.0f error_passes=%.0f failed_retires=%.0f busy_retires=%.0f",
			state.name, state.unheld, connectionReconcileRange, state.retired, state.okPasses, state.errorPasses, state.failedRetires, state.busyRetires))
	}
	return strings.Join(parts, "; ")
}

func (connectionReconcileProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("connection reconcile: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(connectionReconcileQuery(env.cfg.env))
	output, gateway, err := shellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 4194304 '"+queryURL+"'")
	if err != nil {
		return nil, fmt.Errorf("connection reconcile: query Mimir: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("connection reconcile: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("connection reconcile: Mimir response was not a successful vector")
	}

	target := env.cfg.env + "/connect-fleet"
	blocks, visibilityReason := parseConnectionReconcileFrame(response, env.now().UTC())
	if visibilityReason != "" {
		return []finding{{
			probeId: "mimir/connection-reconcile", tier: tierWarn, class: "connection-reconcile-unobservable",
			target: target, sustain: 2,
			symptom:   "No Connect block exports a fresh connection reconcile gauge.",
			mechanism: "Without the gauge, a connection row whose process still heartbeats its handler but no longer holds the connection is invisible: §2.16 sees only rows whose handler is gone.",
			baseline:  "Every Connect block exports urnetwork_connect_connection_reconcile_unheld with fresh samples.",
			observed:  "visibility_reason=" + visibilityReason + " metrics_gateway=" + gateway.name,
			evidence:  "Only a fixed structural reason is exported; raw samples are discarded.",
			context:   "This is expected until Connect runs a Server artifact with the handler connection reconcile. Before it, a failed final disconnect left its row connected for the life of the process (about 29.6k provider rows on Main on 2026-10-14).",
			action:    "Deploy Connect with the handler connection reconcile, then recheck. Until then, compare each block's connected rows with its urnetwork_connect_provider_connections gauge.",
			verify:    "Every Connect block exports the gauge for two cadences.",
			playbook:  "SIGNALS.md §2.16a and §2.16",
		}}, nil
	}

	findings := []finding{healthyFinding("mimir/connection-reconcile", tierWarn, "connection-reconcile-unobservable", target)}
	stuck := []*connectionReconcileBlock{}
	failing := []*connectionReconcileBlock{}
	for _, state := range blocks {
		if connectionReconcileStuckFloor <= state.unheld && state.retired == 0 {
			stuck = append(stuck, state)
		}
		// every pass raised, or retires failed and none succeeded
		if (0 < state.errorPasses && state.okPasses == 0) || (0 < state.failedRetires && state.retired == 0) {
			failing = append(failing, state)
		}
	}

	if 0 < len(stuck) {
		findings = append(findings, finding{
			probeId: "mimir/connection-reconcile", tier: tierWarn, class: "connection-reconcile-stuck",
			target: target, sustain: 2,
			symptom: fmt.Sprintf(
				"%d Connect block(s) hold at least %.0f connected rows their process no longer holds, and retired none in %s",
				len(stuck), connectionReconcileStuckFloor, connectionReconcileRange,
			),
			mechanism: "These rows are connected under a handler that is alive, so handler expiry never reaches them, and every reader of connected rows (the location reliability writer, FP2 and URL probe admission, location and provider counts) counts a provider that is gone. The reconcile retires at most 256 such rows per pass every 15 seconds; none retired means its retires fail, are deferred behind busy endpoint fences, or return nothing, its batch is zero, or a registration it waits for never ends.",
			baseline:  fmt.Sprintf("Unheld rows stay below %.0f, or fall as the reconcile retires them.", connectionReconcileStuckFloor),
			observed:  connectionReconcileObserved(stuck) + " metrics_gateway=" + gateway.name,
			evidence:  "Mimir evaluates the per-block unheld gauge with the reconcile's retirements over the range; no identifier leaves Mimir.",
			context:   "After a database outage a block can briefly hold thousands of unheld rows while it retires them, which is healthy while retirements continue. Rows of a process that ended belong to §2.16.",
			action:    "Check the block's Connect logs for raised reconcile passes and for a registration stuck in its database write. Do not update connection rows or delete handlers manually. If the reconcile cannot recover, a restart of that Connect block lets handler expiry retire all of its rows, at the cost of its live connections reconnecting.",
			verify:    "Unheld falls below the floor or retirements resume for two cadences, and the block's connected provider rows match its provider gauge.",
			playbook:  "SIGNALS.md §2.16a",
		})
	} else {
		findings = append(findings, healthyFinding("mimir/connection-reconcile", tierWarn, "connection-reconcile-stuck", target))
	}

	if 0 < len(failing) {
		findings = append(findings, finding{
			probeId: "mimir/connection-reconcile", tier: tierWarn, class: "connection-reconcile-failing",
			target: target, sustain: 2,
			symptom: fmt.Sprintf(
				"the connection reconcile failed in %s on %d Connect block(s): every pass raised, or retires failed and none succeeded",
				connectionReconcileRange, len(failing),
			),
			mechanism: "A pass reads the handler's connected rows and retires the unheld ones through PostgreSQL. A pass whose read raises leaves its rows for the next pass, which runs after the backlog interval; a retire that raises leaves its row for a later pass, after the rows that have not failed, and three consecutive failures end the pass. Either sustained means connection rows of ended transports accumulate.",
			baseline:  "At least one pass completes, and some retire succeeds whenever retires fail, on every block in the range.",
			observed:  connectionReconcileObserved(failing) + " metrics_gateway=" + gateway.name,
			evidence:  "Per-block pass counts by result over the range; no identifier leaves Mimir.",
			context:   "A database outage fails the passes, and the connections' own final disconnects, together. The passes recover with the database.",
			action:    "Restore the block's PostgreSQL path (§2.11 PgBouncer pool-path stalls, §2.2 wait events), then confirm the passes complete and retirements follow. Do not update connection rows manually.",
			verify:    "Passes complete on every block for two cadences.",
			playbook:  "SIGNALS.md §2.16a",
		})
	} else {
		findings = append(findings, healthyFinding("mimir/connection-reconcile", tierWarn, "connection-reconcile-failing", target))
	}
	return findings, nil
}
