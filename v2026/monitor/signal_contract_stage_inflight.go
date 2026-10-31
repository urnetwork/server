// SIGNALS.md §26 measures current synchronous OTHER occupancy: a count, not call age or compensation attribution.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Configurable counts and source-clock maturity; zero selects defaults, not disablement.
type ContractStageInflightSettings struct {
	WarnCalls float64
	Sustain   int
}

// Root-selected provisional WARN policy; calibration is still required.
func DefaultContractStageInflightSettings() *ContractStageInflightSettings {
	return &ContractStageInflightSettings{WarnCalls: 1000, Sustain: 2}
}

// Measures current OTHER owners independently of completed-call rates (§26).
func NewContractStageInflightSignal() Signal {
	return NewContractStageInflightSignalWithSettings(nil)
}

// Custom positive count and observation thresholds retain the same coverage contract.
func NewContractStageInflightSignalWithSettings(settings *ContractStageInflightSettings) Signal {
	values := *DefaultContractStageInflightSettings()
	if settings != nil {
		if settings.WarnCalls != 0 {
			values.WarnCalls = settings.WarnCalls
		}
		if settings.Sustain != 0 {
			values.Sustain = settings.Sustain
		}
	}
	return &signalAdapter{number: "26", key: "contract-stage-inflight", name: "Current Proxy OTHER stage occupancy", probe: &contractStageInflightProbe{settings: values, previousKVs: map[string]contractStageInflightPrevious{}}}
}

const contractStageInflightId = "mimir/contract-stage-inflight"
const contractStageInflightClass = "contract-stage-inflight-high"
const contractStageInflightSource = "__source_timestamp"
const contractStageInflightMaxBytes = 4 << 20

// Only fixed metric aliases, never arbitrary stage labels, reach the reducer.
var contractStageInflightMetrics = []string{"process_start_time_seconds", "urnetwork_contract_creation_stage_timing_enabled", "other_internal", "other_http"}

// A baseline belongs to one process generation and a strictly advancing original scrape clock.
type contractStageInflightPrevious struct {
	instance  string
	start     float64
	source    time.Time
	highCount int
}

// Serializes source-clock maturity; canceled observations never commit a baseline.
type contractStageInflightProbe struct {
	settings    ContractStageInflightSettings
	stateLock   sync.Mutex
	previousKVs map[string]contractStageInflightPrevious
}

// Identity is stable across process replacements; generation is evidence, not an alert nonce.
func (*contractStageInflightProbe) id() string             { return contractStageInflightId }
func (*contractStageInflightProbe) tier() string           { return tierWarn }
func (*contractStageInflightProbe) cadence() time.Duration { return time.Minute }

// One exact vector retains each metric and original source timestamp independently.
func contractStageInflightQuery(environment string) string {
	parts := []string{}
	for _, metric := range contractStageInflightMetrics {
		selector := metric + `{env=` + strconv.Quote(environment) + `,job="proxy",instance!=""}`
		if strings.HasPrefix(metric, "other_") {
			selector = `urnetwork_contract_creation_stage_inflight{env=` + strconv.Quote(environment) + `,job="proxy",instance!="",stage="other",ingress=` + strconv.Quote(strings.TrimPrefix(metric, "other_")) + `}`
		}
		for _, source := range []bool{false, true} {
			expression, alias := selector, metric
			if source {
				expression = "timestamp(" + selector + ")"
				alias += contractStageInflightSource
			}
			parts = append(parts, `label_replace(`+expression+`,"monitor_metric",`+strconv.Quote(alias)+`,"job",".*")`)
		}
	}
	return strings.Join(parts, " or ")
}

// Process labels remain private to the evaluator; targets are inventory-owned slots.
type contractStageInflightProcess struct {
	host, block, instance string
	values                map[string]float64
	sources               map[string]time.Time
}

// Validate a bounded vector without returning remote diagnostic text or labels.
func parseContractStageInflight(out string, now time.Time, expected map[string]string) (map[string]*contractStageInflightProcess, error) {
	var response struct {
		mimirInstantResponse
		Infos []string `json:"infos"`
	}
	if len(out) > contractStageInflightMaxBytes || json.Unmarshal([]byte(out), &response) != nil || response.Status != "success" || len(response.Warnings) != 0 || len(response.Infos) != 0 || response.Data.ResultType != "vector" || len(response.Data.Result) > 4096 {
		return nil, fmt.Errorf("invalid bounded stage-inflight response")
	}
	allowed := map[string]bool{}
	for _, metric := range contractStageInflightMetrics {
		allowed[metric] = true
		allowed[metric+contractStageInflightSource] = true
	}
	processes := map[string]*contractStageInflightProcess{}
	for _, series := range response.Data.Result {
		metric := series.Metric["monitor_metric"]
		if job := series.Metric["job"]; job != "" && job != "proxy" {
			return nil, fmt.Errorf("unexpected stage-inflight service")
		}
		if !allowed[metric] {
			return nil, fmt.Errorf("unknown stage-inflight metric")
		}
		host, block, instance := series.Metric["host"], series.Metric["block"], series.Metric["instance"]
		if _, ok := expected[host+"\x00"+block]; !ok {
			return nil, fmt.Errorf("unexpected stage-inflight slot")
		}
		if strings.HasPrefix(metric, "other_") {
			ingress := strings.TrimSuffix(strings.TrimPrefix(metric, "other_"), contractStageInflightSource)
			if series.Metric["stage"] != "other" || series.Metric["ingress"] != ingress {
				return nil, fmt.Errorf("invalid stage-inflight fixed labels")
			}
		}
		if instance == "" {
			return nil, fmt.Errorf("missing stage-inflight instance")
		}
		observed, value, err := mimirInstantValue(series.Value)
		if err != nil || observed.Before(now.Add(-30*time.Second)) || observed.After(now.Add(30*time.Second)) || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid stage-inflight sample")
		}
		key := host + "\x00" + block + "\x00" + instance
		process := processes[key]
		if process == nil {
			process = &contractStageInflightProcess{host: host, block: block, instance: instance, values: map[string]float64{}, sources: map[string]time.Time{}}
			processes[key] = process
		}
		if strings.HasSuffix(metric, contractStageInflightSource) {
			metric = strings.TrimSuffix(metric, contractStageInflightSource)
			if _, ok := process.sources[metric]; ok {
				return nil, fmt.Errorf("duplicate stage-inflight source")
			}
			process.sources[metric] = unixFloatTime(value)
		} else {
			if _, ok := process.values[metric]; ok {
				return nil, fmt.Errorf("duplicate stage-inflight value")
			}
			process.values[metric] = value
		}
	}
	return processes, nil
}

// Unknown is explicit visibility, so missing evidence cannot supply absence credit.
func contractStageInflightUnknown() finding {
	return finding{probeId: contractStageInflightId, tier: tierWarn, class: "contract-stage-inflight-unobservable", target: "proxy-fleet", sustain: 1, unobservable: true,
		symptom: "Current Proxy OTHER occupancy is not fully observable", baseline: "Every configured Proxy slot has one current process with coherent fresh start, capability and both ingress gauges.",
		mechanism: "A missing, stale, repeated, replaced or inconsistent process observation cannot establish two consecutive source-clock measurements or recovery.",
		observed:  "current_stage_coverage=unknown", context: "This does not establish hung work, compensation failure, Redis pressure or an outage.", action: "Restore exact process-generation and scrape coverage; retain existing incidents while evidence is incomplete.", verify: "Obtain fresh complete same-generation observations for the configured Proxy scope.", playbook: "SIGNALS.md §26"}
}

// Invalid policy fails observation rather than disabling the detector silently.
func (self *contractStageInflightProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if !proxyDeviceTargetWholeNonnegative(self.settings.WarnCalls) || self.settings.WarnCalls > 9007199254740991 || self.settings.WarnCalls <= 0 || self.settings.Sustain < 2 {
		return nil, fmt.Errorf("invalid stage-inflight count policy")
	}
	expected := map[string]string{}
	hosts := []string{}
	for _, host := range env.cfg.hosts {
		if host.proxy != nil {
			hosts = append(hosts, host.name)
		}
	}
	blocks := env.cfg.logServiceBlocks["proxy"]
	if len(hosts) == 0 || len(blocks) == 0 || env.cfg.proxyPathExpectedHosts > 0 && len(hosts) != env.cfg.proxyPathExpectedHosts {
		return []finding{contractStageInflightUnknown()}, nil
	}
	for _, host := range hosts {
		for _, block := range blocks {
			expected[host+"\x00"+block] = host + "/" + block
		}
	}
	now := env.now().UTC().Truncate(15 * time.Second)
	command := "# monitor-signal-26-contract-stage-inflight\n" + "curl -fsS --max-time 20 --max-filesize " + strconv.Itoa(contractStageInflightMaxBytes) + " --data-urlencode " + shellSingleQuote("query="+contractStageInflightQuery(env.cfg.env)) + " --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(now.Unix(), 10)) + " 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	out, _, err := mimirShellFirstServiceGatewayParsed(ctx, env.runner, env.cfg.hostsWithRole("services"), nil, command, contractStageInflightMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("stage-inflight bounded observation failed: %w", err)
	}
	processes, err := parseContractStageInflight(out, now, expected)
	if err != nil {
		return nil, err
	}
	return self.assess(ctx, processes, expected, now), nil
}

// Maturity counts strictly advancing common source clocks, never monitor ticks.
func (self *contractStageInflightProbe) assess(ctx context.Context, processes map[string]*contractStageInflightProcess, expected map[string]string, now time.Time) []finding {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	selected, gaps := newestProxyProcesses(now, processes, func(process *contractStageInflightProcess) proxyProcessGeneration {
		start, present := process.values["process_start_time_seconds"]
		source, hasSource := process.sources["process_start_time_seconds"]
		reason := ""
		if !hasSource || source.Before(now.Add(-90*time.Second)) || source.After(now.Add(30*time.Second)) || start > float64(source.UnixNano())/1e9 {
			reason = "invalid-process-start-source"
		}
		return proxyProcessGeneration{host: process.host, block: process.block, instance: process.instance, current: true, start: start, startPresent: present, startReason: reason}
	})
	currentKVs := map[string]*contractStageInflightProcess{}
	for _, process := range selected {
		currentKVs[process.host+"\x00"+process.block] = process
	}
	nextKVs := map[string]contractStageInflightPrevious{}
	findings := []finding{}
	unknown := len(gaps) > 0
	slots := []string{}
	for slot := range expected {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		process := currentKVs[slot]
		if process == nil {
			unknown = true
			continue
		}
		source := process.sources["process_start_time_seconds"]
		complete := true
		for _, metric := range contractStageInflightMetrics {
			value, present := process.values[metric]
			stamp, hasSource := process.sources[metric]
			validValue := proxyDeviceTargetWholeNonnegative(value) && value <= 9007199254740991
			if metric == "process_start_time_seconds" {
				validValue = proxyDeviceTargetFinitePositive(value)
			}
			if !present || !hasSource || !stamp.Equal(source) || !validValue {
				complete = false
			}
		}
		if process.values["urnetwork_contract_creation_stage_timing_enabled"] != 1 {
			complete = false
		}
		if !complete {
			unknown = true
			continue
		}
		previous, present := self.previousKVs[slot]
		if present && previous.instance == process.instance && previous.start == process.values["process_start_time_seconds"] && !source.After(previous.source) {
			previous.highCount = 0
			nextKVs[slot] = previous
			unknown = true
			continue
		}
		count := process.values["other_internal"] + process.values["other_http"]
		if !proxyDeviceTargetWholeNonnegative(count) || count > 9007199254740991 {
			unknown = true
			continue
		}
		highCount := 0
		if count > self.settings.WarnCalls {
			highCount = 1
			if present && previous.instance == process.instance && previous.start == process.values["process_start_time_seconds"] {
				highCount += previous.highCount
				if highCount > self.settings.Sustain {
					highCount = self.settings.Sustain
				}
			}
		}
		nextKVs[slot] = contractStageInflightPrevious{instance: process.instance, start: process.values["process_start_time_seconds"], source: source, highCount: highCount}
		if !present || previous.instance != process.instance || previous.start != process.values["process_start_time_seconds"] {
			unknown = true
		}
		if highCount >= self.settings.Sustain {
			findings = append(findings, finding{probeId: contractStageInflightId, tier: tierWarn, class: contractStageInflightClass, target: expected[slot], sustain: 1,
				symptom: "Current Proxy OTHER occupancy exceeds the configured count band", baseline: fmt.Sprintf("OTHER owners <= %.0f per current process; %d distinct consecutive source observations required.", self.settings.WarnCalls, self.settings.Sustain),
				mechanism: "The producer moves each synchronous call between exclusive stages. Current OTHER owners include unfinished calls that completed-call rates cannot see.", observed: fmt.Sprintf("other_inflight=%.0f threshold=%.0f distinct_high_observations=%d", count, self.settings.WarnCalls, highCount),
				context: "WARN only. Occupancy is not individual call age, a compensation queue, Redis pressure, CPU saturation or outage proof; turnover can produce high occupancy.", action: "Correlate the same process generation with bounded stage and compensation-owner evidence before choosing a repair; do not raise pools or restart from this gauge alone.", verify: "Fresh below-band observations with full scope allow the standard notifier recovery hold; unknown or replacement evidence cannot clear.", playbook: "SIGNALS.md §26"})
		} else if highCount > 0 {
			findings = append(findings, finding{probeId: contractStageInflightId, class: contractStageInflightClass, target: expected[slot], clearHold: true})
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	self.previousKVs = nextKVs
	if unknown {
		findings = append(findings, contractStageInflightUnknown())
	}
	return findings
}
