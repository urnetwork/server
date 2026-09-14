package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	// The entire added qualification is bounded, including both helper phases
	// and the intervening query; slow observations remain visibility.
	subtensorConvergenceGenerationDeadline    = 55 * time.Second
	subtensorConvergenceGenerationCallTimeout = 25 * time.Second
	subtensorConvergenceGenerationWorkers     = 4
	subtensorConvergenceGenerationMaxBytes    = 64 * 1024
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
	host          string
	node          string
	job           string
	containerName string
	lagBand       int64
	syncMode      string
}

type subtensorConvergenceMetrics struct {
	target        subtensorConvergenceTarget
	observedAt    time.Time
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
			// Metrics jobs follow independently supervised container names.
			// A semantic fallback cannot qualify a container generation.
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
				containerName: node.ContainerName,
				lagBand:       lagBand, syncMode: node.SyncMode,
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

func (self subtensorConvergenceProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

	qualificationCtx, cancel := context.WithTimeout(ctx, subtensorConvergenceGenerationDeadline)
	defer cancel()
	beforeGenerationKVs := observeSubtensorConvergenceGenerations(qualificationCtx, env, targets)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pinnedTime := env.now().UTC().Truncate(time.Millisecond)
	hostStateKVs := map[string]string{}
	for _, key := range sortedSubtensorConvergenceTargetKeys(targets) {
		target := targets[key]
		observation := beforeGenerationKVs[key]
		state := observation.state
		if state == "qualified" {
			if observation.identity.started.After(pinnedTime) {
				state = "future"
			} else if observation.identity.started.After(pinnedTime.Add(-time.Hour)) {
				state = "young"
			}
		}
		if state != "qualified" && hostStateKVs[target.host] == "" {
			hostStateKVs[target.host] = state
		}
	}
	if qualificationCtx.Err() != nil {
		for _, target := range targets {
			hostStateKVs[target.host] = "deadline"
		}
	}
	activeTargets := map[string]subtensorConvergenceTarget{}
	for key, target := range targets {
		if hostStateKVs[target.host] == "" {
			activeTargets[key] = target
		}
	}
	visibilityFindings := func() []finding {
		findings := []finding{}
		for _, key := range sortedSubtensorConvergenceTargetKeys(targets) {
			target := targets[key]
			if state := hostStateKVs[target.host]; state != "" {
				findings = append(findings, subtensorConvergenceGenerationFinding(target, state))
			}
		}
		return findings
	}
	observationFailure := func(err error) ([]finding, error) {
		if parentErr := ctx.Err(); parentErr != nil {
			return nil, parentErr
		}
		if len(hostStateKVs) == 0 {
			return nil, err
		}
		// Preserve already established per-host generation visibility when an
		// independent metrics source fails; never render its private error.
		for _, target := range activeTargets {
			hostStateKVs[target.host] = "metrics-unobservable"
		}
		return visibilityFindings(), nil
	}
	if len(activeTargets) == 0 {
		return visibilityFindings(), nil
	}

	// The query retains the full desired population. Explicit visibility, not
	// a reduced denominator, withholds unqualified same-host histories.
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(subtensorConvergenceQuery(env.cfg.env, targets)) +
		"&time=" + url.QueryEscape(pinnedTime.Format(time.RFC3339Nano))
	out, metricHost, err := shellFirstServiceGateway(
		qualificationCtx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if qualificationCtx.Err() != nil {
		for _, target := range activeTargets {
			hostStateKVs[target.host] = "deadline"
		}
		return visibilityFindings(), nil
	}
	if err != nil {
		return observationFailure(fmt.Errorf("subtensor convergence: query Mimir through service gateways: %w", err))
	}

	if len(activeTargets) != len(targets) {
		var response mimirInstantResponse
		if err := json.Unmarshal([]byte(out), &response); err != nil {
			return observationFailure(fmt.Errorf("subtensor convergence: decode Mimir response: %w", err))
		}
		selectedSeries := response.Data.Result[:0]
		for _, series := range response.Data.Result {
			key := series.Metric["host"] + "\x00" + series.Metric["job"]
			if _, expected := targets[key]; !expected {
				return observationFailure(fmt.Errorf("subtensor convergence: unexpected metrics identity"))
			}
			if _, active := activeTargets[key]; active {
				selectedSeries = append(selectedSeries, series)
			}
		}
		response.Data.Result = selectedSeries
		filtered, err := json.Marshal(response)
		if err != nil {
			return observationFailure(fmt.Errorf("subtensor convergence: reduce qualified histories: %w", err))
		}
		out = string(filtered)
	}
	metrics, err := parseSubtensorConvergence(out, activeTargets, env.now().UTC())
	if err != nil {
		return observationFailure(err)
	}
	for _, metric := range metrics {
		if !metric.observedAt.Round(time.Millisecond).Equal(pinnedTime) {
			return observationFailure(fmt.Errorf("subtensor convergence: Mimir evaluation does not match pinned time"))
		}
	}

	afterGenerationKVs := observeSubtensorConvergenceGenerations(qualificationCtx, env, activeTargets)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, key := range sortedSubtensorConvergenceTargetKeys(activeTargets) {
		target := activeTargets[key]
		before := beforeGenerationKVs[key]
		after := afterGenerationKVs[key]
		state := after.state
		if state == "qualified" && (!before.identity.started.Equal(after.identity.started) ||
			before.identity.image != after.identity.image || before.identity.dataPath != after.identity.dataPath) {
			state = "changed"
		}
		if state != "qualified" && hostStateKVs[target.host] == "" {
			hostStateKVs[target.host] = state
		}
	}
	checkedAt := env.now().UTC()
	for key, metric := range metrics {
		metric.sampleAge += checkedAt.Sub(pinnedTime).Seconds()
		metrics[key] = metric
		if checkedAt.Before(pinnedTime) || checkedAt.Sub(pinnedTime) > subtensorConvergenceFreshness ||
			metric.sampleAge > subtensorConvergenceFreshness.Seconds() || metric.sampleAge < -subtensorConvergenceFutureSkew.Seconds() {
			if hostStateKVs[metric.target.host] == "" {
				hostStateKVs[metric.target.host] = "source-stale"
			}
		}
	}
	// A child deadline on one sibling does not erase completed independent
	// brackets; parent cancellation above remains authoritative lifecycle.
	findings := visibilityFindings()
	for _, key := range sortedSubtensorConvergenceTargetKeys(activeTargets) {
		metric := metrics[key]
		if hostStateKVs[metric.target.host] == "" {
			if finding, ok := evaluateSubtensorConvergence(metric, metricHost.name); ok {
				findings = append(findings, finding)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return findings, nil
}

// Raw identity values stay only in this run's private comparison state.
type subtensorConvergenceGenerationIdentity struct {
	started  time.Time
	image    string
	dataPath string
}

type subtensorConvergenceGenerationObservation struct {
	identity subtensorConvergenceGenerationIdentity
	state    string
}

// Bounded workers reuse inventory admission and join their context-aware
// transports before returning; no helper/schema or shared client is changed.
func observeSubtensorConvergenceGenerations(ctx context.Context, env *probeEnv, targets map[string]subtensorConvergenceTarget) map[string]subtensorConvergenceGenerationObservation {
	hostKVs := map[string]*host{}
	for _, configured := range env.cfg.hosts {
		hostKVs[configured.name] = configured
	}
	keys := sortedSubtensorConvergenceTargetKeys(targets)
	jobs := make(chan string, len(keys))
	type result struct {
		key         string
		observation subtensorConvergenceGenerationObservation
	}
	results := make(chan result, len(keys))
	for _, key := range keys {
		jobs <- key
	}
	close(jobs)
	var workers sync.WaitGroup
	for worker := 0; worker < min(subtensorConvergenceGenerationWorkers, len(keys)); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for key := range jobs {
				target := targets[key]
				observation := subtensorConvergenceGenerationObservation{state: "helper-unobservable"}
				if ctx.Err() != nil {
					observation.state = "deadline"
				} else if target.containerName == "" {
					observation.state = "missing-container"
				} else if target.containerName != "subtensor" && target.containerName != "subtensor-lightnode" {
					observation.state = "unsupported-container"
				} else if configured := hostKVs[target.host]; configured != nil {
					commandCtx, cancel := context.WithTimeout(ctx, subtensorConvergenceGenerationCallTimeout)
					output, err := env.runner.shell(commandCtx, configured,
						"sudo -n /usr/local/sbin/subtensor-monitor "+shellSingleQuote(target.containerName))
					commandErr := commandCtx.Err()
					cancel()
					if hostScopeOnlyError(err) {
						observation.state = "scope-excluded"
					} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(commandErr, context.DeadlineExceeded) || ctx.Err() != nil {
						observation.state = "deadline"
					} else if err == nil && commandErr == nil {
						observation.identity, observation.state = parseSubtensorConvergenceGeneration(output)
					}
				}
				results <- result{key: key, observation: observation}
			}
		}()
	}
	workers.Wait()
	close(results)
	observations := map[string]subtensorConvergenceGenerationObservation{}
	for result := range results {
		observations[result.key] = result.observation
	}
	return observations
}

// Reject duplicate/trailing JSON and incomplete helper identities, including
// exit-zero container_error. No private payload or parse detail is rendered.
func parseSubtensorConvergenceGeneration(output string) (subtensorConvergenceGenerationIdentity, string) {
	unknown := subtensorConvergenceGenerationIdentity{}
	if len(output) > subtensorConvergenceGenerationMaxBytes {
		return unknown, "helper-unobservable"
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return unknown, "helper-unobservable"
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		name, named := token.(string)
		if err != nil || !named {
			return unknown, "helper-unobservable"
		}
		if _, duplicate := fields[name]; duplicate {
			return unknown, "helper-unobservable"
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return unknown, "helper-unobservable"
		}
		fields[name] = value
	}
	closingToken, err := decoder.Token()
	var trailing json.RawMessage
	if err != nil || closingToken != json.Delim('}') || decoder.Decode(&trailing) != io.EOF {
		return unknown, "helper-unobservable"
	}
	if _, failed := fields["container_error"]; failed {
		return unknown, "helper-unobservable"
	}
	var startString, image, dataPath string
	if json.Unmarshal(fields["container_started"], &startString) != nil ||
		json.Unmarshal(fields["container_image"], &image) != nil || strings.TrimSpace(image) == "" ||
		json.Unmarshal(fields["data_path"], &dataPath) != nil || !filepath.IsAbs(dataPath) {
		return unknown, "helper-unobservable"
	}
	started, err := time.Parse(time.RFC3339Nano, startString)
	if err != nil || started.IsZero() || started.Unix() <= 0 {
		return unknown, "helper-unobservable"
	}
	return subtensorConvergenceGenerationIdentity{started: started.UTC(), image: image, dataPath: dataPath}, "qualified"
}

// Generation uncertainty is visibility for all same-host reference
// contributors, not a production outage or convergence/healthy measurement.
func subtensorConvergenceGenerationFinding(target subtensorConvergenceTarget, state string) finding {
	f := cannotObserveFinding(target.host, fmt.Errorf("Subtensor generation qualification unavailable"))
	f.frame = target.node
	f.symptom = "The monitor could not qualify this node's same-generation Subtensor hour"
	f.mechanism = "Count and freshness do not prove process continuity. Every configured same-host canonical-reference contributor must have complete, stable helper identity bracketing the pinned Mimir evaluation before this node's convergence is known."
	f.baseline = "An explicit supported container returns a valid start no later than the one-hour boundary and unchanged start/image/data identity before and after the query, with current source samples still inside their freshness band."
	f.observed = fmt.Sprintf("generation_state=%s window=%s generation_window_qualified=false helper_values_rendered=false", state, subtensorConvergenceWindow)
	f.evidence = "Qualification requires the existing restricted helper under a 55-second total child deadline, with at most four workers and 25-second per-call deadlines. Available identity fields are compared privately; absent identity is never zero. Unqualified histories remain explicit coverage loss and the full desired inventory is retained."
	f.context = "This records observed container/data continuity, not universal proof against a hidden child-process or collector restart. Missing, young or changed identity is not nonconvergence, outage or recovery."
	f.action = "Preserve the generation. Restore missing helper/identity visibility or wait for a complete same-generation hour, then repeat the bounded comparison; do not restart the node or reduce the sample threshold from this finding."
	f.verify = "All configured contributors on this host return complete stable identity, each start is no later than the pinned hour boundary, and the existing raw-count, trusted-target and source-freshness gates pass. Only then evaluate convergence; a restart alone cannot resolve the boundary."
	f.playbook = "SIGNALS.md §17.5"
	if state == "metrics-unobservable" {
		f.symptom = "The monitor could not observe this node's pinned Subtensor convergence metrics"
		f.mechanism = "Another host's generation visibility remains independently established, but this node's metrics source was unavailable or incompatible. Missing metrics cannot establish a typed convergence result or healthy recovery."
		f.evidence += " The pinned metrics observation was not interpretable; private transport and parse details are omitted."
	}
	return f
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
		if metric.mask != 0 && !metric.observedAt.Equal(observedAt) {
			return nil, fmt.Errorf("subtensor convergence: inconsistent Mimir evaluation times")
		}
		metric.observedAt = observedAt
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
