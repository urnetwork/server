package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	subtensorConvergenceWindow         = "1h"
	subtensorConvergenceMinSamples     = 200
	subtensorConvergenceFreshness      = 90 * time.Second
	subtensorConvergenceMaxETADays     = 14.0
	subtensorConvergenceQueuedBlocks   = 128.0
	subtensorConvergenceBusyFraction   = 0.80
	subtensorConvergenceFutureSkew     = 30 * time.Second
	subtensorConvergenceDefaultWarpLag = int64(4096)
)

// SIGNALS.md §17.5 maps to signal_subtensor_convergence.go and
// signal_subtensor_convergence_test.go. It measures whether a progressing
// Subtensor bootstrap is actually closing its target gap; listener, identity,
// and short-sample health remain owned by §17.1 (`subtensor`).
func NewSubtensorConvergenceSignal() Signal {
	return &signalAdapter{
		number: "17.5", key: "subtensor-convergence", name: "Subtensor catch-up convergence",
		probe: subtensorConvergenceProbe{},
	}
}

type subtensorConvergenceProbe struct{}

func (subtensorConvergenceProbe) id() string             { return "subtensor/convergence" }
func (subtensorConvergenceProbe) tier() string           { return tierWarn }
func (subtensorConvergenceProbe) cadence() time.Duration { return time.Minute }

const (
	subtensorConvergenceLag = 1 << iota
	subtensorConvergenceNetRate
	subtensorConvergenceTargetRate
	subtensorConvergenceImportRate
	subtensorConvergenceImportSeconds
	subtensorConvergenceQueue
	subtensorConvergenceSamples
	subtensorConvergenceSampleAge
	subtensorConvergenceAll = (1 << iota) - 1
)

var subtensorConvergenceMeasureNames = []struct {
	bit  int
	name string
}{
	{bit: subtensorConvergenceLag, name: "lag"},
	{bit: subtensorConvergenceNetRate, name: "net_rate"},
	{bit: subtensorConvergenceTargetRate, name: "target_rate"},
	{bit: subtensorConvergenceImportRate, name: "import_rate"},
	{bit: subtensorConvergenceImportSeconds, name: "import_seconds"},
	{bit: subtensorConvergenceQueue, name: "queued_blocks"},
	{bit: subtensorConvergenceSamples, name: "sample_count"},
	{bit: subtensorConvergenceSampleAge, name: "sample_age"},
}

type subtensorConvergenceTarget struct {
	host     string
	node     string
	job      string
	lagBand  int64
	syncMode string
}

type subtensorConvergenceMetrics struct {
	target        subtensorConvergenceTarget
	lag           float64
	netRate       float64
	targetRate    float64
	importRate    float64
	importSeconds float64
	queuedBlocks  float64
	sampleCount   float64
	sampleAge     float64
	mask          int
}

func subtensorConvergenceTargets(hosts []*host) (map[string]subtensorConvergenceTarget, []string, []string, error) {
	targets := map[string]subtensorConvergenceTarget{}
	hostNames := make([]string, 0, len(hosts))
	jobNames := []string{}
	for _, configuredHost := range hosts {
		if configuredHost.subtensor == nil || len(configuredHost.subtensor.Nodes) == 0 {
			return nil, nil, nil, fmt.Errorf("subtensor convergence: %s has no configured nodes", configuredHost.name)
		}
		hostNames = append(hostNames, configuredHost.name)
		for _, node := range configuredHost.subtensor.Nodes {
			// Snow's metrics jobs deliberately match the independently supervised
			// container names. Falling back to the semantic node name keeps the
			// reusable settings shape useful when no container identity is needed;
			// absent matching series still fail closed below.
			job := strings.TrimSpace(node.ContainerName)
			if job == "" {
				job = strings.TrimSpace(node.Name)
			}
			if job == "" {
				return nil, nil, nil, fmt.Errorf("subtensor convergence: %s has a node without a metrics identity", configuredHost.name)
			}
			lagBand := int64(128)
			if node.SyncMode == "warp" {
				lagBand = configuredHost.subtensor.WarpMaxLag
				if lagBand <= 0 {
					lagBand = subtensorConvergenceDefaultWarpLag
				}
			}
			key := configuredHost.name + "\x00" + job
			if _, exists := targets[key]; exists {
				return nil, nil, nil, fmt.Errorf("subtensor convergence: duplicate metrics identity %s/%s", configuredHost.name, job)
			}
			targets[key] = subtensorConvergenceTarget{
				host: configuredHost.name, node: node.Name, job: job,
				lagBand: lagBand, syncMode: node.SyncMode,
			}
			jobNames = append(jobNames, job)
		}
	}
	sort.Strings(hostNames)
	sort.Strings(jobNames)
	return targets, uniqueStrings(hostNames), uniqueStrings(jobNames), nil
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func exactPrometheusRegex(values []string) string {
	escaped := make([]string, len(values))
	for index, value := range values {
		escaped[index] = regexp.QuoteMeta(value)
	}
	return "^(?:" + strings.Join(escaped, "|") + ")$"
}

func subtensorConvergenceQuery(environment string, hosts, jobs []string) string {
	labels := fmt.Sprintf(
		`env=%s,host=~%s,job=~%s`,
		strconv.Quote(environment),
		strconv.Quote(exactPrometheusRegex(hosts)),
		strconv.Quote(exactPrometheusRegex(jobs)),
	)
	best := `substrate_block_height{` + labels + `,status="best"}`
	target := `substrate_block_height{` + labels + `,status="sync_target"}`
	importCount := `substrate_block_verification_and_import_time_count{` + labels + `}`
	importSum := `substrate_block_verification_and_import_time_sum{` + labels + `}`
	queue := `substrate_sync_queued_blocks{` + labels + `}`

	byHostJob := func(expression string) string {
		return `max by (host,job) (` + expression + `)`
	}
	sumRate := func(metric string) string {
		return `sum by (host,job) (rate(` + metric + `[` + subtensorConvergenceWindow + `]))`
	}
	measure := func(name, expression string) string {
		return `label_replace((` + expression + `),"monitor_measure",` + strconv.Quote(name) + `,"","")`
	}

	importRate := sumRate(importCount)
	return strings.Join([]string{
		measure("lag", `clamp_min(`+byHostJob(target)+` - `+byHostJob(best)+`,0)`),
		measure("net_rate", byHostJob(`deriv(`+best+`[`+subtensorConvergenceWindow+`])`)+` - `+byHostJob(`deriv(`+target+`[`+subtensorConvergenceWindow+`])`)),
		measure("target_rate", byHostJob(`deriv(`+target+`[`+subtensorConvergenceWindow+`])`)),
		measure("import_rate", importRate),
		measure("import_seconds", sumRate(importSum)+` / `+importRate),
		measure("queued_blocks", byHostJob(queue)),
		measure("sample_count", byHostJob(`count_over_time(`+best+`[`+subtensorConvergenceWindow+`])`)),
		measure("sample_age", `time() - `+byHostJob(`timestamp(`+best+`)`)),
	}, " or ")
}

func (subtensorConvergenceProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	subtensorHosts := env.cfg.hostsWithRole("subtensor")
	if len(subtensorHosts) == 0 {
		return nil, fmt.Errorf("subtensor convergence: no subtensor host in inventory")
	}
	targets, hostNames, jobNames, err := subtensorConvergenceTargets(subtensorHosts)
	if err != nil {
		return nil, err
	}
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("subtensor convergence: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(subtensorConvergenceQuery(env.cfg.env, hostNames, jobNames))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("subtensor convergence: query Mimir through service gateways: %w", err)
	}

	metrics, err := parseSubtensorConvergence(out, targets, env.now().UTC())
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(metrics))
	for key := range metrics {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	findings := make([]finding, 0, len(keys))
	for _, key := range keys {
		if finding, ok := evaluateSubtensorConvergence(metrics[key], metricHost.name); ok {
			findings = append(findings, finding)
		}
	}
	return findings, nil
}

func parseSubtensorConvergence(raw string, targets map[string]subtensorConvergenceTarget, now time.Time) (map[string]subtensorConvergenceMetrics, error) {
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, fmt.Errorf("subtensor convergence: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"subtensor convergence: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	metrics := map[string]subtensorConvergenceMetrics{}
	for _, series := range response.Data.Result {
		host := series.Metric["host"]
		job := series.Metric["job"]
		measure := series.Metric["monitor_measure"]
		key := host + "\x00" + job
		target, expected := targets[key]
		if !expected {
			return nil, fmt.Errorf("subtensor convergence: unexpected metrics identity %s/%s", host, job)
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("subtensor convergence: parse %s for %s/%s: %w", measure, host, job, err)
		}
		age := now.Sub(observedAt)
		if age > subtensorConvergenceFreshness || age < -subtensorConvergenceFutureSkew {
			return nil, fmt.Errorf("subtensor convergence: stale Mimir evaluation for %s/%s age=%s", host, job, age.Round(time.Second))
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("subtensor convergence: invalid %s value for %s/%s", measure, host, job)
		}

		metric := metrics[key]
		metric.target = target
		bit := 0
		switch measure {
		case "lag":
			bit, metric.lag = subtensorConvergenceLag, value
		case "net_rate":
			bit, metric.netRate = subtensorConvergenceNetRate, value
		case "target_rate":
			bit, metric.targetRate = subtensorConvergenceTargetRate, value
		case "import_rate":
			bit, metric.importRate = subtensorConvergenceImportRate, value
		case "import_seconds":
			bit, metric.importSeconds = subtensorConvergenceImportSeconds, value
		case "queued_blocks":
			bit, metric.queuedBlocks = subtensorConvergenceQueue, value
		case "sample_count":
			bit, metric.sampleCount = subtensorConvergenceSamples, value
		case "sample_age":
			bit, metric.sampleAge = subtensorConvergenceSampleAge, value
		default:
			return nil, fmt.Errorf("subtensor convergence: unknown measure %q", measure)
		}
		if metric.mask&bit != 0 {
			return nil, fmt.Errorf("subtensor convergence: duplicate %s for %s/%s", measure, host, job)
		}
		metric.mask |= bit
		metrics[key] = metric
	}

	for _, key := range sortedSubtensorConvergenceTargetKeys(targets) {
		target := targets[key]
		metric, ok := metrics[key]
		if !ok || metric.mask != subtensorConvergenceAll {
			mask := 0
			if ok {
				mask = metric.mask
			}
			return nil, fmt.Errorf(
				"subtensor convergence: incomplete one-hour measures for %s/%s missing=%s mask=%d want=%d",
				target.host, target.job, strings.Join(missingSubtensorConvergenceMeasures(mask), ","), mask, subtensorConvergenceAll,
			)
		}
		// Validate the observation window before interpreting its derivatives.
		// A stale or short series can produce a physically impossible slope when
		// the range crosses a scrape/restart boundary; that is observation loss,
		// not evidence that the chain target moved backwards.
		if metric.sampleAge < -subtensorConvergenceFutureSkew.Seconds() {
			return nil, fmt.Errorf(
				"subtensor convergence: %s/%s source sample is %.0fs in the future, want at most %.0fs",
				target.host, target.job, -metric.sampleAge, subtensorConvergenceFutureSkew.Seconds(),
			)
		}
		if metric.sampleAge > subtensorConvergenceFreshness.Seconds() {
			return nil, fmt.Errorf(
				"subtensor convergence: %s/%s source sample is %.0fs old, want at most %.0fs",
				target.host, target.job, metric.sampleAge, subtensorConvergenceFreshness.Seconds(),
			)
		}
		if metric.sampleCount < subtensorConvergenceMinSamples {
			return nil, fmt.Errorf(
				"subtensor convergence: %s/%s has %.0f one-hour samples, want at least %d",
				target.host, target.job, metric.sampleCount, subtensorConvergenceMinSamples,
			)
		}
		if metric.lag < 0 || metric.targetRate < 0 || metric.importRate <= 0 ||
			metric.importSeconds <= 0 || metric.queuedBlocks < 0 {
			return nil, fmt.Errorf(
				"subtensor convergence: inconsistent one-hour measures for %s/%s lag=%.6f target_rate=%.6f import_rate=%.6f import_seconds=%.6f queued_blocks=%.6f",
				target.host, target.job, metric.lag, metric.targetRate, metric.importRate, metric.importSeconds, metric.queuedBlocks,
			)
		}
	}
	return metrics, nil
}

func sortedSubtensorConvergenceTargetKeys(targets map[string]subtensorConvergenceTarget) []string {
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func missingSubtensorConvergenceMeasures(mask int) []string {
	missing := []string{}
	for _, measure := range subtensorConvergenceMeasureNames {
		if mask&measure.bit == 0 {
			missing = append(missing, measure.name)
		}
	}
	return missing
}

func evaluateSubtensorConvergence(metric subtensorConvergenceMetrics, metricHost string) (finding, bool) {
	lag := int64(math.Round(metric.lag))
	if lag <= metric.target.lagBand {
		return finding{}, false
	}

	busyFraction := metric.importRate * metric.importSeconds
	if busyFraction < 0 {
		busyFraction = 0
	}
	etaDays := math.Inf(1)
	if metric.netRate > 0 {
		etaDays = metric.lag / metric.netRate / 86400
	}
	if metric.netRate > 0 && etaDays <= subtensorConvergenceMaxETADays {
		return finding{}, false
	}

	class := "subtensor-slow-convergence"
	symptom := fmt.Sprintf(
		"%s/%s needs an estimated %.1f days to close its %d-block Subtensor lag at the one-hour net rate",
		metric.target.host, metric.target.node, etaDays, lag,
	)
	mechanism := "The node head is advancing, but target growth leaves too little net catch-up to reach the operational band inside 14 days. Instant progress therefore conceals a recovery measured in weeks."
	if metric.netRate <= 0 {
		class = "subtensor-nonconverging"
		symptom = fmt.Sprintf(
			"%s/%s did not reduce its %d-block Subtensor lag over the one-hour window",
			metric.target.host, metric.target.node, lag,
		)
		mechanism = "The one-hour target head advanced at least as fast as the local best head, so an advancing local counter is not convergence."
	}
	if metric.queuedBlocks >= subtensorConvergenceQueuedBlocks && busyFraction >= subtensorConvergenceBusyFraction {
		mechanism += fmt.Sprintf(
			" The node retained %.0f queued blocks while verification/import occupied about %.1f%% of one block-import worker's wall time, localizing the immediate bottleneck to serial historical block import rather than peer supply.",
			metric.queuedBlocks,
			100*busyFraction,
		)
	}

	etaText := "non-converging"
	if !math.IsInf(etaDays, 1) {
		etaText = fmt.Sprintf("%.3f", etaDays)
	}
	return finding{
		probeId: "subtensor/convergence", tier: tierWarn,
		class: class, target: metric.target.host, frame: metric.target.node, sustain: 3,
		symptom:   symptom,
		mechanism: mechanism,
		baseline: fmt.Sprintf(
			"Fresh one-hour source metrics contain at least %d samples; a node outside its %d-block readiness band has positive net catch-up and an ETA no greater than %.0f days.",
			subtensorConvergenceMinSamples, metric.target.lagBand, subtensorConvergenceMaxETADays,
		),
		observed: fmt.Sprintf(
			"window=%s sync_mode=%s lag=%d net_blocks_per_second=%.6f target_blocks_per_second=%.6f imported_blocks_per_second=%.6f seconds_per_imported_block=%.6f queued_blocks=%.0f import_worker_busy_pct=%.1f eta_days=%s sample_count=%.0f sample_age_s=%.1f metrics_gateway=%s",
			subtensorConvergenceWindow, metric.target.syncMode, lag, metric.netRate,
			metric.targetRate, metric.importRate, metric.importSeconds,
			metric.queuedBlocks, 100*busyFraction, etaText, metric.sampleCount,
			metric.sampleAge, metricHost,
		),
		evidence: "Mimir computes the one-hour derivative of best and sync-target height plus verification/import counter rates, queue depth, raw-sample count, and raw-sample age for the exact host/job pair.",
		context:  "An archive full sync and a resumed warp database can both advance while failing to converge. A deep queued import pipeline plus near-full block-import-worker occupancy means adding peers cannot improve the current stage. Spare host-wide cores also cannot accelerate an importer that processes this historical path serially; faster per-core/storage hardware, a node import improvement, or a materially newer trusted chain checkpoint are distinct closure candidates. Runtime spec number alone is not checkpoint evidence: the official v452 finney checkpoint is not present in the v452 testfinney chain spec.",
		action:   "Preserve the progressing generation. Correlate this window with the exact process cgroup cpu.stat, memory.events, io.stat, host vmstat, and current image/chain-spec checkpoint. If the queue stays deep and the import worker stays busy without CPU throttling, OOM, or disk wait, do not add peers or restart the same generation. Test a newer trusted checkpoint only in an isolated generation after proving that the exact configured chain spec contains a materially newer checkpoint; otherwise operations must accept the measured wait or provision faster single-core/storage hardware. Do not replace the archive while testing a lightnode candidate.",
		verify: fmt.Sprintf(
			"The same generation reaches its readiness band, or two consecutive one-hour windows retain at least %d fresh samples, positive net catch-up, and an ETA no greater than %.0f days. Any authorized replacement must prove a materially newer checkpoint and preserve the other node's exact identity.",
			subtensorConvergenceMinSamples, subtensorConvergenceMaxETADays,
		),
		playbook: "SIGNALS.md §17.5",
	}, true
}
