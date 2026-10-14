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

// Signal proxy-device-target implements SIGNALS.md §14.7d. The process-wide
// admission gate was removed; this checks the aggregate of private 24 MiB
// targets without mistaking the aggregate for a shared budget.
func NewProxyDeviceTargetSignal() Signal {
	return &signalAdapter{
		number: "14.7d", key: "proxy-device-target", name: "Proxy private device targets",
		probe: proxyDeviceTargetProbe{},
	}
}

type proxyDeviceTargetProbe struct{}

func (proxyDeviceTargetProbe) id() string             { return "runtime/proxy-device-target" }
func (proxyDeviceTargetProbe) tier() string           { return tierWarn }
func (proxyDeviceTargetProbe) cadence() time.Duration { return time.Minute }

const (
	proxyDeviceTargetMarker    = "monitor-signal-14.7d-proxy-device-target"
	proxyDeviceTargetEndpoint  = "http://127.0.0.1:3100/prometheus/api/v1/query"
	proxyDeviceTargetFreshness = 90 * time.Second
	proxyDeviceTargetBytes     = 24 * 1024 * 1024
	proxyDeviceTargetMaxBytes  = 4 << 20
	proxyDeviceTargetMaxSeries = 1024
	proxyDeviceTargetSource    = "__source_timestamp"
	proxyDeviceLegacyBudget    = "urnetwork_proxy_device_memory_budget_bytes"
)

var proxyDeviceTargetMetrics = []string{
	"process_start_time_seconds",
	"urnetwork_proxy_devices_live",
	"urnetwork_proxy_device_memory_target_bytes",
	"urnetwork_proxy_device_memory_tracked_used_bytes",
}

type proxyDeviceTargetProcess struct {
	host, block, instance string
	values                map[string]float64
	sources               map[string]time.Time
}

func proxyDeviceTargetQuery(environment string) string {
	parts := make([]string, 0, 2*(len(proxyDeviceTargetMetrics)+1))
	for _, metric := range append(append([]string{}, proxyDeviceTargetMetrics...), proxyDeviceLegacyBudget) {
		series := fmt.Sprintf(`%s{env=%s,job="proxy",instance!=""}`, metric, strconv.Quote(environment))
		fresh := fmt.Sprintf(`timestamp(%s) >= time() - %d`, series, int64(proxyDeviceTargetFreshness/time.Second))
		parts = append(parts,
			fmt.Sprintf(`(label_replace(%s,"monitor_metric",%s,"job",".*") and on(env,host,block,instance) (%s))`, series, strconv.Quote(metric), fresh),
			fmt.Sprintf(`(label_replace(timestamp(%s),"monitor_metric",%s,"job",".*") and on(env,host,block,instance) (%s))`, series, strconv.Quote(metric+proxyDeviceTargetSource), fresh),
		)
	}
	return strings.Join(parts, " or ")
}

func (proxyDeviceTargetProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	expectedHosts := []string{}
	for _, host := range env.cfg.hosts {
		if host.proxy != nil {
			expectedHosts = append(expectedHosts, host.name)
		}
	}
	if len(expectedHosts) == 0 {
		return []finding{healthyFinding("runtime/proxy-device-target", tierWarn, "proxy-device-target-mismatch", "proxy-fleet")}, nil
	}
	blocks := env.cfg.logServiceBlocks["proxy"]
	if len(blocks) == 0 || env.cfg.proxyPathExpectedHosts > 0 && len(expectedHosts) != env.cfg.proxyPathExpectedHosts {
		return []finding{proxyDeviceTargetFinding("proxy-device-target-unobservable", "inventory", "Proxy placement inventory is incomplete", fmt.Sprintf("hosts=%d blocks=%d expected_hosts=%d", len(expectedHosts), len(blocks), env.cfg.proxyPathExpectedHosts))}, nil
	}
	expected := map[string]string{}
	for _, host := range expectedHosts {
		for _, block := range blocks {
			expected[host+"\x00"+block] = host + "/" + block
		}
	}
	gateways := env.cfg.hostsWithRole("services")
	if len(gateways) == 0 {
		return nil, fmt.Errorf("proxy device target: no services host for Mimir query")
	}
	now := env.now().UTC().Truncate(15 * time.Second)
	command := "# " + proxyDeviceTargetMarker + "\n" +
		"curl -fsS --max-time 20 --max-filesize " + strconv.Itoa(proxyDeviceTargetMaxBytes) +
		" --data-urlencode " + shellSingleQuote("query="+proxyDeviceTargetQuery(env.cfg.env)) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(now.Unix(), 10)) +
		" " + shellSingleQuote(proxyDeviceTargetEndpoint)
	out, gateway, err := shellFirstServiceGateway(ctx, env.runner, gateways, nil, command)
	if err != nil {
		return nil, fmt.Errorf("proxy device target: query Mimir: %w", err)
	}
	if len(out) > proxyDeviceTargetMaxBytes {
		return nil, fmt.Errorf("proxy device target: Mimir response too large")
	}
	processes, err := parseProxyDeviceTargetResponse(out, now, expected)
	if err != nil {
		return nil, fmt.Errorf("proxy device target: response from %s: %w", gateway.name, err)
	}
	return assessProxyDeviceTargets(processes, expected, now), nil
}

func parseProxyDeviceTargetResponse(out string, now time.Time, expected map[string]string) (map[string]*proxyDeviceTargetProcess, error) {
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, err
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("Mimir status=%q type=%q error=%q", response.Status, response.Data.ResultType, response.Error)
	}
	if len(response.Data.Result) > proxyDeviceTargetMaxSeries {
		return nil, fmt.Errorf("Mimir returned %d series", len(response.Data.Result))
	}
	allowed := map[string]bool{}
	for _, metric := range proxyDeviceTargetMetrics {
		allowed[metric] = true
		allowed[metric+proxyDeviceTargetSource] = true
	}
	allowed[proxyDeviceLegacyBudget] = true
	allowed[proxyDeviceLegacyBudget+proxyDeviceTargetSource] = true
	processes := map[string]*proxyDeviceTargetProcess{}
	for _, series := range response.Data.Result {
		metric := series.Metric["monitor_metric"]
		if !allowed[metric] {
			return nil, fmt.Errorf("unexpected metric %q", metric)
		}
		host, block, instance := series.Metric["host"], series.Metric["block"], series.Metric["instance"]
		if _, ok := expected[host+"\x00"+block]; !ok {
			continue
		}
		if instance == "" {
			return nil, fmt.Errorf("missing instance on %s/%s", host, block)
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, err
		}
		if observedAt.Before(now.Add(-30*time.Second)) || observedAt.After(now.Add(30*time.Second)) || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid pinned sample for %s/%s", host, block)
		}
		key := host + "\x00" + block + "\x00" + instance
		process := processes[key]
		if process == nil {
			process = &proxyDeviceTargetProcess{host: host, block: block, instance: instance, values: map[string]float64{}, sources: map[string]time.Time{}}
			processes[key] = process
		}
		if strings.HasSuffix(metric, proxyDeviceTargetSource) {
			metric = strings.TrimSuffix(metric, proxyDeviceTargetSource)
			if _, ok := process.sources[metric]; ok {
				return nil, fmt.Errorf("duplicate source for %s", metric)
			}
			process.sources[metric] = unixFloatTime(value)
		} else {
			if _, ok := process.values[metric]; ok {
				return nil, fmt.Errorf("duplicate value for %s", metric)
			}
			process.values[metric] = value
		}
	}
	return processes, nil
}

func assessProxyDeviceTargets(processes map[string]*proxyDeviceTargetProcess, expected map[string]string, now time.Time) []finding {
	current := map[string]*proxyDeviceTargetProcess{}
	for _, process := range processes {
		start, ok := proxyDeviceTargetValue(process, proxyDeviceTargetMetrics[0], now)
		if !ok || !proxyDeviceTargetFinitePositive(start) || start > float64(now.Add(30*time.Second).Unix()) {
			continue
		}
		slot := process.host + "\x00" + process.block
		prior := current[slot]
		if prior == nil || start > prior.values[proxyDeviceTargetMetrics[0]] || start == prior.values[proxyDeviceTargetMetrics[0]] && process.instance > prior.instance {
			current[slot] = process
		}
	}
	missing, invalid, pressure, shared := []string{}, []string{}, []string{}, []string{}
	for slot, label := range expected {
		process := current[slot]
		if process == nil {
			missing = append(missing, label+"[no-current-process]")
			continue
		}
		values := map[string]float64{}
		complete := true
		for _, metric := range proxyDeviceTargetMetrics {
			value, ok := proxyDeviceTargetValue(process, metric, now)
			if !ok {
				complete = false
				missing = append(missing, label+"["+metric+"]")
			}
			values[metric] = value
		}
		if !complete {
			continue
		}
		if budget, present := proxyDeviceTargetValue(process, proxyDeviceLegacyBudget, now); present && budget > 0 {
			shared = append(shared, fmt.Sprintf("%s[shared_admission_budget=%.0f]", label, budget))
		}
		devices := values[proxyDeviceTargetMetrics[1]]
		target := values[proxyDeviceTargetMetrics[2]]
		used := values[proxyDeviceTargetMetrics[3]]
		if !proxyDeviceTargetWholeNonnegative(devices) || !proxyDeviceTargetWholeNonnegative(target) || !proxyDeviceTargetWholeNonnegative(used) ||
			target != devices*proxyDeviceTargetBytes || devices == 0 && used != 0 {
			invalid = append(invalid, fmt.Sprintf("%s[devices=%.0f target=%.0f used=%.0f]", label, devices, target, used))
		} else if target > 0 && used > target {
			pressure = append(pressure, fmt.Sprintf("%s[devices=%.0f tracked_used=%.0f target=%.0f]", label, devices, used, target))
		}
	}
	sort.Strings(missing)
	sort.Strings(invalid)
	sort.Strings(pressure)
	sort.Strings(shared)
	findings := []finding{}
	if len(missing) > 0 {
		findings = append(findings, proxyDeviceTargetFinding("proxy-device-target-unobservable", "proxy-fleet", "Fresh per-device target telemetry is incomplete", strings.Join(missing, ";")))
	}
	if len(invalid) > 0 {
		findings = append(findings, proxyDeviceTargetFinding("proxy-device-target-mismatch", "proxy-fleet", "Proxy devices do not each report a 24 MiB target", strings.Join(invalid, ";")))
	}
	if len(pressure) > 0 {
		findings = append(findings, proxyDeviceTargetFinding("proxy-device-target-pressure", "proxy-fleet", "Tracked private device use exceeds the sum of targets", strings.Join(pressure, ";")))
	}
	if len(shared) > 0 {
		findings = append(findings, proxyDeviceTargetFinding("proxy-device-shared-admission", "proxy-fleet", "A current proxy process still enforces a shared device admission budget", strings.Join(shared, ";")))
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("runtime/proxy-device-target", tierWarn, "proxy-device-target-mismatch", "proxy-fleet")}
	}
	return findings
}

func proxyDeviceTargetValue(process *proxyDeviceTargetProcess, metric string, now time.Time) (float64, bool) {
	value, hasValue := process.values[metric]
	source, hasSource := process.sources[metric]
	return value, hasValue && hasSource && !source.Before(now.Add(-proxyDeviceTargetFreshness)) && !source.After(now.Add(30*time.Second))
}

func proxyDeviceTargetWholeNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && math.Trunc(value) == value
}

func proxyDeviceTargetFinitePositive(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}

func proxyDeviceTargetFinding(class, frame, symptom, observed string) finding {
	result := finding{
		probeId: "runtime/proxy-device-target", tier: tierWarn, class: class, target: "proxy-fleet", frame: frame, sustain: 1,
		symptom:   symptom,
		mechanism: "Proxy DeviceLocal memory targets are private; a missing, inconsistent, or over-target sample needs source and host-memory correlation, not a process-wide admission change.",
		baseline:  "Every current Proxy device has its own 24 MiB target, so target bytes equal live devices times 24 MiB in every process generation.",
		observed:  observed,
		context:   "Tracked use is not total RSS and 24 MiB is a steady-state target, not a hard per-device heap limit. Host reserve, OOM, and UDP loss determine hardware pressure.",
		action:    "Verify the running artifact and metric freshness, then compare §14.7 host RSS, MemAvailable, OOM, and UDP-loss evidence. Add hardware or rebalance if physical reserve is insufficient.",
		verify:    "Current process generations report coherent private targets and §14.7 host reserve remains healthy under ordinary acceptance.",
		playbook:  "SIGNALS.md §14.7d",
	}
	if class == "proxy-device-shared-admission" {
		result.mechanism = "The current Proxy image still exports a positive process-wide device admission ceiling; this can refuse an otherwise valid device despite its private 24 MiB target."
		result.action = "Verify the running Proxy artifact and complete the rollout of the per-device admission fix. Do not raise the retired process ceiling as a substitute."
		result.verify = "No current Proxy process exports a positive retired shared-admission ceiling, and independent device opens succeed under ordinary load."
	}
	return result
}
