// Mimir distributor balance is reduced on each host before any metric labels
// or socket peers can leave that host.
package monitor

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mimirBalanceMarker       = "monitor-signal-11.20b-mimir-balance"
	mimirBalanceStateVersion = 1
	mimirBalanceHistoryLimit = 1024
	mimirBalanceMaximumGap   = 3 * time.Minute
	mimirBalanceFrontPort    = 3100
)

// Signal mimir-balance implements SIGNALS.md §11.20b. It distinguishes a
// genuinely full tenant budget from one overloaded distributor caused by
// persistent remote-write connection skew.
func NewMimirBalanceSignal() Signal {
	return &signalAdapter{
		number: "11.20b", key: "mimir-balance", name: "Mimir distributor ingestion balance",
		probe: &mimirBalanceProbe{},
	}
}

type mimirBalanceProbe struct {
	cycleLock sync.Mutex
}

func (*mimirBalanceProbe) id() string             { return "observability/mimir-balance" }
func (*mimirBalanceProbe) tier() string           { return tierWarn }
func (*mimirBalanceProbe) cadence() time.Duration { return time.Minute }

type mimirBalanceIdentity struct {
	host         string
	port         int
	processStart string
}

type mimirBalanceHistory struct {
	identity        mimirBalanceIdentity
	observedUnixNS  int64
	samplesInTotal  int64
	receivedTotal   int64
	requestsInTotal int64
}

type mimirBalancePersistedState struct {
	Histories []mimirBalancePersistedHistory `json:"histories"`
}

type mimirBalancePersistedHistory struct {
	Host            string `json:"host"`
	Port            int    `json:"port"`
	ProcessStart    string `json:"process_start"`
	ObservedUnixNS  int64  `json:"observed_unix_ns"`
	SamplesInTotal  int64  `json:"samples_in_total"`
	ReceivedTotal   int64  `json:"received_total"`
	RequestsInTotal int64  `json:"requests_in_total"`
}

type mimirBalanceInstance struct {
	port               int
	observable         bool
	observableSeen     bool
	processStart       string
	samplesInTotal     int64
	receivedTotal      int64
	requestsInTotal    int64
	activeDistributors int64
	ingestionRateLimit float64
	seen               map[string]bool
}

type mimirBalanceHostSample struct {
	instances         []mimirBalanceInstance
	count             int
	localConnections  int64
	remoteConnections int64
}

type mimirBalanceHostResult struct {
	host   *host
	sample mimirBalanceHostSample
	err    error
}

type mimirBalanceRate struct {
	host       string
	attempted  float64
	accepted   float64
	requests   float64
	effective  float64
	overloaded bool
}

type mimirBalanceAssessment struct {
	configuredHosts             int
	observableHosts             int
	instanceCount               int
	comparableInstances         int
	generationChanges           int
	counterResets               int
	directComplete              bool
	rateComplete                bool
	activeDistributorsMinimum   int64
	activeDistributorsMaximum   int64
	ingestionRateMinimum        float64
	ingestionRateMaximum        float64
	effectiveRateMinimum        float64
	effectiveRateMaximum        float64
	fleetAttemptedRate          float64
	fleetAcceptedRate           float64
	fleetRequestRate            float64
	childAttemptedMinimum       float64
	childAttemptedMaximum       float64
	overloadedInstances         int
	localConnections            int64
	remoteConnections           int64
	overloadedRemoteConnections int64
	visibilityFailures          []mimirAdmissionVisibilityFailure
}

func (self *mimirBalanceProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("mimir balance: no services hosts in inventory")
	}

	self.cycleLock.Lock()
	defer self.cycleLock.Unlock()
	stateLock, err := lockProviderState(ctx, env.cfg.stateDir, "mimir-balance")
	if err != nil {
		return nil, fmt.Errorf("mimir balance state lock: %w", err)
	}
	defer stateLock.Close()

	state := mimirBalancePersistedState{}
	if _, err := loadProviderState(env.cfg.stateDir, "mimir-balance", mimirBalanceStateVersion, &state); err != nil {
		return nil, fmt.Errorf("mimir balance state: %w", err)
	}
	if err := validateMimirBalanceState(state); err != nil {
		return nil, err
	}

	command := mimirBalanceCommand()
	resultValues := make(chan mimirBalanceHostResult, len(hosts))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, configuredHost := range hosts {
		target := configuredHost
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				resultValues <- mimirBalanceHostResult{host: target, err: ctx.Err()}
				return
			}
			output, err := env.runner.shell(ctx, target, command)
			if err != nil {
				resultValues <- mimirBalanceHostResult{host: target, err: err}
				return
			}
			sample, err := parseMimirBalanceHostSample(output)
			resultValues <- mimirBalanceHostResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(resultValues)

	results := make([]mimirBalanceHostResult, 0, len(hosts))
	for result := range resultValues {
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].host.name < results[j].host.name })

	assessment, next := assessMimirBalance(env.now().UTC(), state, results)
	if err := saveProviderState(env.cfg.stateDir, "mimir-balance", mimirBalanceStateVersion, next); err != nil {
		return nil, fmt.Errorf("mimir balance state save: %w", err)
	}

	findings := make([]finding, 0, len(assessment.visibilityFailures)+1)
	seenVisibility := map[string]bool{}
	for _, failure := range assessment.visibilityFailures {
		if seenVisibility[failure.target] {
			continue
		}
		seenVisibility[failure.target] = true
		findings = append(findings, cannotObserveFinding(failure.target, failure.err))
	}
	if mimirBalanceIsSkewed(assessment) {
		findings = append(findings, mimirBalanceFinding(assessment))
	} else if assessment.directComplete && assessment.rateComplete {
		findings = append(findings, healthyFinding(
			"observability/mimir-balance", tierWarn, "mimir-distributor-skew", "mimir-fleet",
		))
	}
	return findings, nil
}

func validateMimirBalanceState(state mimirBalancePersistedState) error {
	if len(state.Histories) > mimirBalanceHistoryLimit {
		return fmt.Errorf("mimir balance state has too many histories")
	}
	seen := map[mimirBalanceIdentity]bool{}
	for _, history := range state.Histories {
		identity := mimirBalanceIdentity{host: history.Host, port: history.Port, processStart: history.ProcessStart}
		processStart, ok := new(big.Rat).SetString(history.ProcessStart)
		if strings.TrimSpace(history.Host) == "" || history.Port < 1 || 65535 < history.Port ||
			!ok || processStart.Sign() <= 0 || processStart.RatString() != history.ProcessStart ||
			history.ObservedUnixNS <= 0 || history.SamplesInTotal < 0 ||
			history.ReceivedTotal < 0 || history.RequestsInTotal < 0 {
			return fmt.Errorf("mimir balance state has an invalid history")
		}
		if seen[identity] {
			return fmt.Errorf("mimir balance state repeats a history")
		}
		seen[identity] = true
	}
	return nil
}

func mimirBalanceStateMap(state mimirBalancePersistedState) map[mimirBalanceIdentity]mimirBalanceHistory {
	histories := make(map[mimirBalanceIdentity]mimirBalanceHistory, len(state.Histories))
	for _, persisted := range state.Histories {
		identity := mimirBalanceIdentity{host: persisted.Host, port: persisted.Port, processStart: persisted.ProcessStart}
		histories[identity] = mimirBalanceHistory{
			identity: identity, observedUnixNS: persisted.ObservedUnixNS,
			samplesInTotal: persisted.SamplesInTotal, receivedTotal: persisted.ReceivedTotal,
			requestsInTotal: persisted.RequestsInTotal,
		}
	}
	return histories
}

func mimirBalancePersisted(histories map[mimirBalanceIdentity]mimirBalanceHistory) mimirBalancePersistedState {
	state := mimirBalancePersistedState{Histories: make([]mimirBalancePersistedHistory, 0, len(histories))}
	for _, history := range histories {
		state.Histories = append(state.Histories, mimirBalancePersistedHistory{
			Host: history.identity.host, Port: history.identity.port, ProcessStart: history.identity.processStart,
			ObservedUnixNS: history.observedUnixNS, SamplesInTotal: history.samplesInTotal,
			ReceivedTotal: history.receivedTotal, RequestsInTotal: history.requestsInTotal,
		})
	}
	sort.Slice(state.Histories, func(i, j int) bool {
		if state.Histories[i].Host != state.Histories[j].Host {
			return state.Histories[i].Host < state.Histories[j].Host
		}
		if state.Histories[i].Port != state.Histories[j].Port {
			return state.Histories[i].Port < state.Histories[j].Port
		}
		return state.Histories[i].ProcessStart < state.Histories[j].ProcessStart
	})
	return state
}

func assessMimirBalance(
	now time.Time,
	state mimirBalancePersistedState,
	results []mimirBalanceHostResult,
) (mimirBalanceAssessment, mimirBalancePersistedState) {
	prior := mimirBalanceStateMap(state)
	next := map[mimirBalanceIdentity]mimirBalanceHistory{}
	assessment := mimirBalanceAssessment{configuredHosts: len(results), directComplete: true, rateComplete: true}
	rates := []mimirBalanceRate{}
	hostOverloaded := map[string]bool{}
	rangeInitialized := false

	for _, result := range results {
		hostName := result.host.name
		preserveHost := func() {
			for identity, history := range prior {
				if identity.host == hostName {
					next[identity] = history
				}
			}
		}
		if result.err != nil {
			assessment.directComplete = false
			assessment.rateComplete = false
			preserveHost()
			assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
				target: hostName + "/mimir-balance", err: fmt.Errorf("host observation failed"),
			})
			continue
		}
		assessment.localConnections += result.sample.localConnections
		assessment.remoteConnections += result.sample.remoteConnections
		if result.sample.count == 0 || len(result.sample.instances) == 0 {
			assessment.directComplete = false
			assessment.rateComplete = false
			preserveHost()
			assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
				target: hostName + "/mimir-balance", err: fmt.Errorf("no local Mimir child was identified"),
			})
			continue
		}

		hostComplete := true
		assessment.instanceCount += len(result.sample.instances)
		for _, instance := range result.sample.instances {
			if !instance.observable {
				hostComplete = false
				assessment.directComplete = false
				assessment.rateComplete = false
				assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
					target: hostName + "/mimir-balance", err: fmt.Errorf("a local Mimir child omitted its bounded observation"),
				})
				continue
			}

			if !rangeInitialized {
				assessment.activeDistributorsMinimum = instance.activeDistributors
				assessment.activeDistributorsMaximum = instance.activeDistributors
				assessment.ingestionRateMinimum = instance.ingestionRateLimit
				assessment.ingestionRateMaximum = instance.ingestionRateLimit
				rangeInitialized = true
			} else {
				assessment.activeDistributorsMinimum = min(assessment.activeDistributorsMinimum, instance.activeDistributors)
				assessment.activeDistributorsMaximum = max(assessment.activeDistributorsMaximum, instance.activeDistributors)
				assessment.ingestionRateMinimum = min(assessment.ingestionRateMinimum, instance.ingestionRateLimit)
				assessment.ingestionRateMaximum = max(assessment.ingestionRateMaximum, instance.ingestionRateLimit)
			}

			identity := mimirBalanceIdentity{host: hostName, port: instance.port, processStart: instance.processStart}
			current := mimirBalanceHistory{
				identity: identity, observedUnixNS: now.UnixNano(), samplesInTotal: instance.samplesInTotal,
				receivedTotal: instance.receivedTotal, requestsInTotal: instance.requestsInTotal,
			}
			previous, observedBefore := prior[identity]
			next[identity] = current
			if !observedBefore {
				assessment.rateComplete = false
				for previousIdentity := range prior {
					if previousIdentity.host == hostName {
						assessment.generationChanges++
						break
					}
				}
				continue
			}
			if instance.samplesInTotal < previous.samplesInTotal || instance.receivedTotal < previous.receivedTotal ||
				instance.requestsInTotal < previous.requestsInTotal {
				assessment.rateComplete = false
				assessment.counterResets++
				assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
					target: hostName + "/mimir-balance", err: fmt.Errorf("a monotonic distributor counter decreased"),
				})
				continue
			}
			elapsed := time.Duration(now.UnixNano() - previous.observedUnixNS)
			if elapsed <= 0 || mimirBalanceMaximumGap < elapsed {
				assessment.rateComplete = false
				assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
					target: hostName + "/mimir-balance", err: fmt.Errorf("the live counter comparison interval is unavailable"),
				})
				continue
			}
			seconds := elapsed.Seconds()
			effective := instance.ingestionRateLimit / float64(instance.activeDistributors)
			rate := mimirBalanceRate{
				host:      hostName,
				attempted: float64(instance.samplesInTotal-previous.samplesInTotal) / seconds,
				accepted:  float64(instance.receivedTotal-previous.receivedTotal) / seconds,
				requests:  float64(instance.requestsInTotal-previous.requestsInTotal) / seconds,
				effective: effective,
			}
			rate.overloaded = effective < rate.attempted
			rates = append(rates, rate)
			assessment.comparableInstances++
		}
		if hostComplete {
			assessment.observableHosts++
		} else {
			preserveHost()
		}
	}

	if len(next) > mimirBalanceHistoryLimit {
		assessment.directComplete = false
		assessment.rateComplete = false
		assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
			target: "mimir-balance/state", err: fmt.Errorf("current child identity bound exceeded"),
		})
		return assessment, state
	}
	if assessment.directComplete && len(rates) == assessment.instanceCount &&
		(assessment.activeDistributorsMinimum != assessment.activeDistributorsMaximum ||
			assessment.activeDistributorsMinimum != int64(assessment.instanceCount) ||
			math.Abs(assessment.ingestionRateMaximum-assessment.ingestionRateMinimum) > 0.001) {
		assessment.visibilityFailures = append(assessment.visibilityFailures, mimirAdmissionVisibilityFailure{
			target: "mimir-fleet/mimir-balance", err: fmt.Errorf("the distributor ring or rate view is inconsistent"),
		})
	}
	if !assessment.directComplete || len(rates) != assessment.instanceCount ||
		assessment.activeDistributorsMinimum != assessment.activeDistributorsMaximum ||
		assessment.activeDistributorsMinimum != int64(assessment.instanceCount) ||
		math.Abs(assessment.ingestionRateMaximum-assessment.ingestionRateMinimum) > 0.001 {
		assessment.rateComplete = false
	}
	for index, rate := range rates {
		assessment.fleetAttemptedRate += rate.attempted
		assessment.fleetAcceptedRate += rate.accepted
		assessment.fleetRequestRate += rate.requests
		if index == 0 {
			assessment.childAttemptedMinimum = rate.attempted
			assessment.childAttemptedMaximum = rate.attempted
			assessment.effectiveRateMinimum = rate.effective
			assessment.effectiveRateMaximum = rate.effective
		} else {
			assessment.childAttemptedMinimum = min(assessment.childAttemptedMinimum, rate.attempted)
			assessment.childAttemptedMaximum = max(assessment.childAttemptedMaximum, rate.attempted)
			assessment.effectiveRateMinimum = min(assessment.effectiveRateMinimum, rate.effective)
			assessment.effectiveRateMaximum = max(assessment.effectiveRateMaximum, rate.effective)
		}
		if rate.overloaded {
			assessment.overloadedInstances++
			hostOverloaded[rate.host] = true
		}
	}
	for _, result := range results {
		if result.err == nil && hostOverloaded[result.host.name] {
			assessment.overloadedRemoteConnections += result.sample.remoteConnections
		}
	}
	return assessment, mimirBalancePersisted(next)
}

func mimirBalanceIsSkewed(assessment mimirBalanceAssessment) bool {
	return assessment.directComplete && assessment.rateComplete &&
		0 < assessment.overloadedInstances &&
		assessment.fleetAttemptedRate < assessment.ingestionRateMinimum
}

func mimirBalanceFinding(assessment mimirBalanceAssessment) finding {
	lostRate := max(0.0, assessment.fleetAttemptedRate-assessment.fleetAcceptedRate)
	return finding{
		probeId: "observability/mimir-balance", tier: tierWarn,
		class: "mimir-distributor-skew", target: "mimir-fleet", frame: "persistent-remote-write", sustain: 2,
		symptom:   "One or more Mimir distributors exceed their local share of the tenant ingestion rate while the fleet remains below the global budget.",
		mechanism: "Mimir's global ingestion strategy divides the tenant rate by the healthy distributor count and enforces that share in an independent token bucket on each child. Persistent remote-write connections concentrated on one front can therefore reject samples while idle siblings retain unused capacity; a multi-address hosts entry is failover, not request balancing.",
		baseline:  "The fleet attempted rate remains below the global tenant limit and every child remains below its effective limit; persistent non-loopback publisher connections and attempted rates are distributed so one child cannot drain its local bucket.",
		observed: fmt.Sprintf(
			"configured_hosts=%d observable_hosts=%d instances=%d comparable_instances=%d active_distributors=%d..%d fleet_attempted_samples_per_s=%.1f fleet_accepted_samples_per_s=%.1f inferred_unaccepted_samples_per_s=%.1f fleet_requests_per_s=%.2f global_ingestion_rate=%.1f..%.1f effective_child_rate=%.1f..%.1f child_attempted_rate=%.1f..%.1f overloaded_instances=%d local_front_connections=%d remote_front_connections=%d overloaded_remote_front_connections=%d generation_changes=%d counter_resets=%d",
			assessment.configuredHosts, assessment.observableHosts, assessment.instanceCount,
			assessment.comparableInstances, assessment.activeDistributorsMinimum,
			assessment.activeDistributorsMaximum, assessment.fleetAttemptedRate,
			assessment.fleetAcceptedRate, lostRate, assessment.fleetRequestRate,
			assessment.ingestionRateMinimum, assessment.ingestionRateMaximum,
			assessment.effectiveRateMinimum, assessment.effectiveRateMaximum,
			assessment.childAttemptedMinimum, assessment.childAttemptedMaximum,
			assessment.overloadedInstances, assessment.localConnections,
			assessment.remoteConnections, assessment.overloadedRemoteConnections,
			assessment.generationChanges, assessment.counterResets,
		),
		evidence: "Each enabled services host identifies its loopback Mimir child, reduces exact attempted/accepted/request counters and the active-distributor/rate controls locally, and returns only numeric aggregates. Socket peers are reduced to loopback and non-loopback connection counts on-host; no tenant label, peer address, rendered config, or raw metric line leaves the host.",
		context:  "The attempted-minus-accepted rate is an upper bound on admission loss because deduplication and other rejections can contribute; the exact rate_limited counter in §11.20a remains the loss authority. When fleet attempted load itself exceeds the global limit, this signal does not label the incident as balance-only: software optimization plus hardware or an explicit capacity change may be required.",
		action:   "Give each remote publisher a deterministic, distinct preferred enabled Grafana front while retaining the other fronts as failover, then reconnect those publishers in an approved rollout. Remove proven unnecessary metric families as an independent load reduction. Do not raise the global limit or restart Mimir to erase counters when the measured fleet still has unused capacity.",
		verify:   "For two consecutive one-minute balance samples, no child exceeds its effective share and remote front connections are no longer concentrated on the overloaded child. Then require §11.20a to observe all exact children with zero new rate_limited increments and fresh required metrics through its complete two-hour quiet window.",
		playbook: "SIGNALS.md §11.20b and §11.20a",
	}
}

func mimirBalanceCommand() string {
	return "# " + mimirBalanceMarker + "\n" +
		"loopback_address='127.0.0.1'\n" +
		fmt.Sprintf("front_port='%d'\n", mimirBalanceFrontPort) +
		mimirBalanceScriptBody
}

func parseMimirBalanceHostSample(output string) (mimirBalanceHostSample, error) {
	sample := mimirBalanceHostSample{}
	var current *mimirBalanceInstance
	seenTrailer := map[string]bool{}
	for lineNumber, raw := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(raw)
		if len(fields) != 2 {
			return sample, fmt.Errorf("mimir balance line %d: invalid field count", lineNumber+1)
		}
		key, value := fields[0], fields[1]
		if key == "instance_begin" {
			if current != nil {
				return sample, fmt.Errorf("mimir balance line %d: nested instance", lineNumber+1)
			}
			port, err := parseMimirBalanceInt(value)
			if err != nil || port < 1 || 65535 < port {
				return sample, fmt.Errorf("mimir balance line %d: invalid port", lineNumber+1)
			}
			current = &mimirBalanceInstance{port: int(port), seen: map[string]bool{}}
			continue
		}
		if key == "instance_end" {
			if current == nil || value != "1" {
				return sample, fmt.Errorf("mimir balance line %d: invalid instance end", lineNumber+1)
			}
			if !current.observableSeen {
				return sample, fmt.Errorf("mimir balance line %d: instance omitted observable", lineNumber+1)
			}
			if current.observable {
				for _, required := range []string{
					"process_start", "samples_in_total", "received_total", "requests_in_total",
					"active_distributors", "ingestion_rate_limit",
				} {
					if !current.seen[required] {
						return sample, fmt.Errorf("mimir balance line %d: incomplete observable instance", lineNumber+1)
					}
				}
			}
			if current.observable && (current.processStart == "" || current.activeDistributors <= 0 ||
				current.ingestionRateLimit <= 0) {
				return sample, fmt.Errorf("mimir balance line %d: incomplete observable instance", lineNumber+1)
			}
			if !current.observable && len(current.seen) != 1 {
				return sample, fmt.Errorf("mimir balance line %d: unobservable instance contains metric fields", lineNumber+1)
			}
			sample.instances = append(sample.instances, *current)
			current = nil
			continue
		}
		if current != nil {
			if current.seen[key] {
				return sample, fmt.Errorf("mimir balance line %d: duplicate instance field", lineNumber+1)
			}
			current.seen[key] = true
			switch key {
			case "observable":
				if value != "0" && value != "1" {
					return sample, fmt.Errorf("mimir balance line %d: invalid observable", lineNumber+1)
				}
				current.observable = value == "1"
				current.observableSeen = true
			case "process_start":
				canonical, err := canonicalMimirAdmissionProcessStart(value)
				if err != nil {
					return sample, fmt.Errorf("mimir balance line %d: invalid process start", lineNumber+1)
				}
				current.processStart = canonical
			case "samples_in_total":
				parsed, err := parseMimirBalanceInt(value)
				if err != nil {
					return sample, fmt.Errorf("mimir balance line %d: invalid samples total", lineNumber+1)
				}
				current.samplesInTotal = parsed
			case "received_total":
				parsed, err := parseMimirBalanceInt(value)
				if err != nil {
					return sample, fmt.Errorf("mimir balance line %d: invalid received total", lineNumber+1)
				}
				current.receivedTotal = parsed
			case "requests_in_total":
				parsed, err := parseMimirBalanceInt(value)
				if err != nil {
					return sample, fmt.Errorf("mimir balance line %d: invalid requests total", lineNumber+1)
				}
				current.requestsInTotal = parsed
			case "active_distributors":
				parsed, err := parseMimirBalanceInt(value)
				if err != nil {
					return sample, fmt.Errorf("mimir balance line %d: invalid active distributors", lineNumber+1)
				}
				current.activeDistributors = parsed
			case "ingestion_rate_limit":
				parsed, err := strconv.ParseFloat(value, 64)
				if err != nil || parsed <= 0 || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
					return sample, fmt.Errorf("mimir balance line %d: invalid ingestion rate", lineNumber+1)
				}
				current.ingestionRateLimit = parsed
			default:
				return sample, fmt.Errorf("mimir balance line %d: unknown instance field", lineNumber+1)
			}
			continue
		}
		if seenTrailer[key] {
			return sample, fmt.Errorf("mimir balance line %d: duplicate trailer field", lineNumber+1)
		}
		seenTrailer[key] = true
		parsed, err := parseMimirBalanceInt(value)
		if err != nil {
			return sample, fmt.Errorf("mimir balance line %d: invalid trailer value", lineNumber+1)
		}
		switch key {
		case "mimir_count":
			sample.count = int(parsed)
		case "front_local_connections":
			sample.localConnections = parsed
		case "front_remote_connections":
			sample.remoteConnections = parsed
		default:
			return sample, fmt.Errorf("mimir balance line %d: unknown trailer field", lineNumber+1)
		}
	}
	if current != nil {
		return sample, fmt.Errorf("mimir balance: unterminated instance")
	}
	for _, key := range []string{"mimir_count", "front_local_connections", "front_remote_connections"} {
		if !seenTrailer[key] {
			return sample, fmt.Errorf("mimir balance: missing %s", key)
		}
	}
	if sample.count != len(sample.instances) {
		return sample, fmt.Errorf("mimir balance: invalid instance count")
	}
	return sample, nil
}

func parseMimirBalanceInt(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid nonnegative integer")
	}
	return parsed, nil
}

const mimirBalanceScriptBody = `set -u
for required in ss curl awk sort; do
  if ! command -v "$required" >/dev/null 2>&1; then
    printf 'mimir balance prerequisite missing: %s\n' "$required" >&2
    exit 1
  fi
done
mimir_count=0
ports=$(ss -ltnH 2>/dev/null | awk -v address="$loopback_address" '
  index($4, address ":") == 1 {
    port=$4
    sub(/.*:/, "", port)
    if (port ~ /^[0-9]+$/) print port
  }
' | sort -n -u)
for port in $ports; do
  build_info=$(curl -fsS --max-time 2 "http://${loopback_address}:${port}/api/v1/status/buildinfo" 2>/dev/null || true)
  case "$build_info" in
    *'"application":"Grafana Mimir"'*) ;;
    *) continue ;;
  esac
  mimir_count=$((mimir_count+1))
  printf 'instance_begin %s\n' "$port"
  ingestion_rate_limit=$(curl -fsS --max-time 10 "http://${loopback_address}:${port}/config" 2>/dev/null | awk '
    function key(value) {sub(/:$/, "", value); return value}
    function leave(indent) {
      while (depth > 0 && indent <= path_indent[depth]) {
        delete path_key[depth]
        delete path_indent[depth]
        depth--
      }
    }
    {
      if ($0 ~ /^[ ]*$/ || $0 ~ /^[ ]*#/ || index($0, "\t")) next
      match($0, /[^ ]/)
      if (RSTART == 0) next
      indent=RSTART-1
      leave(indent)
      if ($1 !~ /:$/) next
      name=key($1)
      if (depth == 1 && path_key[1] == "limits" && name == "ingestion_rate") {
        count++
        value=$2
      }
      depth++
      path_indent[depth]=indent
      path_key[depth]=name
    }
    END {
      if (count == 1 && value ~ /^[0-9]+([.][0-9]+)?$/ && value > 0) print value
      else exit 41
    }
  ')
  config_status=$?
  metrics=$(curl -fsS --max-time 10 "http://${loopback_address}:${port}/metrics" 2>/dev/null)
  metrics_status=$?
  if [ "$config_status" -ne 0 ] || [ "$metrics_status" -ne 0 ]; then
    printf 'observable 0\ninstance_end 1\n'
    continue
  fi
  reduced=$(printf '%s\n' "$metrics" | awk '
    function numeric(value) {return value ~ /^[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$/}
    /^# HELP cortex_distributor_samples_in_total / {samples_help++}
    /^# TYPE cortex_distributor_samples_in_total counter$/ {samples_type++}
    /^# HELP cortex_distributor_received_samples_total / {received_help++}
    /^# TYPE cortex_distributor_received_samples_total counter$/ {received_type++}
    /^# HELP cortex_distributor_requests_in_total / {requests_help++}
    /^# TYPE cortex_distributor_requests_in_total counter$/ {requests_type++}
    /^process_start_time_seconds[ \t]/ && numeric($NF) {process_count++; process_start=$NF}
    /^cortex_distributor_samples_in_total([{ \t])/ {
      samples_rows++
      if (!numeric($NF)) invalid++
      else samples_total+=$NF
    }
    /^cortex_distributor_received_samples_total([{ \t])/ {
      received_rows++
      if (!numeric($NF)) invalid++
      else received_total+=$NF
    }
    /^cortex_distributor_requests_in_total([{ \t])/ {
      requests_rows++
      if (!numeric($NF)) invalid++
      else requests_total+=$NF
    }
    /^cortex_ring_members[{]/ &&
      $0 ~ /(^|[{,])name="distributor"([,}])/ &&
      $0 ~ /(^|[{,])state="ACTIVE"([,}])/ {
      active_rows++
      if (!numeric($NF)) invalid++
      else active_distributors=$NF
    }
    END {
      if (invalid || process_count != 1 || process_start <= 0 ||
          samples_help != 1 || samples_type != 1 || samples_rows < 1 ||
          received_help != 1 || received_type != 1 || received_rows < 1 ||
          requests_help != 1 || requests_type != 1 || requests_rows < 1 ||
          active_rows != 1 || active_distributors <= 0) exit 42
      printf "process_start %s\n", process_start
      printf "samples_in_total %.0f\n", samples_total
      printf "received_total %.0f\n", received_total
      printf "requests_in_total %.0f\n", requests_total
      printf "active_distributors %.0f\n", active_distributors
    }
  ')
  reduced_status=$?
  if [ "$reduced_status" -ne 0 ]; then
    printf 'observable 0\ninstance_end 1\n'
    continue
  fi
  printf 'observable 1\n%s\ningestion_rate_limit %s\ninstance_end 1\n' "$reduced" "$ingestion_rate_limit"
done
printf 'mimir_count %s\n' "$mimir_count"
connections=$(ss -tnH state established 2>/dev/null)
connections_status=$?
if [ "$connections_status" -ne 0 ]; then
  printf 'mimir balance socket reduction failed\n' >&2
  exit 1
fi
printf '%s\n' "$connections" | awk -v front_port="$front_port" '
  function endpoint_port(endpoint, value) {
    value=endpoint
    sub(/^.*:/, "", value)
    return value
  }
  function endpoint_host(endpoint, value) {
    value=endpoint
    sub(/:[^:]*$/, "", value)
    sub(/^\[/, "", value)
    sub(/\]$/, "", value)
    return value
  }
  endpoint_port($3) == front_port {
    peer=endpoint_host($4)
    if (peer == "127.0.0.1" || peer == "::1") local++
    else remote++
  }
  END {
    printf "front_local_connections %.0f\n", local
    printf "front_remote_connections %.0f\n", remote
  }
'
`
