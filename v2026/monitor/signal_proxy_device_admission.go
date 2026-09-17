// Correlates process-generation device admission refusals with bounded budget
// context without treating a utilization ratio as the admission threshold.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	proxyDeviceAdmissionMarker       = "monitor-signal-14.7d-proxy-device-admission"
	proxyDeviceAdmissionEndpoint     = "http://127.0.0.1:3100/prometheus/api/v1/query"
	proxyDeviceAdmissionFreshness    = 90 * time.Second
	proxyDeviceAdmissionWindow       = 10 * time.Minute
	proxyDeviceAdmissionSubqueryStep = 15 * time.Second
	proxyDeviceAdmissionResponseMax  = 4 << 20
	proxyDeviceAdmissionSeriesMax    = 1024
	proxyDeviceAdmissionClockSkew    = 30 * time.Second
	proxyDeviceAdmissionSourceSuffix = "__source_timestamp"
)

const (
	proxyDeviceAdmissionMetricRSS     = "process_resident_memory_bytes"
	proxyDeviceAdmissionMetricStart   = "process_start_time_seconds"
	proxyDeviceAdmissionMetricCounter = "urnetwork_proxy_device_admission_refused_total"
	proxyDeviceAdmissionMetricBudget  = "urnetwork_proxy_device_memory_budget_bytes"
	proxyDeviceAdmissionMetricUsed    = "urnetwork_proxy_device_memory_budget_used_bytes"

	proxyDeviceAdmissionMetricDelta       = "monitor_proxy_device_admission_refused_delta"
	proxyDeviceAdmissionMetricResets      = "monitor_proxy_device_admission_refused_resets"
	proxyDeviceAdmissionMetricSamples     = "monitor_proxy_device_admission_refused_samples"
	proxyDeviceAdmissionMetricFirstSource = "monitor_proxy_device_admission_refused_first_source"
	proxyDeviceAdmissionMetricLastSource  = "monitor_proxy_device_admission_refused_last_source"
)

var proxyDeviceAdmissionRawMetrics = []string{
	proxyDeviceAdmissionMetricRSS,
	proxyDeviceAdmissionMetricStart,
	proxyDeviceAdmissionMetricCounter,
	proxyDeviceAdmissionMetricBudget,
	proxyDeviceAdmissionMetricUsed,
}

var proxyDeviceAdmissionDerivedMetrics = []string{
	proxyDeviceAdmissionMetricDelta,
	proxyDeviceAdmissionMetricResets,
	proxyDeviceAdmissionMetricSamples,
	proxyDeviceAdmissionMetricFirstSource,
	proxyDeviceAdmissionMetricLastSource,
}

// NewProxyDeviceAdmissionSignal implements SIGNALS.md §14.7d.
func NewProxyDeviceAdmissionSignal() Signal {
	return &signalAdapter{
		number: "14.7d", key: "proxy-device-admission", name: "Proxy aggregate device admission",
		probe: proxyDeviceAdmissionProbe{},
	}
}

type proxyDeviceAdmissionProbe struct{}

func (proxyDeviceAdmissionProbe) id() string             { return "runtime/proxy-device-admission" }
func (proxyDeviceAdmissionProbe) tier() string           { return tierPage }
func (proxyDeviceAdmissionProbe) cadence() time.Duration { return time.Minute }

type proxyDeviceAdmissionMetrics struct {
	host        string
	block       string
	instance    string
	values      map[string]float64
	sourceTimes map[string]time.Time
}

type proxyDeviceAdmissionCounterAssessment struct {
	delta      float64
	mode       string
	fullWindow bool
	invalid    string
	missing    []string
}

func proxyDeviceAdmissionQuery(environment string) string {
	parts := make([]string, 0, 2*len(proxyDeviceAdmissionRawMetrics)+len(proxyDeviceAdmissionDerivedMetrics))
	raw := func(metric string) string {
		return fmt.Sprintf(`%s{env=%s,job="proxy",instance!=""}`, metric, strconv.Quote(environment))
	}
	for _, metric := range proxyDeviceAdmissionRawMetrics {
		series := raw(metric)
		fresh := fmt.Sprintf(`timestamp(%s) >= time() - %d`, series, int64(proxyDeviceAdmissionFreshness/time.Second))
		parts = append(parts,
			fmt.Sprintf(`(label_replace(%s,"monitor_metric",%s,"job",".*") and on(env,host,block,instance) (%s))`, series, strconv.Quote(metric), fresh),
			fmt.Sprintf(`(label_replace(timestamp(%s),"monitor_metric",%s,"job",".*") and on(env,host,block,instance) (%s))`, series, strconv.Quote(metric+proxyDeviceAdmissionSourceSuffix), fresh),
		)
	}
	counter := raw(proxyDeviceAdmissionMetricCounter)
	rangeCounter := counter + "[" + proxyDeviceAdmissionWindow.String() + "]"
	timestampRange := fmt.Sprintf("timestamp(%s)[%s:%s]", counter, proxyDeviceAdmissionWindow, proxyDeviceAdmissionSubqueryStep)
	derived := []struct {
		name string
		expr string
	}{
		{proxyDeviceAdmissionMetricDelta, fmt.Sprintf("max_over_time(%s) - min_over_time(%s)", rangeCounter, rangeCounter)},
		{proxyDeviceAdmissionMetricResets, "resets(" + rangeCounter + ")"},
		{proxyDeviceAdmissionMetricSamples, "count_over_time(" + rangeCounter + ")"},
		{proxyDeviceAdmissionMetricFirstSource, "min_over_time(" + timestampRange + ")"},
		{proxyDeviceAdmissionMetricLastSource, "max_over_time(" + timestampRange + ")"},
	}
	for _, metric := range derived {
		parts = append(parts, fmt.Sprintf(
			`label_replace((%s),"monitor_metric",%s,"job",".*")`,
			metric.expr,
			strconv.Quote(metric.name),
		))
	}
	return strings.Join(parts, " or ")
}

func (proxyDeviceAdmissionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	expectedHosts := []string{}
	for _, target := range env.cfg.hosts {
		if target.proxy != nil {
			expectedHosts = append(expectedHosts, target.name)
		}
	}
	sort.Strings(expectedHosts)
	if env.cfg.proxyPathExpectedHosts > 0 && len(expectedHosts) != env.cfg.proxyPathExpectedHosts {
		return []finding{proxyDeviceAdmissionVisibilityFinding(
			"active-inventory",
			fmt.Sprintf("expected_proxy_hosts=%d armed_proxy_hosts=%d", env.cfg.proxyPathExpectedHosts, len(expectedHosts)),
			"The active services.yml Proxy placement count and the monitor's armed host set differ.",
		)}, nil
	}
	if len(expectedHosts) == 0 {
		return []finding{healthyFinding("runtime/proxy-device-admission", tierPage, "proxy-device-admission-refused", "proxy-fleet")}, nil
	}
	expectedBlocks := append([]string(nil), env.cfg.logServiceBlocks["proxy"]...)
	sort.Strings(expectedBlocks)
	if len(expectedBlocks) == 0 {
		return []finding{proxyDeviceAdmissionVisibilityFinding(
			"active-inventory",
			fmt.Sprintf("expected_proxy_hosts=%d expected_proxy_blocks=0", len(expectedHosts)),
			"The active services.yml snapshot supplies no Proxy block denominator.",
		)}, nil
	}
	expectedSlots := map[string]string{}
	for _, host := range expectedHosts {
		for _, block := range expectedBlocks {
			expectedSlots[host+"\x00"+block] = host + "/" + block
		}
	}

	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("proxy device admission: no services host for the loopback Mimir query")
	}
	queryTime := env.now().UTC().Truncate(proxyDeviceAdmissionSubqueryStep)
	command := "# " + proxyDeviceAdmissionMarker + "\n" +
		"curl -fsS --max-time 20 --max-filesize " + strconv.Itoa(proxyDeviceAdmissionResponseMax) +
		" --data-urlencode " + shellSingleQuote("query="+proxyDeviceAdmissionQuery(env.cfg.env)) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(queryTime.Unix(), 10)) +
		" " + shellSingleQuote(proxyDeviceAdmissionEndpoint)
	out, metricHost, err := shellFirstServiceGateway(ctx, env.runner, metricHosts, nil, command)
	if err != nil {
		return nil, fmt.Errorf("proxy device admission: query Mimir through service gateways: %w", err)
	}
	if len(out) > proxyDeviceAdmissionResponseMax {
		return nil, fmt.Errorf("proxy device admission: Mimir response exceeded %d bytes", proxyDeviceAdmissionResponseMax)
	}
	processes, parseIssues, err := parseProxyDeviceAdmissionResponse(out, queryTime, expectedSlots)
	if err != nil {
		return nil, fmt.Errorf("proxy device admission: response from %s: %w", metricHost.name, err)
	}

	fresh := []*proxyDeviceAdmissionMetrics{}
	selectionIssues := append([]string(nil), parseIssues...)
	for _, process := range processes {
		rss, rssOK := proxyDeviceAdmissionFreshRaw(process, proxyDeviceAdmissionMetricRSS, queryTime)
		if !rssOK || !proxyDeviceAdmissionFinitePositive(rss) {
			continue
		}
		start, startOK := proxyDeviceAdmissionFreshRaw(process, proxyDeviceAdmissionMetricStart, queryTime)
		if !startOK || !proxyDeviceAdmissionFinitePositive(start) || start > float64(queryTime.Add(proxyDeviceAdmissionClockSkew).Unix()) {
			selectionIssues = append(selectionIssues, proxyDeviceAdmissionLabel(process)+"[missing-or-invalid-process-start]")
			continue
		}
		fresh = append(fresh, process)
	}

	current := map[string]*proxyDeviceAdmissionMetrics{}
	for _, process := range fresh {
		slot := process.host + "\x00" + process.block
		start := process.values[proxyDeviceAdmissionMetricStart]
		previous := current[slot]
		if previous == nil || start > previous.values[proxyDeviceAdmissionMetricStart] ||
			(start == previous.values[proxyDeviceAdmissionMetricStart] && process.instance > previous.instance) {
			current[slot] = process
		}
	}

	visibility := append([]string(nil), selectionIssues...)
	invalid := []string{}
	disabled := []string{}
	refused := []string{}
	currentKeys := map[string]bool{}
	for slot, label := range expectedSlots {
		process := current[slot]
		if process == nil {
			visibility = append(visibility, label+"[absent-current-generation]")
			continue
		}
		currentKeys[proxyDeviceAdmissionProcessKey(process)] = true
	}

	for _, process := range fresh {
		label := proxyDeviceAdmissionLabel(process)
		assessment := proxyDeviceAdmissionAssessCounter(process, queryTime)
		isCurrent := currentKeys[proxyDeviceAdmissionProcessKey(process)]
		if assessment.invalid != "" {
			invalid = append(invalid, label+"["+assessment.invalid+"]")
		}

		budget, budgetOK := proxyDeviceAdmissionFreshRaw(process, proxyDeviceAdmissionMetricBudget, queryTime)
		used, usedOK := proxyDeviceAdmissionFreshRaw(process, proxyDeviceAdmissionMetricUsed, queryTime)
		counter, counterOK := proxyDeviceAdmissionFreshRaw(process, proxyDeviceAdmissionMetricCounter, queryTime)
		if isCurrent {
			missingSet := map[string]bool{}
			for _, metric := range assessment.missing {
				missingSet[metric] = true
			}
			if !budgetOK {
				missingSet[proxyDeviceAdmissionMetricBudget] = true
			}
			if !usedOK {
				missingSet[proxyDeviceAdmissionMetricUsed] = true
			}
			if !counterOK {
				missingSet[proxyDeviceAdmissionMetricCounter] = true
			}
			missing := make([]string, 0, len(missingSet))
			for metric := range missingSet {
				missing = append(missing, metric)
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				visibility = append(visibility, label+"[missing="+strings.Join(missing, ",")+"]")
			} else if !proxyDeviceAdmissionSameSourceTime(process, proxyDeviceAdmissionMetricBudget, proxyDeviceAdmissionMetricUsed, proxyDeviceAdmissionMetricCounter) {
				visibility = append(visibility, label+"[budget-used-counter-source-skew]")
			}
			if budgetOK && usedOK {
				switch {
				case !proxyDeviceAdmissionWholeNonnegative(budget):
					invalid = append(invalid, label+"[invalid-budget]")
				case !proxyDeviceAdmissionWholeNonnegative(used):
					invalid = append(invalid, label+"[invalid-used]")
				case used > budget:
					invalid = append(invalid, label+"[used-exceeds-budget]")
				case budget == 0 && used == 0 && counterOK && counter == 0:
					disabled = append(disabled, label+"[budget_bytes=0 used_bytes=0]")
				case budget == 0:
					invalid = append(invalid, label+"[disabled-budget-has-use-or-refusals]")
				}
			}
			if assessment.delta == 0 && len(missing) == 0 && !assessment.fullWindow && assessment.invalid == "" {
				visibility = append(visibility, label+"[quiet-window-warming]")
			}
		}
		if assessment.delta > 0 {
			refused = append(refused, proxyDeviceAdmissionRefusalObservation(process, assessment, budget, budgetOK, used, usedOK))
		}
	}

	sort.Strings(visibility)
	sort.Strings(invalid)
	sort.Strings(disabled)
	sort.Strings(refused)
	findings := []finding{}
	if len(visibility) > 0 {
		findings = append(findings, proxyDeviceAdmissionVisibilityFinding(
			metricHost.name,
			fmt.Sprintf("expected_proxy_identities=%d current_proxy_identities=%d visibility_gaps=%d gaps=%s", len(expectedSlots), len(current), len(visibility), strings.Join(visibility, ";")),
			"Expected current process telemetry or its complete generation-bracketed quiet window is absent, stale, or incoherent.",
		))
	}
	if len(invalid) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-device-admission", tier: tierWarn,
			class: "proxy-device-admission-invalid", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom:   fmt.Sprintf("%d fresh Proxy process generations violate the aggregate device-admission metric contract", len(invalid)),
			mechanism: "A counter reset/decrease, non-integral count, impossible process start, or used bytes above the configured budget makes the affected generation unsafe for a capacity or recovery inference.",
			baseline:  "Every fresh process has one monotonic integral refusal counter, nonnegative integral budget/use gauges with used no greater than budget, and one stable process-start identity.",
			observed:  fmt.Sprintf("invalid_generations=%d invalid=%s", len(invalid), strings.Join(invalid, ";")),
			context:   "This does not erase a separately proved positive refusal delta, and it does not prove host memory is exhausted.",
			action:    "Preserve the exact process and source timestamps; repair exporter/accounting or label-generation drift before changing capacity. Do not restart or raise the budget to manufacture a valid sample.",
			verify:    "Every expected process exports a coherent valid metric set for one complete ten-minute window with no counter reset.",
			playbook:  "SIGNALS.md §14.7d",
		})
	}
	if len(disabled) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-device-admission", tier: tierWarn,
			class: "proxy-device-admission-disabled", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom:   fmt.Sprintf("%d current Proxy process generations have aggregate device admission disabled", len(disabled)),
			mechanism: "A zero process_device_memory_budget deliberately removes the aggregate reservation gate and restores unbounded new-device admission for that process.",
			baseline:  "Every current Main Proxy process exports a positive aggregate device budget; zero is never interpreted as spare capacity.",
			observed:  fmt.Sprintf("disabled_generations=%d disabled=%s", len(disabled), strings.Join(disabled, ";")),
			context:   "This is a desired-state safety boundary, not evidence of current pressure, failed opens, or available hardware.",
			action:    "Restore the reviewed positive Main budget through the ordinary serialized Proxy rollout. Size it only after joining §14.7 host RSS and reserve; do not choose zero to silence refusals.",
			verify:    "Every current generation reports a positive budget for a complete ten-minute window and §14.7 host reserve remains healthy.",
			playbook:  "SIGNALS.md §14.7d",
		})
	}
	if len(refused) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-device-admission", tier: tierPage,
			class: "proxy-device-admission-refused", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom:   fmt.Sprintf("%d fresh Proxy process generations refused new-device admission", len(refused)),
			mechanism: "The process-monotonic counter advances only when the exact atomic aggregate reservation cannot fit one complete DeviceLocal target. Admission stops before device construction; existing installed devices are not evicted.",
			baseline:  "No fresh Proxy process records an aggregate device-admission refusal; the configured positive budget retains at least one usable reservation slot under legitimate demand.",
			observed:  fmt.Sprintf("refusing_generations=%d refusals=%s", len(refused), strings.Join(refused, ";")),
			evidence:  "The bounded query pins one evaluation time and joins the counter window to env/host/block/instance/process-start. It retains fresh overlap generations instead of selecting only the newest process.",
			context:   "The delta counts failed attempts, not unique devices, customers, flows, or bytes; retries can amplify it. Current used/budget is context only and can fall after a release. A refusal proves one configured process ceiling, not host or fleet hardware exhaustion.",
			action:    "Preserve the exact generations and interval. Compare affected and sibling process headroom, then join §14.7 direct host RSS, MemAvailable, swap, OOM, and rollout reserve before a measured placement, budget, RAM, or host change. Do not restart away evidence, evict live devices, disable the budget, or raise it blindly.",
			verify:    "With stable complete generations, the refusal counter remains flat for one complete ten-minute window and admission has either one full reservation of usable headroom or a controlled successful fresh open; §14.7 host reserve and ordinary Proxy acceptance remain healthy.",
			playbook:  "SIGNALS.md §14.7d",
		})
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("runtime/proxy-device-admission", tierPage, "proxy-device-admission-refused", "proxy-fleet")}, nil
	}
	return findings, nil
}

func parseProxyDeviceAdmissionResponse(
	out string,
	queryTime time.Time,
	expectedSlots map[string]string,
) (map[string]*proxyDeviceAdmissionMetrics, []string, error) {
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, nil, fmt.Errorf("decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, nil, fmt.Errorf("Mimir status=%q result_type=%q error=%q", response.Status, response.Data.ResultType, response.Error)
	}
	if len(response.Data.Result) > proxyDeviceAdmissionSeriesMax {
		return nil, nil, fmt.Errorf("Mimir returned %d series, limit %d", len(response.Data.Result), proxyDeviceAdmissionSeriesMax)
	}
	allowed := map[string]bool{}
	for _, metric := range proxyDeviceAdmissionRawMetrics {
		allowed[metric] = true
		allowed[metric+proxyDeviceAdmissionSourceSuffix] = true
	}
	for _, metric := range proxyDeviceAdmissionDerivedMetrics {
		allowed[metric] = true
	}
	processes := map[string]*proxyDeviceAdmissionMetrics{}
	issues := []string{}
	for _, series := range response.Data.Result {
		metric := series.Metric["monitor_metric"]
		if !allowed[metric] {
			return nil, nil, fmt.Errorf("unexpected monitor_metric %q", metric)
		}
		host, block, instance := series.Metric["host"], series.Metric["block"], series.Metric["instance"]
		if _, expected := expectedSlots[host+"\x00"+block]; !expected {
			continue
		}
		if instance == "" {
			issues = append(issues, host+"/"+block+"[missing-instance]")
			continue
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s sample: %w", metric, err)
		}
		if observedAt.Before(queryTime.Add(-proxyDeviceAdmissionClockSkew)) || observedAt.After(queryTime.Add(proxyDeviceAdmissionClockSkew)) {
			return nil, nil, fmt.Errorf("%s evaluation timestamp outside pinned boundary", metric)
		}
		key := host + "\x00" + block + "\x00" + instance
		process := processes[key]
		if process == nil {
			process = &proxyDeviceAdmissionMetrics{host: host, block: block, instance: instance, values: map[string]float64{}, sourceTimes: map[string]time.Time{}}
			processes[key] = process
		}
		if strings.HasSuffix(metric, proxyDeviceAdmissionSourceSuffix) {
			base := strings.TrimSuffix(metric, proxyDeviceAdmissionSourceSuffix)
			if _, duplicate := process.sourceTimes[base]; duplicate {
				return nil, nil, fmt.Errorf("duplicate %s source timestamp for one process", base)
			}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, nil, fmt.Errorf("invalid %s source timestamp", base)
			}
			sourceTime := unixFloatTime(value)
			if sourceTime.After(queryTime.Add(proxyDeviceAdmissionClockSkew)) {
				return nil, nil, fmt.Errorf("future %s source timestamp", base)
			}
			process.sourceTimes[base] = sourceTime
			continue
		}
		if _, duplicate := process.values[metric]; duplicate {
			return nil, nil, fmt.Errorf("duplicate %s for one process", metric)
		}
		process.values[metric] = value
	}
	return processes, issues, nil
}

func proxyDeviceAdmissionAssessCounter(process *proxyDeviceAdmissionMetrics, queryTime time.Time) proxyDeviceAdmissionCounterAssessment {
	assessment := proxyDeviceAdmissionCounterAssessment{}
	counter, counterOK := proxyDeviceAdmissionFreshRaw(process, proxyDeviceAdmissionMetricCounter, queryTime)
	start, startOK := proxyDeviceAdmissionFreshRaw(process, proxyDeviceAdmissionMetricStart, queryTime)
	if !counterOK {
		assessment.missing = append(assessment.missing, proxyDeviceAdmissionMetricCounter)
		return assessment
	}
	if !startOK {
		assessment.missing = append(assessment.missing, proxyDeviceAdmissionMetricStart)
		return assessment
	}
	if !proxyDeviceAdmissionWholeNonnegative(counter) {
		assessment.invalid = "invalid-refusal-counter"
		return assessment
	}
	if !proxyDeviceAdmissionFinitePositive(start) {
		assessment.invalid = "invalid-process-start"
		return assessment
	}
	for _, metric := range proxyDeviceAdmissionDerivedMetrics {
		if _, ok := process.values[metric]; !ok {
			assessment.missing = append(assessment.missing, metric)
		}
	}
	windowStart := queryTime.Add(-proxyDeviceAdmissionWindow)
	young := unixFloatTime(start).After(windowStart)
	if young && counter > 0 {
		assessment.delta = counter
		assessment.mode = "since-process-start"
	}
	if len(assessment.missing) > 0 {
		return assessment
	}
	delta := process.values[proxyDeviceAdmissionMetricDelta]
	resets := process.values[proxyDeviceAdmissionMetricResets]
	samples := process.values[proxyDeviceAdmissionMetricSamples]
	firstSource := process.values[proxyDeviceAdmissionMetricFirstSource]
	lastSource := process.values[proxyDeviceAdmissionMetricLastSource]
	if !proxyDeviceAdmissionWholeNonnegative(delta) || !proxyDeviceAdmissionWholeNonnegative(resets) ||
		!proxyDeviceAdmissionWholeNonnegative(samples) || !proxyDeviceAdmissionFinitePositive(firstSource) ||
		!proxyDeviceAdmissionFinitePositive(lastSource) || firstSource > lastSource {
		assessment.invalid = "invalid-counter-window"
		return assessment
	}
	if resets > 0 {
		assessment.invalid = "counter-reset-within-process-generation"
		assessment.delta = 0
		return assessment
	}
	if delta > 0 && assessment.delta == 0 {
		assessment.delta = delta
		assessment.mode = "observed-window-delta"
	}
	first := unixFloatTime(firstSource)
	last := unixFloatTime(lastSource)
	assessment.fullWindow = !young && samples >= 2 &&
		!first.After(windowStart.Add(proxyDeviceAdmissionFreshness)) &&
		!last.Before(queryTime.Add(-proxyDeviceAdmissionFreshness))
	return assessment
}

func proxyDeviceAdmissionFreshRaw(process *proxyDeviceAdmissionMetrics, metric string, queryTime time.Time) (float64, bool) {
	value, valueOK := process.values[metric]
	sourceTime, sourceOK := process.sourceTimes[metric]
	if !valueOK || !sourceOK || sourceTime.Before(queryTime.Add(-proxyDeviceAdmissionFreshness)) || sourceTime.After(queryTime.Add(proxyDeviceAdmissionClockSkew)) {
		return 0, false
	}
	return value, true
}

func proxyDeviceAdmissionSameSourceTime(process *proxyDeviceAdmissionMetrics, metrics ...string) bool {
	if len(metrics) == 0 {
		return true
	}
	want, ok := process.sourceTimes[metrics[0]]
	if !ok {
		return false
	}
	for _, metric := range metrics[1:] {
		if got, ok := process.sourceTimes[metric]; !ok || !got.Equal(want) {
			return false
		}
	}
	return true
}

func proxyDeviceAdmissionWholeNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value == math.Trunc(value)
}

func proxyDeviceAdmissionFinitePositive(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}

func proxyDeviceAdmissionProcessKey(process *proxyDeviceAdmissionMetrics) string {
	return process.host + "\x00" + process.block + "\x00" + process.instance
}

func proxyDeviceAdmissionLabel(process *proxyDeviceAdmissionMetrics) string {
	return process.host + "/" + process.block + "#" + process.instance
}

func proxyDeviceAdmissionRefusalObservation(
	process *proxyDeviceAdmissionMetrics,
	assessment proxyDeviceAdmissionCounterAssessment,
	budget float64,
	budgetOK bool,
	used float64,
	usedOK bool,
) string {
	budgetText, usedText, remainingText, ratioText := "unknown", "unknown", "unknown", "unknown"
	if budgetOK {
		budgetText = strconv.FormatFloat(budget, 'f', 0, 64)
	}
	if usedOK {
		usedText = strconv.FormatFloat(used, 'f', 0, 64)
	}
	if budgetOK && usedOK && budget >= used {
		remainingText = strconv.FormatFloat(budget-used, 'f', 0, 64)
	}
	if budgetOK && usedOK && budget > 0 {
		ratioText = strconv.FormatFloat(used/budget, 'g', -1, 64)
	}
	return fmt.Sprintf(
		"%s:mode=%s refusal_attempt_delta=%.0f budget_bytes=%s used_bytes=%s remaining_bytes=%s used_budget_ratio=%s",
		proxyDeviceAdmissionLabel(process), assessment.mode, assessment.delta, budgetText, usedText, remainingText, ratioText,
	)
}

func proxyDeviceAdmissionVisibilityFinding(frame, observed, evidence string) finding {
	return finding{
		probeId: "runtime/proxy-device-admission", tier: tierWarn,
		class: "proxy-device-admission-unobservable", target: "proxy-fleet", frame: frame, sustain: 1,
		symptom:   "The aggregate Proxy device-admission boundary is not completely observable",
		mechanism: "An expected active host/block lacks one fresh current process identity, a required metric/source timestamp, or a complete stable quiet-window bracket. Missing admission telemetry is unknown, never zero pressure or recovery.",
		baseline:  "Every active services.yml Proxy host/block has one newest fresh process exporting process identity plus the three admission metrics, and its monotonic counter spans a stable ten-minute quiet window.",
		observed:  observed,
		evidence:  evidence,
		context:   "A visibility gap does not suppress an independently proved refusal from this or an overlapping fresh generation. §14.6 carrier admission and §14.7 host memory remain independent.",
		action:    "Restore the exact active inventory, deployed metric family, or Mimir source-time continuity. Preserve current processes; do not restart or change a budget to manufacture a green sample.",
		verify:    "Every expected current process remains generation-stable and exports one coherent complete metric set through a full ten-minute window.",
		playbook:  "SIGNALS.md §14.7d",
	}
}
