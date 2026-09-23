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

// SIGNALS.md §23.1 (connect-h1plus): source enablement is not live use.
func NewConnectH1PlusSignal() Signal {
	return &signalAdapter{number: "23.1", key: "connect-h1plus", name: "Connect H1+ carrier activity", probe: h1PlusProbe{service: "connect", protocol: "urnetwork-framer/1"}}
}

// Both carriers are passive observations; never manufacture upgrade traffic.
type h1PlusProbe struct {
	service  string
	protocol string
}

func (self h1PlusProbe) id() string             { return "observability/" + self.service + "-h1plus" }
func (self h1PlusProbe) tier() string           { return tierWarn }
func (self h1PlusProbe) cadence() time.Duration { return 5 * time.Minute }

const (
	h1PlusWindow         = "15m"
	h1PlusWindowDuration = 15 * time.Minute
	h1PlusFreshness      = 90 * time.Second
	// Eight fields × value/source-time × two bounds, at most four rollout
	// generations per desired slot. 1 KiB/row is a response budget, not a
	// label-size assumption: longer rows or more generations fail visibility.
	h1PlusSamplesPerProcess = 32
	h1PlusGenerations       = 4
	h1PlusMaxSlots          = 64
	h1PlusRowBytes          = 1024
)

var h1PlusObservedFields = []string{"attempts", "accepted", "messages", "bytes", "auth_failures", "handshake_failures", "fallbacks"}

type h1PlusIdentity struct{ host, block, instance string }
type h1PlusSample struct {
	value, sourceTime   float64
	valueSeen, timeSeen bool
}
type h1PlusProcess struct{ samples map[string]h1PlusSample }
type h1PlusBudget struct{ samples, bytes int }

// Keep producer times and identity on every field. No sum/increase before
// validating current-generation ownership; absence is never vector(0).
func h1PlusQuery(environment string, self h1PlusProbe) string {
	parts := []string{}
	for _, field := range append([]string{"start"}, h1PlusObservedFields...) {
		selector := fmt.Sprintf("process_start_time_seconds{env=%s,job=%s}", strconv.Quote(environment), strconv.Quote(self.service))
		if field != "start" {
			selector = fmt.Sprintf("urnetwork_%s_h1plus_%s_total{env=%s,job=%s,protocol=%s}", self.service, field, strconv.Quote(environment), strconv.Quote(self.service), strconv.Quote(self.protocol))
		}
		for _, bound := range []string{"now", "prior"} {
			value := selector
			if bound == "prior" {
				value += " offset " + h1PlusWindow
			}
			for _, sourceTime := range []bool{false, true} {
				key := bound + "/" + field
				expression := value
				if sourceTime {
					expression = "timestamp(" + value + ")"
					key += "/time"
				}
				parts = append(parts, "label_replace("+expression+",\"monitor_h1plus\","+strconv.Quote(key)+",\"\",\"\")")
			}
		}
	}
	return strings.Join(parts, " or ")
}

func (self h1PlusProbe) base(target string) finding {
	playbook := "SIGNALS.md §23.1"
	if self.service == "proxy" {
		playbook = "SIGNALS.md §23.2"
	}
	return finding{
		probeId: self.id(), tier: tierWarn, target: target, frame: self.protocol, sustain: 2,
		baseline: "Every configured host/block slot has a fresh, complete same-process window; positive payload is a block-local use witness, not a fleet uptake percentage.",
		context:  "Eligible demand is native H1 selection: successful H3 traffic need not attempt H1+. Browser/JS and configured HTTP-proxy clients use WebSocket. These counters record successful server-side framed writes, not reads, peer receipt, client live connections, or customer-route success. Zero traffic is unverified, not a feature failure. Current process selection does not attest artifact ancestry.",
		playbook: playbook,
	}
}

func (self h1PlusProbe) unavailable(reason string) []finding {
	base := self.base(self.service + "-inventory")
	base.class = "h1plus-telemetry-incomplete"
	base.symptom = "H1+ coverage and activity are unverified"
	base.mechanism = "Desired slot inventory or the bounded observation is unavailable; no zero or healthy replacement is inferred."
	base.observed = "visibility=unknown reason=" + reason
	base.action = "Restore authoritative active service placement and complete bounded metrics; check running artifact ancestry independently."
	base.verify = "Each desired slot has a fresh, complete current-process observation at both window bounds."
	return []finding{base}
}

func (self h1PlusProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts, blocks := env.cfg.logServiceHosts[self.service], env.cfg.logServiceBlocks[self.service]
	if len(hosts) == 0 || len(blocks) == 0 || len(hosts) > h1PlusMaxSlots || len(blocks) > h1PlusMaxSlots/len(hosts) {
		return self.unavailable("slot-inventory-unavailable-or-over-bound"), nil
	}
	// Desired topology is not reduced by operational host exclusions. Whole
	// environment metrics are permitted, but excluded slots remain unknown.
	excluded := map[string]bool{}
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		for _, name := range scoped.excludedNames() {
			excluded[name] = true
		}
	}
	configured := map[string]bool{}
	for _, h := range env.cfg.scopeHosts() {
		configured[h.name] = true
		excluded[h.name] = excluded[h.name] || h.disabled
	}
	budget := h1PlusBudget{samples: len(hosts) * len(blocks) * h1PlusGenerations * h1PlusSamplesPerProcess}
	budget.bytes = max(65536, budget.samples*h1PlusRowBytes)
	now := env.now()
	var gateway *host
	for _, candidate := range env.cfg.hostsWithRole("services") {
		if !excluded[candidate.name] {
			gateway = candidate
			break
		}
	}
	if gateway == nil {
		return self.unavailable("metrics-gateway-unavailable"), nil
	}
	// POST follows the existing Mimir transport: the bounded multi-field
	// expression must not depend on an HTTP request-line size limit. Exactly
	// one gateway attempt; no retry that changes the source window or budget.
	out, err := env.runner.shell(ctx, gateway,
		"curl -fsS --max-time 15 --max-filesize "+strconv.Itoa(budget.bytes)+
			" --data-urlencode "+shellSingleQuote("query="+h1PlusQuery(env.cfg.env, self))+
			" --data-urlencode "+shellSingleQuote("time="+strconv.FormatInt(now.Unix(), 10))+
			" 'http://127.0.0.1:3100/prometheus/api/v1/query'")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return self.unavailable("bounded-source-unavailable"), nil
	}
	processes, err := parseH1PlusProcesses(out, env.cfg.env, self, now, budget)
	if err != nil {
		return self.unavailable("invalid-or-incomplete-source-response"), nil
	}
	return self.evaluate(now, hosts, blocks, configured, excluded, processes), nil
}

// Errors are fixed text: do not render response bodies, identities or labels.
func parseH1PlusProcesses(raw, environment string, self h1PlusProbe, now time.Time, budget h1PlusBudget) (map[h1PlusIdentity]*h1PlusProcess, error) {
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if len(raw) > budget.bytes || json.Unmarshal([]byte(raw), &response) != nil ||
		response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 || len(response.Infos) != 0 ||
		len(response.Data.Result) > budget.samples {
		return nil, fmt.Errorf("invalid bounded H1+ response")
	}
	processes := map[h1PlusIdentity]*h1PlusProcess{}
	for _, series := range response.Data.Result {
		identity := h1PlusIdentity{host: series.Metric["host"], block: series.Metric["block"], instance: series.Metric["instance"]}
		key := series.Metric["monitor_h1plus"]
		isTime := strings.HasSuffix(key, "/time")
		key = strings.TrimSuffix(key, "/time")
		parts := strings.Split(key, "/")
		known := len(parts) == 2 && (parts[0] == "now" || parts[0] == "prior")
		field := ""
		if known {
			field = parts[1]
			known = field == "start"
			for _, candidate := range h1PlusObservedFields {
				known = known || field == candidate
			}
		}
		at, value, err := mimirInstantValue(series.Value)
		if !known || series.Metric["env"] != environment || series.Metric["job"] != self.service ||
			(field != "start" && series.Metric["protocol"] != self.protocol) ||
			identity.host == "" || identity.block == "" || identity.instance == "" ||
			err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 ||
			now.Sub(at) > h1PlusFreshness || at.Sub(now) > 30*time.Second {
			return nil, fmt.Errorf("invalid H1+ sample identity or value")
		}
		process := processes[identity]
		if process == nil {
			process = &h1PlusProcess{samples: map[string]h1PlusSample{}}
			processes[identity] = process
		}
		sample := process.samples[key]
		if isTime {
			if sample.timeSeen {
				return nil, fmt.Errorf("duplicate H1+ source timestamp")
			}
			sample.sourceTime, sample.timeSeen = value, true
		} else {
			if sample.valueSeen {
				return nil, fmt.Errorf("duplicate H1+ value")
			}
			sample.value, sample.valueSeen = value, true
		}
		process.samples[key] = sample
	}
	return processes, nil
}

func h1PlusFresh(sample h1PlusSample, bound time.Time) bool {
	return sample.valueSeen && sample.timeSeen && sample.sourceTime > 0 &&
		float64(bound.UnixNano())/1e9-sample.sourceTime <= h1PlusFreshness.Seconds() &&
		sample.sourceTime-float64(bound.UnixNano())/1e9 <= 30
}

// Select before checking counter completeness. Never substitute a complete
// draining process for a newer incomplete one; equal starts are ambiguous.
func h1PlusCurrent(now time.Time, candidates []*h1PlusProcess) (*h1PlusProcess, string) {
	var current *h1PlusProcess
	for _, process := range candidates {
		start := process.samples["now/start"]
		if !start.valueSeen && !start.timeSeen {
			for key := range process.samples {
				if strings.HasPrefix(key, "now/") {
					return nil, "incomplete"
				}
			}
			continue // prior-only retired generation
		}
		if !start.valueSeen || !start.timeSeen || start.value <= 0 || start.value > start.sourceTime {
			return nil, "incomplete"
		}
		if current == nil || start.value > current.samples["now/start"].value {
			current = process
		}
	}
	if current == nil {
		return nil, "missing"
	}
	for _, process := range candidates {
		if process != current &&
			process.samples["now/start"].value == current.samples["now/start"].value {
			return nil, "ambiguous"
		}
	}
	if !h1PlusFresh(current.samples["now/start"], now) {
		return nil, "stale"
	}
	return current, ""
}

func h1PlusWindowDeltas(now time.Time, process *h1PlusProcess) (map[string]float64, string) {
	start, priorStart := process.samples["now/start"], process.samples["prior/start"]
	hasCollector := false
	for _, field := range h1PlusObservedFields {
		sample := process.samples["now/"+field]
		hasCollector = hasCollector || sample.valueSeen || sample.timeSeen
	}
	if !hasCollector {
		return nil, "missing"
	}
	if !priorStart.valueSeen || !priorStart.timeSeen || start.value > float64(now.Add(-h1PlusWindowDuration).UnixNano())/1e9 {
		return nil, "warming"
	}
	if !h1PlusFresh(priorStart, now.Add(-h1PlusWindowDuration)) {
		return nil, "stale"
	}
	if priorStart.value <= 0 || priorStart.value > priorStart.sourceTime || priorStart.value != start.value || start.sourceTime <= priorStart.sourceTime {
		return nil, "reset"
	}
	deltas := map[string]float64{}
	for _, field := range h1PlusObservedFields {
		current, prior := process.samples["now/"+field], process.samples["prior/"+field]
		if !current.valueSeen || !current.timeSeen || !prior.valueSeen || !prior.timeSeen {
			return nil, "incomplete"
		}
		if !h1PlusFresh(current, now) || !h1PlusFresh(prior, now.Add(-h1PlusWindowDuration)) {
			return nil, "stale"
		}
		if current.sourceTime != start.sourceTime || prior.sourceTime != priorStart.sourceTime {
			return nil, "incoherent"
		}
		if current.value < prior.value {
			return nil, "reset"
		}
		deltas[field] = current.value - prior.value
	}
	// Snapshot loads independent atomics. An attempts increment may precede
	// one scrape while its accepted increment follows it: do not require
	// cross-counter delta inequalities or pretend these are outcome ratios.
	return deltas, ""
}

func (self h1PlusProbe) evaluate(now time.Time, hosts, blocks []string, configured, excluded map[string]bool, processes map[h1PlusIdentity]*h1PlusProcess) []finding {
	bySlot := map[string][]*h1PlusProcess{}
	for identity, process := range processes {
		key := identity.host + "\x00" + identity.block
		bySlot[key] = append(bySlot[key], process)
	}
	blocks = append([]string(nil), blocks...)
	sort.Strings(blocks)
	result := []finding{}
	for _, block := range blocks {
		reasons := map[string]int{}
		complete, active := 0, 0
		totals := map[string]float64{}
		for _, name := range hosts {
			if excluded[name] {
				reasons["excluded"]++
				continue
			}
			if !configured[name] {
				reasons["inventory_unknown"]++
				continue
			}
			current, reason := h1PlusCurrent(now, bySlot[name+"\x00"+block])
			if reason != "" {
				reasons[reason]++
				continue
			}
			deltas, reason := h1PlusWindowDeltas(now, current)
			if reason != "" {
				reasons[reason]++
				continue
			}
			complete++
			for field, value := range deltas {
				totals[field] += value
			}
			// A long-lived authenticated stream need not negotiate in this
			// window. Empty heartbeat messages carry zero payload bytes.
			if current.samples["now/accepted"].value > 0 && deltas["messages"] > 0 && deltas["bytes"] > 0 {
				active++
			}
		}
		base := self.base(self.service + "/" + block)
		base.observed = fmt.Sprintf("window=%s expected_slots=%d complete_slots=%d payload_active_slots=%d", h1PlusWindow, len(hosts), complete, active)
		for _, reason := range []string{"missing", "stale", "incomplete", "ambiguous", "warming", "reset", "incoherent", "excluded", "inventory_unknown"} {
			base.observed += fmt.Sprintf(" %s_slots=%d", reason, reasons[reason])
		}
		for _, field := range h1PlusObservedFields {
			base.observed += fmt.Sprintf(" paired_%s_delta=%.0f", field, totals[field])
		}
		if complete != len(hosts) {
			base.class = "h1plus-telemetry-incomplete"
			if reasons["missing"] == len(hosts) {
				base.class = "h1plus-telemetry-missing"
			} else if reasons["stale"] == len(hosts) {
				base.class = "h1plus-telemetry-stale"
			}
			base.symptom = "H1+ current-slot coverage is incomplete; support and activity are unverified"
			base.mechanism = "A healthy sibling, draining generation, or fresh evaluation timestamp cannot certify a missing, stale, excluded, restarted or ambiguously owned slot."
			base.action = "Restore each desired slot's exact-protocol counters and producer timestamps; retain unknown during rollout warmup and verify artifact ancestry separately."
			base.verify = "Every configured slot has coherent, fresh, monotonic counters paired across the same stable process start."
		} else if active == 0 {
			base.class = "h1plus-activity-unverified"
			base.symptom = "No confirmed H1+ server-write payload in this block's current-process window"
			base.mechanism = "Eligible traffic may be idle, receive-only, opted out, disabled, using WebSocket or rejected. Server attempts omit some early rejections, and server fallbacks are not client WebSocket selection; neither supplies a native-demand denominator."
			base.action = "Correlate eligible native demand, running versions, enable/kill switches, authorization and ingress Upgrade forwarding; do not infer failure or zero client fallback."
			base.verify = "On one current process observe accepted lifetime count >0 and positive paired messages plus payload bytes; a fresh upgrade within this window is not required."
		} else {
			healthy := healthyFinding(self.id(), tierWarn, "h1plus-activity-unverified", base.target)
			healthy.frame = self.protocol
			result = append(result, healthy)
			continue
		}
		result = append(result, base)
	}
	return result
}
