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
	circleAdmissionSamplesSuffix   = ":range-samples"
	circleAdmissionMeanWaitSeconds = 5.0
	// 66525afc completes fail-closed Redis error accounting but predates the
	// capability gauge and exact pre-POST marker. 928abfca is the surviving
	// current-main ancestry boundary for that complete observation contract.
	circleAdmissionFailClosedBaselineCommit = "66525afc"
	circleAdmissionMarkerBaselineCommit     = "928abfca"
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
	circleAdmissionMetricObservable
	circleAdmissionMetricAll = circleAdmissionMetricAdmissions |
		circleAdmissionMetricDeferrals |
		circleAdmissionMetricErrors |
		circleAdmissionMetricWaitCount |
		circleAdmissionMetricWaitSum |
		circleAdmissionMetricObservable
	circleAdmissionMetricDeltaAll = circleAdmissionMetricAdmissions |
		circleAdmissionMetricDeferrals |
		circleAdmissionMetricErrors |
		circleAdmissionMetricWaitCount |
		circleAdmissionMetricWaitSum
)

const circleAdmissionObservableMetricName = "urnetwork_circle_transfer_admission_observable_info"

var circleAdmissionDeltaMetricNames = []string{
	"urnetwork_circle_transfer_admissions_total",
	"urnetwork_circle_transfer_deferrals_total",
	"urnetwork_circle_transfer_admission_errors_total",
	"urnetwork_circle_transfer_admission_wait_seconds_count",
	"urnetwork_circle_transfer_admission_wait_seconds_sum",
}

var circleAdmissionMetricNames = []string{
	"urnetwork_circle_transfer_admissions_total",
	"urnetwork_circle_transfer_deferrals_total",
	"urnetwork_circle_transfer_admission_errors_total",
	"urnetwork_circle_transfer_admission_wait_seconds_count",
	"urnetwork_circle_transfer_admission_wait_seconds_sum",
	circleAdmissionObservableMetricName,
}

type circleAdmissionMetrics struct {
	host           string
	block          string
	instance       string
	start          float64
	admissions     float64
	deferrals      float64
	errors         float64
	waitCount      float64
	waitSum        float64
	sampleMask     uint8
	deltaMask      uint8
	rangeSamples   map[string]float64
	seenMetrics    map[string]bool
	invalidMetrics map[string]string
	fresh          bool
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
		if metricName == circleAdmissionObservableMetricName {
			parts = append(parts, fmt.Sprintf(
				`label_replace((%s and on(env,host,block,instance) (timestamp(%s) >= time() - %d)),"monitor_metric",%s,"job",".*")`,
				metric,
				metric,
				int64(circleAdmissionFreshness/time.Second),
				strconv.Quote(metricName),
			))
			continue
		}
		parts = append(parts, fmt.Sprintf(
			`label_replace((increase(%s[%s]) and on(env,host,block,instance) (timestamp(%s) >= time() - %d)),"monitor_metric",%s,"job",".*")`,
			metric,
			circleAdmissionRange,
			metric,
			int64(circleAdmissionFreshness/time.Second),
			strconv.Quote(metricName),
		))
		parts = append(parts, fmt.Sprintf(
			`label_replace((count_over_time(%s[%s]) and on(env,host,block,instance) (timestamp(%s) >= time() - %d)),"monitor_metric",%s,"job",".*")`,
			metric,
			circleAdmissionRange,
			metric,
			int64(circleAdmissionFreshness/time.Second),
			strconv.Quote(metricName+circleAdmissionSamplesSuffix),
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
	nowSeconds := float64(now.Unix()) + float64(now.Nanosecond())/float64(time.Second)
	processes := map[string]*circleAdmissionMetrics{}
	sourceIssues := map[string]bool{}
	for _, series := range response.Data.Result {
		metricName := series.Metric["monitor_metric"]
		if metricName == "" {
			metricName = series.Metric["__name__"]
		}
		baseMetric := strings.TrimSuffix(metricName, circleAdmissionSamplesSuffix)
		mask := circleAdmissionMetricMask(baseMetric)
		isRange := metricName != baseMetric
		if mask == 0 || (isRange && mask&circleAdmissionMetricDeltaAll == 0) {
			sourceIssues["unexpected metric family"] = true
			continue
		}
		host := series.Metric["host"]
		block := series.Metric["block"]
		instance := series.Metric["instance"]
		if host == "" || strings.Contains(host+block+instance, "\x00") ||
			series.Metric["env"] != env.cfg.env || series.Metric["job"] != "taskworker" {
			sourceIssues["missing or mismatched runtime identity"] = true
			continue
		}
		key := host + "\x00" + block + "\x00" + instance
		process := processes[key]
		if process == nil {
			process = &circleAdmissionMetrics{
				host: host, block: block, instance: instance,
				rangeSamples:   map[string]float64{},
				seenMetrics:    map[string]bool{},
				invalidMetrics: map[string]string{},
			}
			processes[key] = process
		}
		// Extra labels do not create another owner for these scalar families.
		// Duplicate invalidation is sticky, including equal or malformed values.
		if process.seenMetrics[metricName] {
			process.invalidMetrics[metricName] = "duplicate"
			process.fresh = true
			continue
		}
		process.seenMetrics[metricName] = true
		var timestamp *float64
		if len(series.Value) != 2 || json.Unmarshal(series.Value[0], &timestamp) != nil ||
			timestamp == nil || *timestamp <= 0 || math.IsNaN(*timestamp) || math.IsInf(*timestamp, 0) ||
			strings.TrimSpace(string(series.Value[1])) == "null" {
			process.invalidMetrics[metricName] = "malformed sample"
			process.fresh = true
			continue
		}
		if nowSeconds+30 < *timestamp {
			process.invalidMetrics[metricName] = "future sample"
			process.fresh = true
			continue
		}
		if *timestamp < nowSeconds-circleAdmissionFreshness.Seconds() {
			process.invalidMetrics[metricName] = "stale sample"
			continue
		}
		process.fresh = true
		_, value, err := mimirInstantValue(series.Value)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			process.invalidMetrics[metricName] = "invalid value"
			continue
		}
		if isRange {
			if value < 1 || value != math.Trunc(value) {
				process.invalidMetrics[metricName] = "invalid sample count"
				continue
			}
			process.sampleMask |= mask
			process.rangeSamples[baseMetric] = value
			continue
		}
		switch metricName {
		case circleAdmissionObservableMetricName:
			if value != 1 {
				process.invalidMetrics[metricName] = "capability is not one"
				continue
			}
			process.sampleMask |= circleAdmissionMetricObservable
		case "process_start_time_seconds":
			if value <= 0 || nowSeconds+30 < value || block == "" || instance == "" {
				process.invalidMetrics[metricName] = "invalid process start or identity"
				continue
			}
			process.start = value
			process.sampleMask |= circleAdmissionMetricStart
		case "urnetwork_circle_transfer_admissions_total":
			process.admissions = value
			process.deltaMask |= circleAdmissionMetricAdmissions
		case "urnetwork_circle_transfer_deferrals_total":
			process.deferrals = value
			process.deltaMask |= circleAdmissionMetricDeferrals
		case "urnetwork_circle_transfer_admission_errors_total":
			process.errors = value
			process.deltaMask |= circleAdmissionMetricErrors
		case "urnetwork_circle_transfer_admission_wait_seconds_count":
			process.waitCount = value
			process.deltaMask |= circleAdmissionMetricWaitCount
		case "urnetwork_circle_transfer_admission_wait_seconds_sum":
			process.waitSum = value
			process.deltaMask |= circleAdmissionMetricWaitSum
		}
	}
	for _, process := range processes {
		for metricName := range process.invalidMetrics {
			baseMetric := strings.TrimSuffix(metricName, circleAdmissionSamplesSuffix)
			mask := circleAdmissionMetricMask(baseMetric)
			if metricName != baseMetric || mask&circleAdmissionMetricDeltaAll == 0 {
				process.sampleMask &^= mask
			} else {
				process.deltaMask &^= mask
			}
		}
		for _, metricName := range circleAdmissionDeltaMetricNames {
			mask := circleAdmissionMetricMask(metricName)
			if process.sampleMask&mask == 0 || process.rangeSamples[metricName] < 2 {
				process.deltaMask &^= mask
			}
		}
		waitMask := uint8(circleAdmissionMetricWaitCount | circleAdmissionMetricWaitSum)
		if process.deltaMask&waitMask == waitMask &&
			((process.waitCount == 0 && process.waitSum != 0) ||
				(process.waitCount > 0 && math.IsInf(process.waitSum/process.waitCount, 0))) {
			process.invalidMetrics["urnetwork_circle_transfer_admission_wait_seconds_sum"] = "invalid wait denominator or ratio"
			process.deltaMask &^= circleAdmissionMetricWaitSum
		}
	}

	current, generationGaps := newestCircleAdmissionProcesses(processes)
	for issue := range sourceIssues {
		generationGaps = append(generationGaps, issue)
	}
	sort.Strings(generationGaps)
	if len(current) == 0 && len(generationGaps) == 0 {
		return nil, fmt.Errorf("circle admission: Mimir returned no fresh taskworker process starts")
	}

	sort.Slice(current, func(i, j int) bool {
		left, right := current[i], current[j]
		return left.host+"\x00"+left.block+"\x00"+left.instance < right.host+"\x00"+right.block+"\x00"+right.instance
	})

	noSamples := []string{}
	rangeGaps := []string{}
	invalidSamples := append([]string(nil), generationGaps...)
	rows := []string{}
	var totalAdmissions float64
	var totalDeferrals float64
	var totalErrors float64
	var totalWaitCount float64
	var totalWaitSum float64
	var maxMeanWait float64
	var maxMeanWaitProcess string
	completeMask := uint8(circleAdmissionMetricDeltaAll)
	if len(generationGaps) > 0 {
		completeMask = 0
	}
	for _, process := range current {
		if process.sampleMask&circleAdmissionMetricAll != circleAdmissionMetricAll {
			noSamples = append(noSamples, fmt.Sprintf(
				"%s[%s]",
				circleAdmissionProcessLabel(process),
				strings.Join(circleAdmissionMissingSamples(process.sampleMask), ","),
			))
		} else if process.deltaMask&circleAdmissionMetricDeltaAll != circleAdmissionMetricDeltaAll {
			rangeGaps = append(rangeGaps, fmt.Sprintf(
				"%s[%s]",
				circleAdmissionProcessLabel(process),
				strings.Join(circleAdmissionRangeGaps(process), ","),
			))
		}
		if len(process.invalidMetrics) > 0 {
			issues := []string{}
			for metricName, reason := range process.invalidMetrics {
				issues = append(issues, metricName+":"+reason)
			}
			sort.Strings(issues)
			invalidSamples = append(invalidSamples, circleAdmissionProcessLabel(process)+"["+strings.Join(issues, ",")+"]")
		}
		rows = append(rows, circleAdmissionObservation(process))
		completeMask &= process.deltaMask
		if process.deltaMask&circleAdmissionMetricAdmissions != 0 {
			totalAdmissions += process.admissions
		}
		if process.deltaMask&circleAdmissionMetricDeferrals != 0 {
			totalDeferrals += process.deferrals
		}
		if process.deltaMask&circleAdmissionMetricErrors != 0 {
			totalErrors += process.errors
		}
		waitMask := uint8(circleAdmissionMetricWaitCount | circleAdmissionMetricWaitSum)
		if process.deltaMask&waitMask == waitMask {
			totalWaitCount += process.waitCount
			totalWaitSum += process.waitSum
			if 0 < process.waitCount {
				meanWait := process.waitSum / process.waitCount
				if maxMeanWait < meanWait {
					maxMeanWait = meanWait
					maxMeanWaitProcess = circleAdmissionProcessLabel(process)
				}
			}
		} else {
			completeMask &^= waitMask
		}
	}
	for _, value := range []float64{totalAdmissions, totalDeferrals, totalErrors, totalWaitCount, totalWaitSum} {
		if math.IsInf(value, 0) {
			invalidSamples = append(invalidSamples, "aggregate overflow")
			completeMask = 0
			break
		}
	}
	// Complete families can prove a failure independently. Missing families
	// remain unknown in the output and never supply fleet totals or a recovery.
	totalObservation := fmt.Sprintf(
		"admissions_5m=%s deferrals_5m=%s admission_errors_5m=%s wait_count_5m=%s wait_sum_seconds_5m=%s metrics_gateway=%s",
		circleAdmissionNumber(totalAdmissions, completeMask&circleAdmissionMetricAdmissions != 0, 3),
		circleAdmissionNumber(totalDeferrals, completeMask&circleAdmissionMetricDeferrals != 0, 3),
		circleAdmissionNumber(totalErrors, completeMask&circleAdmissionMetricErrors != 0, 3),
		circleAdmissionNumber(totalWaitCount, completeMask&circleAdmissionMetricWaitCount != 0, 3),
		circleAdmissionNumber(totalWaitSum, completeMask&circleAdmissionMetricWaitSum != 0, 6),
		metricHost.name,
	)

	findings := []finding{}
	if len(noSamples) > 0 || len(rangeGaps) > 0 || len(invalidSamples) > 0 {
		observations := []string{}
		if len(invalidSamples) > 0 {
			observations = append(observations, "invalid_samples="+strings.Join(invalidSamples, "; "))
		}
		if len(noSamples) > 0 {
			observations = append(observations, "no_range_samples="+strings.Join(noSamples, "; "))
		}
		if len(rangeGaps) > 0 {
			observations = append(observations, "insufficient_range="+strings.Join(rangeGaps, "; "))
		}
		observableMissing := false
		for _, process := range current {
			if process.sampleMask&circleAdmissionMetricObservable == 0 {
				observableMissing = true
				break
			}
		}
		mechanism := "No accepted sample for one or more Circle metric families is queryable in the five-minute range. Mimir cannot distinguish an absent collector from stats delivery or admission loss from this observation alone."
		evidence := "Fresh count_over_time establishes accepted sample presence independently from five-minute increase availability. Only §8.12 source and immutable artifact evidence can prove whether the running process registered the collector."
		action := "Prove the affected block's source and immutable image digest with §8.12. If the artifact contains current-main marker-capable commit " + circleAdmissionMarkerBaselineCommit + ", restore Taskworker stats delivery and Mimir admission; only an artifact proven to predate that marker baseline justifies a Taskworker deployment. Commit " + circleAdmissionFailClosedBaselineCommit + " is only the fail-closed activity/error baseline and does not prove the capability gauge or exact pre-POST marker. Do not infer source from a mutable version string, bypass the gate, accelerate payout tasks, or rotate payment idempotency keys."
		if len(invalidSamples) > 0 {
			mechanism = "Circle admission observations are malformed, stale, duplicated, or lack a unique current process. A repeated exact runtime/family is ambiguous even when its values agree or extra labels differ. No ambiguous value contributes to a diagnosis or a healthy zero."
			evidence = "Each exact host/block/instance owns one process start, one capability gauge, and one increase plus integer range count for each activity family. Two accepted samples are required for each increase. Complete independent error and wait families remain actionable; an unknown process start prevents another instance in that host/block cohort from standing in for it."
			action = "Restore unique, finite, fresh observations for the affected runtime/family and prove its process generation. Resolve duplicate series and telemetry/schema loss before interpreting the missing activity. Preserve independently confirmed failures; do not bypass the gate, replay payments, or deploy Taskworker from ambiguous metrics alone."
		} else if observableMissing {
			mechanism = "At least one newest Taskworker has no accepted admission-observable capability sample. The bounded pre-POST marker can be absent because of a mixed rollout, a missing current collector, or telemetry loss; its absence is unknown and must never be rendered as zero admitted submissions."
			evidence = "The capability gauge is fixed at one by the same executable that emits one identifier-free marker immediately after Redis admission and before the processor POST. Newest-process selection prevents an old draining generation from supplying a replacement's missing capability."
			action = "Use §8.12 to distinguish a Taskworker artifact that lacks current-main marker-capable commit " + circleAdmissionMarkerBaselineCommit + " from one whose metric delivery was lost. An artifact containing only fail-closed baseline " + circleAdmissionFailClosedBaselineCommit + " can expose all five activity families while still lacking the capability gauge and exact pre-POST marker. Deploy the current observable artifact only to a block proven to predate " + circleAdmissionMarkerBaselineCommit + "; otherwise restore stats delivery. Keep payout-wallet-insufficient and processor 429 visibility active, and do not infer zero admissions, bypass the gate, accelerate tasks, or rotate payment idempotency keys."
		} else if len(rangeGaps) > 0 && len(noSamples) == 0 {
			mechanism = "The Circle collectors are registered, but at least one newest process lacks a usable five-minute increase. Fewer than two accepted samples prevent PromQL from calculating an increase; a missing increase with two or more samples is also an observation gap. A new generation can cause insufficient range briefly; on an established generation, correlated gaps across all five families point to telemetry admission or delivery loss, not missing gate code."
			action = "Restore enough Taskworker stats delivery and Mimir admission for two consecutive accepted samples on every current process, then rerun the five-minute delta. Do not deploy the Taskworker merely because increase() had insufficient range samples, and do not weaken or bypass the Circle gate."
		} else if len(rangeGaps) > 0 {
			mechanism += " Other families have one accepted sample but not the two required to calculate an increase."
			action += " Restore two consecutive accepted samples for the independently visible families before evaluating their deltas."
		}
		findings = append(findings, finding{
			probeId: "task/circle-transfer-admission", tier: tierWarn,
			class: "circle-transfer-admission-unobservable", target: "taskworker-fleet", sustain: 1,
			symptom: fmt.Sprintf(
				"Circle admission authority is incomplete for the observed taskworker fleet (%d current identities, %d generation/source gaps)",
				len(current), len(generationGaps),
			),
			mechanism: mechanism,
			baseline:  "Every newest fresh taskworker exports the admission-observable capability plus admissions, deferrals, fail-closed errors, and admission-wait count/sum; counter families have two consecutive scrapes.",
			observed:  strings.Join(observations, " "),
			evidence:  evidence,
			context:   "Circle documents a default five POST requests/second for Wallets API endpoints. Current-main server commit 14928f69 atomically admits at most three transfer submits in a Redis-time rolling second; descendant " + circleAdmissionFailClosedBaselineCommit + " converts the Redis wrapper's panic path into the measured fail-closed activity/error path. Later current-main commit " + circleAdmissionMarkerBaselineCommit + " adds the capability gauge and exact Redis-time pre-POST marker. This leaves two requests/second of headroom and preserves the existing payment idempotency key.",
			action:    action,
			verify:    "§8.12 proves source/digest identity convergence on marker-capable commit " + circleAdmissionMarkerBaselineCommit + " or a descendant and every newest Taskworker exposes the admission-observable capability and all five admission activity families; then exact pre-POST markers stay below four admitted submissions/second and no processor 429 occurs for a full 90-minute retry window.",
			playbook:  "SIGNALS.md §2.14, §1.2, §5.7, and §8.12",
		})
	}

	if 0 < totalErrors {
		symptom := fmt.Sprintf("Circle transfer admission failed closed %.0f time(s) in the last five minutes", totalErrors)
		if completeMask&circleAdmissionMetricErrors == 0 {
			symptom = "Circle transfer admission failed closed on an independently observed current process; the fleet error total is unknown"
		}
		findings = append(findings, finding{
			probeId: "task/circle-transfer-admission", tier: tierWarn,
			class: "circle-transfer-admission-error", target: "taskworker-fleet", sustain: 1,
			symptom:   symptom,
			mechanism: "A Taskworker could not obtain an atomic Redis admission before the transfer POST. The gate deliberately returns an error without contacting Circle; bypassing it would turn a Redis or context failure into an ambiguous financial submit and could recreate the fleet request burst.",
			baseline:  "Five-minute admission error increase is zero on every current Taskworker.",
			observed:  totalObservation,
			evidence:  strings.Join(rows, "; "),
			context:   "The transfer's durable processor idempotency key remains stable and no HTTP POST is attempted before admission. A deploy drain can cancel a waiter; repeated errors outside a drain implicate the Redis path or task context budget.",
			action:    "Correlate the exact interval with Taskworker drain state, Redis liveness/latency, and the privacy-safe transfer-admission failure log. Repair the failed boundary while keeping the gate fail closed; do not manually replay the payout or loosen the ceiling.",
			verify:    "Admission errors remain zero for two five-minute windows, Redis has no command-path failure, all payout retries preserve their original idempotency keys, and no Circle 429 appears.",
			playbook:  "SIGNALS.md §2.14, §1.2, §3, and §5.7",
		})
	}

	fleetMeanWait := float64(0)
	waitMask := uint8(circleAdmissionMetricWaitCount | circleAdmissionMetricWaitSum)
	fleetWaitComplete := completeMask&waitMask == waitMask
	if fleetWaitComplete && 0 < totalWaitCount {
		fleetMeanWait = totalWaitSum / totalWaitCount
	}
	if circleAdmissionMeanWaitSeconds < fleetMeanWait || circleAdmissionMeanWaitSeconds < maxMeanWait {
		symptom := fmt.Sprintf(
			"Circle transfer admission waited %.3fs on average fleet-wide and up to %.3fs on %s over five minutes",
			fleetMeanWait, maxMeanWait, maxMeanWaitProcess,
		)
		if !fleetWaitComplete {
			symptom = fmt.Sprintf("Circle transfer admission waited up to %.3fs on %s over five minutes; the fleet mean is unknown", maxMeanWait, maxMeanWaitProcess)
		}
		findings = append(findings, finding{
			probeId: "task/circle-transfer-admission", tier: tierWarn,
			class: "circle-transfer-admission-pressure", target: "taskworker-fleet", sustain: 2,
			symptom:   symptom,
			mechanism: "The shared gate is preventing a provider burst, but submit demand is repeatedly filling the three-per-rolling-second safety envelope. Long admission waits consume AdvancePayment's two-minute execution budget and can turn a payment backlog or unfunded-wallet retry wave into task timeouts even though Circle itself is protected.",
			baseline:  "Fleet and per-process mean completed admission wait stay at or below five seconds over two consecutive five-minute observations, admission errors remain zero, and ordinary deferrals may be non-zero.",
			observed: fmt.Sprintf(
				"%s fleet_mean_wait_seconds_5m=%s max_process_mean_wait_seconds_5m=%.6f max_process=%s",
				totalObservation, circleAdmissionNumber(fleetMeanWait, fleetWaitComplete, 6), maxMeanWait, maxMeanWaitProcess,
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

// A missing or ambiguous start cannot be replaced by another instance in its
// host/block cohort. Strictly older, uniquely started instances can drain safely.
func newestCircleAdmissionProcesses(processes map[string]*circleAdmissionMetrics) ([]*circleAdmissionMetrics, []string) {
	newest := map[string]*circleAdmissionMetrics{}
	unknown := map[string]bool{}
	unknownHosts := map[string]bool{}
	for _, process := range processes {
		if !process.fresh {
			continue
		}
		if process.block == "" {
			unknownHosts[process.host] = true
			continue
		}
		key := process.host + "\x00" + process.block
		if process.sampleMask&circleAdmissionMetricStart == 0 {
			unknown[key] = true
			continue
		}
		current := newest[key]
		if current == nil || current.start < process.start ||
			(current.start == process.start && current.instance < process.instance) {
			newest[key] = process
		}
	}
	for _, process := range processes {
		key := process.host + "\x00" + process.block
		current := newest[key]
		if process.fresh && current != nil && process != current &&
			process.sampleMask&circleAdmissionMetricStart != 0 && process.start == current.start {
			unknown[key] = true
		}
	}
	current := make([]*circleAdmissionMetrics, 0, len(newest))
	for key, process := range newest {
		if !unknown[key] && !unknownHosts[process.host] {
			current = append(current, process)
		}
	}
	gaps := []string{}
	for key := range unknown {
		gaps = append(gaps, strings.ReplaceAll(key, "\x00", "/")+"[missing, invalid, duplicate, or tied process start]")
	}
	for host := range unknownHosts {
		gaps = append(gaps, host+"[missing block identity]")
	}
	return current, gaps
}

func circleAdmissionMetricMask(metricName string) uint8 {
	switch metricName {
	case "process_start_time_seconds":
		return circleAdmissionMetricStart
	case "urnetwork_circle_transfer_admissions_total":
		return circleAdmissionMetricAdmissions
	case "urnetwork_circle_transfer_deferrals_total":
		return circleAdmissionMetricDeferrals
	case "urnetwork_circle_transfer_admission_errors_total":
		return circleAdmissionMetricErrors
	case "urnetwork_circle_transfer_admission_wait_seconds_count":
		return circleAdmissionMetricWaitCount
	case "urnetwork_circle_transfer_admission_wait_seconds_sum":
		return circleAdmissionMetricWaitSum
	case circleAdmissionObservableMetricName:
		return circleAdmissionMetricObservable
	default:
		return 0
	}
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

func circleAdmissionMissingSamples(mask uint8) []string {
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
		{circleAdmissionMetricObservable, "admission-observable"},
	} {
		if mask&metric.mask == 0 {
			missing = append(missing, metric.name)
		}
	}
	return missing
}

func circleAdmissionRangeGaps(process *circleAdmissionMetrics) []string {
	gaps := []string{}
	for _, metric := range []struct {
		mask uint8
		name string
		key  string
	}{
		{circleAdmissionMetricAdmissions, "admissions", "urnetwork_circle_transfer_admissions_total"},
		{circleAdmissionMetricDeferrals, "deferrals", "urnetwork_circle_transfer_deferrals_total"},
		{circleAdmissionMetricErrors, "admission-errors", "urnetwork_circle_transfer_admission_errors_total"},
		{circleAdmissionMetricWaitCount, "wait-count", "urnetwork_circle_transfer_admission_wait_seconds_count"},
		{circleAdmissionMetricWaitSum, "wait-sum", "urnetwork_circle_transfer_admission_wait_seconds_sum"},
	} {
		if process.deltaMask&metric.mask == 0 {
			gaps = append(gaps, fmt.Sprintf("%s=%.0f", metric.name, process.rangeSamples[metric.key]))
		}
	}
	return gaps
}

// Unknown inputs retain an explicit marker instead of a numeric zero.
func circleAdmissionNumber(value float64, known bool, precision int) string {
	if !known || math.IsNaN(value) || math.IsInf(value, 0) {
		return "unknown"
	}
	return strconv.FormatFloat(value, 'f', precision, 64)
}

// Report only independently complete delta families for the selected process.
func circleAdmissionObservation(process *circleAdmissionMetrics) string {
	meanWait := float64(0)
	waitMask := uint8(circleAdmissionMetricWaitCount | circleAdmissionMetricWaitSum)
	waitKnown := process.deltaMask&waitMask == waitMask
	if waitKnown && 0 < process.waitCount {
		meanWait = process.waitSum / process.waitCount
	}
	return fmt.Sprintf(
		"%s admissions_5m=%s deferrals_5m=%s admission_errors_5m=%s wait_count_5m=%s wait_sum_seconds_5m=%s mean_wait_seconds_5m=%s",
		circleAdmissionProcessLabel(process),
		circleAdmissionNumber(process.admissions, process.deltaMask&circleAdmissionMetricAdmissions != 0, 3),
		circleAdmissionNumber(process.deferrals, process.deltaMask&circleAdmissionMetricDeferrals != 0, 3),
		circleAdmissionNumber(process.errors, process.deltaMask&circleAdmissionMetricErrors != 0, 3),
		circleAdmissionNumber(process.waitCount, waitKnown, 3),
		circleAdmissionNumber(process.waitSum, waitKnown, 6),
		circleAdmissionNumber(meanWait, waitKnown, 6),
	)
}
