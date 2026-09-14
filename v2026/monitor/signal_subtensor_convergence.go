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
	subtensorConvergenceTargetSamples
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
	{bit: subtensorConvergenceTargetSamples, name: "target_sample_count"},
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
	chain         string
	lag           float64
	netRate       float64
	targetRate    float64
	importRate    float64
	importSeconds float64
	queuedBlocks  float64
	sampleCount   float64
	sampleAge     float64
	targetSamples float64
	mask          int
}

func subtensorConvergenceTargets(hosts []*host) (map[string]subtensorConvergenceTarget, error) {
	targets := map[string]subtensorConvergenceTarget{}
	for _, configuredHost := range hosts {
		if configuredHost.subtensor == nil || len(configuredHost.subtensor.Nodes) == 0 {
			return nil, fmt.Errorf("subtensor convergence: %s has no configured nodes", configuredHost.name)
		}
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
				return nil, fmt.Errorf("subtensor convergence: %s has a node without a metrics identity", configuredHost.name)
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
				return nil, fmt.Errorf("subtensor convergence: duplicate metrics identity %s/%s", configuredHost.name, job)
			}
			targets[key] = subtensorConvergenceTarget{
				host: configuredHost.name, node: node.Name, job: job,
				lagBand: lagBand, syncMode: node.SyncMode,
			}
		}
	}
	return targets, nil
}

func exactPrometheusRegex(values []string) string {
	escaped := make([]string, len(values))
	for index, value := range values {
		escaped[index] = regexp.QuoteMeta(value)
	}
	return "^(?:" + strings.Join(escaped, "|") + ")$"
}

// Select exact inventory pairs before host/chain aggregation. Independent
// host and job matchers would admit an unconfigured cross-host job.
func subtensorConvergenceQuery(environment string, targets map[string]subtensorConvergenceTarget) string {
	hostJobs := map[string][]string{}
	for _, key := range sortedSubtensorConvergenceTargetKeys(targets) {
		target := targets[key]
		hostJobs[target.host] = append(hostJobs[target.host], target.job)
	}
	hosts := make([]string, 0, len(hostJobs))
	for host := range hostJobs {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	parts := []string{}
	for _, host := range hosts {
		labels := fmt.Sprintf(`env=%s,host=%s,job=~%s`, strconv.Quote(environment), strconv.Quote(host), strconv.Quote(exactPrometheusRegex(hostJobs[host])))
		expressions := subtensorConvergenceExpressions(labels, labels)
		for _, measure := range subtensorConvergenceMeasureNames {
			parts = append(parts, `label_replace((`+expressions[measure.name]+`),"monitor_measure",`+strconv.Quote(measure.name)+`,"","")`)
		}
	}
	return strings.Join(parts, " or ")
}

// Qualify source samples at each subquery step, then derive the shared chain
// target. Equality with own best while syncing is the node's target fallback,
// not convergence. An absent reference stays absent through the derivative.
// Separate node labels let a dashboard display one node while both configured
// jobs still supply the reference; monitor labels are identical in both roles.
func subtensorConvergenceExpressions(targetLabels, nodeLabels string) map[string]string {
	byNode := func(expression string) string { return `max by (host,chain,job) (` + expression + `)` }
	fresh := func(metric string) string {
		return `(` + metric + ` and (timestamp(` + metric + `) >= time() - 90))`
	}
	best := `substrate_block_height{` + nodeLabels + `,status="best"}`
	targetBest := byNode(fresh(`substrate_block_height{` + targetLabels + `,status="best"}`))
	target := byNode(fresh(`substrate_block_height{` + targetLabels + `,status="sync_target"}`))
	major := byNode(fresh(`substrate_sub_libp2p_is_major_syncing{` + targetLabels + `}`))
	trusted := `((` + target + ` > ` + targetBest + `) or ((` + target + ` == ` + targetBest + `) and (` + major + ` == 0)))`
	canonical := `max by (host,chain) (` + trusted + `)`
	targetRange := `(` + canonical + `)[1h:15s]`
	targetRate := `deriv(` + targetRange + `)`
	targetSamples := `count_over_time(` + targetRange + `)`
	freshBest := byNode(fresh(best))
	// A reference behind this node is inconsistent, not zero lag. Requiring
	// current lag also prevents a populated historical range from presenting
	// convergence after all current target sources have disappeared.
	lag := `((` + canonical + `) - on (host,chain) group_right () ` + freshBest + `) >= 0`
	broadcast := func(expression string) string {
		return `(((` + expression + `) + on (host,chain) group_right () (0 * ` + freshBest + `)) and on (host,chain,job) (` + lag + `))`
	}
	sumRate := func(name string) string {
		return `sum by (host,chain,job) (rate(` + name + `{` + nodeLabels + `}[1h]))`
	}
	importRate := sumRate(`substrate_block_verification_and_import_time_count`)
	importTime := sumRate(`substrate_block_verification_and_import_time_sum`)
	return map[string]string{
		"lag":                 lag,
		"net_rate":            `(` + byNode(`deriv(`+best+`[1h])`) + ` - on (host,chain) group_left () (` + targetRate + `)) and on (host,chain,job) (` + lag + `) and on (host,chain) (` + targetRate + ` >= 0)`,
		"target_rate":         broadcast(targetRate),
		"import_rate":         importRate,
		"import_seconds":      `(` + importTime + ` / (` + importRate + ` > 0)) or ((0 * (` + importRate + ` == 0)) and (` + importTime + ` == 0))`,
		"queued_blocks":       byNode(`substrate_sync_queued_blocks{` + nodeLabels + `}`),
		"sample_count":        byNode(`count_over_time(` + best + `[1h])`),
		"sample_age":          `time() - ` + byNode(`timestamp(`+best+`)`),
		"target_sample_count": broadcast(targetSamples),
	}
}

func (subtensorConvergenceProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	subtensorHosts := env.cfg.hostsWithRole("subtensor")
	if len(subtensorHosts) == 0 {
		return nil, fmt.Errorf("subtensor convergence: no subtensor host in inventory")
	}
	targets, err := subtensorConvergenceTargets(subtensorHosts)
	if err != nil {
		return nil, err
	}
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("subtensor convergence: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(subtensorConvergenceQuery(env.cfg.env, targets))
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
		chain := series.Metric["chain"]
		if chain == "" || (metric.chain != "" && metric.chain != chain) {
			return nil, fmt.Errorf("subtensor convergence: missing or mixed chain identity for %s/%s", host, job)
		}
		metric.chain = chain
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
		case "target_sample_count":
			bit, metric.targetSamples = subtensorConvergenceTargetSamples, value
		default:
			return nil, fmt.Errorf("subtensor convergence: unknown measure %q", measure)
		}
		if metric.mask&bit != 0 {
			return nil, fmt.Errorf("subtensor convergence: duplicate %s for %s/%s", measure, host, job)
		}
		metric.mask |= bit
		metrics[key] = metric
	}

	hostChains := map[string]string{}
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
		if previous := hostChains[target.host]; previous != "" && previous != metric.chain {
			return nil, fmt.Errorf("subtensor convergence: configured jobs on %s expose different chains", target.host)
		}
		hostChains[target.host] = metric.chain
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
		if metric.targetSamples < subtensorConvergenceMinSamples {
			return nil, fmt.Errorf("subtensor convergence: %s/%s has %.0f trusted target samples, want at least %d; syncing fallback is not a chain target", target.host, target.job, metric.targetSamples, subtensorConvergenceMinSamples)
		}
		if metric.lag < 0 || metric.targetRate < 0 || metric.importRate < 0 ||
			metric.importSeconds < 0 || metric.queuedBlocks < 0 ||
			(metric.importRate == 0 && metric.importSeconds != 0) {
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
	mechanism := "The trailing-one-hour best-head slope remains positive, but target growth leaves too little net catch-up to reach the operational band inside 14 days. That historical slope does not prove the current head is advancing; a co-resident subtensor-progress finding is the stronger current-state signal."
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
			"Fresh one-hour best-head and trusted same-host/chain target histories each contain at least %d samples; a node outside its %d-block readiness band has positive net catch-up and an ETA no greater than %.0f days. A syncing node's own-best target fallback is not a chain reference.",
			subtensorConvergenceMinSamples, metric.target.lagBand, subtensorConvergenceMaxETADays,
		),
		observed: fmt.Sprintf(
			"window=%s sync_mode=%s chain=%s lag=%d net_blocks_per_second=%.6f target_blocks_per_second=%.6f imported_blocks_per_second=%.6f seconds_per_imported_block=%.6f queued_blocks=%.0f import_worker_busy_pct=%.1f eta_days=%s sample_count=%.0f trusted_target_sample_count=%.0f sample_age_s=%.1f metrics_gateway=%s",
			subtensorConvergenceWindow, metric.target.syncMode, metric.chain, lag, metric.netRate,
			metric.targetRate, metric.importRate, metric.importSeconds,
			metric.queuedBlocks, 100*busyFraction, etaText, metric.sampleCount,
			metric.targetSamples, metric.sampleAge, metricHost,
		),
		evidence: "Mimir selects exact configured host/job pairs, rejects targets older than 90s and own-best fallbacks while syncing, then takes the same-host/chain maximum before its one-hour derivative. Best-head derivatives, verification/import rates, queue depth, raw-best and trusted-target sample counts, and source age retain host/chain/job identity. A missing current reference is observation loss even when historical target samples remain.",
		context:  "An archive full sync and a resumed warp database can both advance while failing to converge. A deep queued import pipeline plus near-full block-import-worker occupancy means adding peers cannot improve the current stage. Spare host-wide cores also cannot accelerate an importer that processes this historical path serially; faster per-core/storage hardware, a node import improvement, or a materially newer trusted chain checkpoint are distinct closure candidates. Runtime spec number alone is not checkpoint evidence: the official v452 finney checkpoint is not present in the v452 testfinney chain spec.",
		action:   "Preserve the generation and its evidence. If subtensor-progress is also active, follow that current static-head boundary first; this trailing-one-hour finding does not establish current movement. First corroborate fresh best, sync_target, and major_syncing samples on both configured jobs of this host and chain; equal target/best while syncing is fallback, not zero lag. Correlate a qualified window with the exact process cgroup cpu.stat, memory.events, io.stat, host vmstat, and current image/chain-spec checkpoint. If the queue stays deep and the import worker stays busy without CPU throttling, OOM, or disk wait, do not add peers or restart the same generation. Test a newer trusted checkpoint only in an isolated generation after proving that the exact configured chain spec contains a materially newer checkpoint; otherwise operations must accept the measured wait or provision faster single-core/storage hardware. Do not replace the archive while testing a lightnode candidate.",
		verify: fmt.Sprintf(
			"With a currently visible trusted reference, the same generation reaches its readiness band, or two consecutive one-hour windows each retain at least %d raw-best and trusted-target samples, positive net catch-up, and an ETA no greater than %.0f days. Missing target evidence cannot resolve an alert. Any authorized replacement must prove a materially newer checkpoint and preserve the other node's exact identity.",
			subtensorConvergenceMinSamples, subtensorConvergenceMaxETADays,
		),
		playbook: "SIGNALS.md §17.5",
	}, true
}
