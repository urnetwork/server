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
	circleAdmissionFreshness       = 90 * time.Second
	circleAdmissionRange           = "5m"
	circleAdmissionMeanWaitSeconds = 5.0
)

// Signal circle-admission implements SIGNALS.md §2.14. It verifies that every
// newest taskworker exposes the fleet-wide Circle transfer admission gate,
// that the gate fails closed without errors, and that admission wait does not
// become a hidden task-deadline queue.
func NewCircleAdmissionSignal() Signal {
	return &signalAdapter{
		number: "2.14", key: "circle-admission", name: "Circle transfer admission",
		probe: circleAdmissionProbe{},
	}
}

type circleAdmissionProbe struct{}

func (circleAdmissionProbe) id() string             { return "task/circle-transfer-admission" }
func (circleAdmissionProbe) tier() string           { return tierWarn }
func (circleAdmissionProbe) cadence() time.Duration { return time.Minute }

const (
	circleAdmissionMetricStart uint8 = 1 << iota
	circleAdmissionMetricAdmissions
	circleAdmissionMetricDeferrals
	circleAdmissionMetricErrors
	circleAdmissionMetricWaitCount
	circleAdmissionMetricWaitSum
	circleAdmissionMetricAll = circleAdmissionMetricAdmissions |
		circleAdmissionMetricDeferrals |
		circleAdmissionMetricErrors |
		circleAdmissionMetricWaitCount |
		circleAdmissionMetricWaitSum
)

var circleAdmissionMetricNames = []string{
	"urnetwork_circle_transfer_admissions_total",
	"urnetwork_circle_transfer_deferrals_total",
	"urnetwork_circle_transfer_admission_errors_total",
	"urnetwork_circle_transfer_admission_wait_seconds_count",
	"urnetwork_circle_transfer_admission_wait_seconds_sum",
}

type circleAdmissionMetrics struct {
	host          string
	block         string
	instance      string
	start         float64
	admissions    float64
	deferrals     float64
	errors        float64
	waitCount     float64
	waitSum       float64
	availableMask uint8
}

func circleAdmissionQuery(environment string) string {
	selector := fmt.Sprintf(`{env=%s,job="taskworker"}`, strconv.Quote(environment))
	start := fmt.Sprintf(
		`label_replace(process_start_time_seconds%s,"monitor_metric","process_start_time_seconds","job",".*")`,
		selector,
	)
	parts := []string{fmt.Sprintf(
		`(%s and on(monitor_metric,env,host,block,instance) (timestamp(%s) >= time() - %d))`,
		start,
		start,
		int64(circleAdmissionFreshness/time.Second),
	)}
	for _, metricName := range circleAdmissionMetricNames {
		metric := fmt.Sprintf(`%s%s`, metricName, selector)
		parts = append(parts, fmt.Sprintf(
			`label_replace((increase(%s[%s]) and on(env,host,block,instance) (timestamp(%s) >= time() - %d)),"monitor_metric",%s,"job",".*")`,
			metric,
			circleAdmissionRange,
			metric,
			int64(circleAdmissionFreshness/time.Second),
			strconv.Quote(metricName),
		))
	}
	return strings.Join(parts, " or ")
}

func (circleAdmissionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("circle admission: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(circleAdmissionQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("circle admission: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("circle admission: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"circle admission: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	now := env.now().UTC()
	processes := map[string]*circleAdmissionMetrics{}
	for _, series := range response.Data.Result {
		metricName := series.Metric["monitor_metric"]
		if metricName == "" {
			metricName = series.Metric["__name__"]
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("circle admission: parse %s sample: %w", metricName, err)
		}
		age := now.Sub(observedAt)
		if age > circleAdmissionFreshness || age < -30*time.Second {
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("circle admission: invalid %s value %v", metricName, value)
		}
		host := series.Metric["host"]
		if host == "" {
			continue
		}
		block := series.Metric["block"]
		instance := series.Metric["instance"]
		key := host + "\x00" + block + "\x00" + instance
		process := processes[key]
		if process == nil {
			process = &circleAdmissionMetrics{host: host, block: block, instance: instance}
			processes[key] = process
		}
		switch metricName {
		case "process_start_time_seconds":
			process.start = value
			process.availableMask |= circleAdmissionMetricStart
		case "urnetwork_circle_transfer_admissions_total":
			process.admissions = value
			process.availableMask |= circleAdmissionMetricAdmissions
		case "urnetwork_circle_transfer_deferrals_total":
			process.deferrals = value
			process.availableMask |= circleAdmissionMetricDeferrals
		case "urnetwork_circle_transfer_admission_errors_total":
			process.errors = value
			process.availableMask |= circleAdmissionMetricErrors
		case "urnetwork_circle_transfer_admission_wait_seconds_count":
			process.waitCount = value
			process.availableMask |= circleAdmissionMetricWaitCount
		case "urnetwork_circle_transfer_admission_wait_seconds_sum":
			process.waitSum = value
			process.availableMask |= circleAdmissionMetricWaitSum
		}
	}

	current := newestCircleAdmissionProcesses(processes)
	if len(current) == 0 {
		return nil, fmt.Errorf("circle admission: Mimir returned no fresh taskworker process starts")
	}
	sort.Slice(current, func(i, j int) bool {
		return circleAdmissionProcessLabel(current[i]) < circleAdmissionProcessLabel(current[j])
	})

	missing := []string{}
	rows := []string{}
	var totalAdmissions float64
	var totalDeferrals float64
	var totalErrors float64
	var totalWaitCount float64
	var totalWaitSum float64
	var maxMeanWait float64
	var maxMeanWaitProcess string
	for _, process := range current {
		if process.availableMask&circleAdmissionMetricAll != circleAdmissionMetricAll {
			missing = append(missing, fmt.Sprintf(
				"%s[%s]",
				circleAdmissionProcessLabel(process),
				strings.Join(circleAdmissionMissingMetrics(process.availableMask), ","),
			))
			continue
		}
		row := circleAdmissionObservation(process)
		rows = append(rows, row)
		totalAdmissions += process.admissions
		totalDeferrals += process.deferrals
		totalErrors += process.errors
		totalWaitCount += process.waitCount
		totalWaitSum += process.waitSum
		if 0 < process.waitCount {
			meanWait := process.waitSum / process.waitCount
			if maxMeanWait < meanWait {
				maxMeanWait = meanWait
				maxMeanWaitProcess = circleAdmissionProcessLabel(process)
			}
		}
	}

	findings := []finding{}
	if len(missing) > 0 {
		findings = append(findings, finding{
			probeId: "task/circle-transfer-admission", tier: tierWarn,
			class: "circle-transfer-admission-unobservable", target: "taskworker-fleet", sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh taskworker identities do not expose the complete Circle transfer admission metric set",
				len(missing), len(current),
			),
			mechanism: "The process predates the fleet-wide transfer gate or its collector is incomplete. Retry jitter changes the average schedule but cannot impose a hard cross-process request ceiling, so an unobservable block can still race the visible blocks and receive a processor 429.",
			baseline:  "Every newest fresh taskworker exports admissions, deferrals, fail-closed errors, and admission-wait count/sum for two consecutive scrapes.",
			observed:  strings.Join(missing, "; "),
			evidence:  "Newest process identity is selected by fresh process_start_time_seconds for each host/block; draining generations cannot satisfy a replacement's collector contract.",
			context:   "Circle documents a default five POST requests/second for Wallets API endpoints. Server commit eb7e79b6 atomically admits at most three transfer submits in a Redis-time rolling second; current-main descendant 66525afc also converts the Redis wrapper's panic path into the measured fail-closed error. This leaves two requests/second of headroom and preserves the existing payment idempotency key.",
			action:    "Deploy a clean Taskworker artifact containing current-main server commit 66525afc or a clean descendant to only the missing blocks. Do not infer source from a mutable version string, bypass the gate, accelerate payout tasks, or rotate payment idempotency keys.",
			verify:    "§8.12 proves clean source/digest convergence and every newest Taskworker exposes all five admission metric families for two scrapes; then the remaining §1.5 payout cohort stays below four canonical attempts/second and produces no processor 429 for a full 90-minute retry window.",
			playbook:  "SIGNALS.md §2.14, §1.2, §5.7, and §8.12",
		})
	}

	if 0 < totalErrors {
		findings = append(findings, finding{
			probeId: "task/circle-transfer-admission", tier: tierWarn,
			class: "circle-transfer-admission-error", target: "taskworker-fleet", sustain: 1,
			symptom:   fmt.Sprintf("Circle transfer admission failed closed %.0f time(s) in the last five minutes", totalErrors),
			mechanism: "A Taskworker could not obtain an atomic Redis admission before the transfer POST. The gate deliberately returns an error without contacting Circle; bypassing it would turn a Redis or context failure into an ambiguous financial submit and could recreate the fleet request burst.",
			baseline:  "Five-minute admission error increase is zero on every current Taskworker.",
			observed: fmt.Sprintf(
				"admissions_5m=%.3f deferrals_5m=%.3f admission_errors_5m=%.3f wait_count_5m=%.3f wait_sum_seconds_5m=%.6f metrics_gateway=%s",
				totalAdmissions, totalDeferrals, totalErrors, totalWaitCount, totalWaitSum, metricHost.name,
			),
			evidence: strings.Join(rows, "; "),
			context:  "The transfer's durable processor idempotency key remains stable and no HTTP POST is attempted before admission. A deploy drain can cancel a waiter; repeated errors outside a drain implicate the Redis path or task context budget.",
			action:   "Correlate the exact interval with Taskworker drain state, Redis liveness/latency, and the privacy-safe transfer-admission failure log. Repair the failed boundary while keeping the gate fail closed; do not manually replay the payout or loosen the ceiling.",
			verify:   "Admission errors remain zero for two five-minute windows, Redis has no command-path failure, all payout retries preserve their original idempotency keys, and no Circle 429 appears.",
			playbook: "SIGNALS.md §2.14, §1.2, §3, and §5.7",
		})
	}

	fleetMeanWait := float64(0)
	if 0 < totalWaitCount {
		fleetMeanWait = totalWaitSum / totalWaitCount
	}
	if circleAdmissionMeanWaitSeconds < fleetMeanWait || circleAdmissionMeanWaitSeconds < maxMeanWait {
		findings = append(findings, finding{
			probeId: "task/circle-transfer-admission", tier: tierWarn,
			class: "circle-transfer-admission-pressure", target: "taskworker-fleet", sustain: 2,
			symptom: fmt.Sprintf(
				"Circle transfer admission waited %.3fs on average fleet-wide and up to %.3fs on %s over five minutes",
				fleetMeanWait, maxMeanWait, maxMeanWaitProcess,
			),
			mechanism: "The shared gate is preventing a provider burst, but submit demand is repeatedly filling the three-per-rolling-second safety envelope. Long admission waits consume AdvancePayment's two-minute execution budget and can turn a payment backlog or unfunded-wallet retry wave into task timeouts even though Circle itself is protected.",
			baseline:  "Fleet and per-process mean completed admission wait stay at or below five seconds over two consecutive five-minute observations, admission errors remain zero, and ordinary deferrals may be non-zero.",
			observed: fmt.Sprintf(
				"admissions_5m=%.3f deferrals_5m=%.3f admission_errors_5m=%.3f fleet_mean_wait_seconds_5m=%.6f max_process_mean_wait_seconds_5m=%.6f max_process=%s metrics_gateway=%s",
				totalAdmissions, totalDeferrals, totalErrors, fleetMeanWait, maxMeanWait, maxMeanWaitProcess, metricHost.name,
			),
			evidence: strings.Join(rows, "; "),
			context:  "This is backpressure visibility, not permission to weaken the financial-submit guard. If payout-wallet-insufficient is also active, finance/ops owns funding or pausing the wallet; software cannot create liquidity. Legitimate sustained submit growth may require an authoritative Circle quota change before code is retuned.",
			action:   "First remove the demand source: fund or pause an insufficient payout wallet, or repair an unintended scheduler/retry amplification. If the volume is legitimate, obtain the account's authoritative Circle quota and change the shared ceiling only with a deterministic fleet test and matching monitor threshold. Never accelerate or manually replay rows.",
			verify:   "For two consecutive five-minute observations, fleet and per-process mean wait are at most five seconds, admission errors and processor 429s are zero, and AdvancePayment attempts finish inside their two-minute task budget with stable idempotency keys.",
			playbook: "SIGNALS.md §2.14, §1.2, and §5.7",
		})
	}

	if len(findings) == 0 {
		return []finding{healthyFinding("task/circle-transfer-admission", tierWarn, "circle-transfer-admission", "taskworker-fleet")}, nil
	}
	return findings, nil
}

func newestCircleAdmissionProcesses(processes map[string]*circleAdmissionMetrics) []*circleAdmissionMetrics {
	newest := map[string]*circleAdmissionMetrics{}
	for _, process := range processes {
		if process.availableMask&circleAdmissionMetricStart == 0 {
			continue
		}
		key := process.host + "\x00" + process.block
		current := newest[key]
		if current == nil || current.start < process.start ||
			(current.start == process.start && current.instance < process.instance) {
			newest[key] = process
		}
	}
	current := make([]*circleAdmissionMetrics, 0, len(newest))
	for _, process := range newest {
		current = append(current, process)
	}
	return current
}

func circleAdmissionProcessLabel(process *circleAdmissionMetrics) string {
	label := process.host
	if process.block != "" {
		label += "/" + process.block
	}
	if process.instance != "" {
		label += "#" + process.instance
	}
	return label
}

func circleAdmissionMissingMetrics(mask uint8) []string {
	missing := []string{}
	for _, metric := range []struct {
		mask uint8
		name string
	}{
		{circleAdmissionMetricAdmissions, "admissions"},
		{circleAdmissionMetricDeferrals, "deferrals"},
		{circleAdmissionMetricErrors, "admission-errors"},
		{circleAdmissionMetricWaitCount, "wait-count"},
		{circleAdmissionMetricWaitSum, "wait-sum"},
	} {
		if mask&metric.mask == 0 {
			missing = append(missing, metric.name)
		}
	}
	return missing
}

func circleAdmissionObservation(process *circleAdmissionMetrics) string {
	meanWait := float64(0)
	if 0 < process.waitCount {
		meanWait = process.waitSum / process.waitCount
	}
	return fmt.Sprintf(
		"%s admissions_5m=%.3f deferrals_5m=%.3f admission_errors_5m=%.3f wait_count_5m=%.3f wait_sum_seconds_5m=%.6f mean_wait_seconds_5m=%.6f",
		circleAdmissionProcessLabel(process),
		process.admissions,
		process.deferrals,
		process.errors,
		process.waitCount,
		process.waitSum,
		meanWait,
	)
}
