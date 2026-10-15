package monitor

// SIGNALS.md §2.19e separates control-plane request pressure from provider exit
// failure. Mimir owns the five-minute history, so monitor restart cannot learn
// an active incident as a healthy baseline.

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
	controlRouteFreshness   = 90 * time.Second
	controlRouteResponseMax = 2 * 1024 * 1024
	controlRouteLabel       = "POST ^/connect/control$"
)

// The only automatic action is an alert; no request admission policy changes.
func NewControlRoutePressureSignal() Signal {
	return &signalAdapter{
		number: "2.19e", key: "control-route-pressure", name: "Control route pressure",
		probe: controlRoutePressureProbe{},
	}
}

type controlRoutePressureProbe struct{}

func (controlRoutePressureProbe) id() string             { return "runtime/control-route-pressure" }
func (controlRoutePressureProbe) tier() string           { return tierWarn }
func (controlRoutePressureProbe) cadence() time.Duration { return time.Minute }

var controlRoutePhases = [...]string{"prepare", "authenticate", "controller", "response"}
var controlRouteMessages = [...]string{
	"create_contract", "close_contract", "provide", "encrypted_key",
	"client_key", "control_ping", "provide_ping", "other",
}

// Exact process identity stays in the private join. Render host/block only.
type controlRouteProcess struct {
	host, block, instance string
	values                map[string]float64
	invalid               bool
}

// Aggregate only the configured route and bounded diagnostics at the server.
// Freshness is checked on source timestamps, not PromQL evaluation timestamps.
func controlRoutePressureQuery(environment string) string {
	identity := "env,job,host,block,instance"
	base := fmt.Sprintf(`env=%s,job="api"`, strconv.Quote(environment))
	route := base + ",route=" + strconv.Quote(controlRouteLabel)
	count := "urnetwork_http_request_duration_seconds_count{" + route + "}"
	sum := "urnetwork_http_request_duration_seconds_sum{" + route + "}"
	requests := "urnetwork_http_requests_total{" + route + "}"
	canceled := "urnetwork_http_requests_total{" + route + `,outcome="canceled"}`
	inflight := "urnetwork_http_requests_inflight{" + route + "}"
	start := "process_start_time_seconds{" + base + "}"
	parts := []string{}
	add := func(name, expression string) {
		parts = append(parts, fmt.Sprintf(`label_replace((%s),"monitor_metric",%s,"job",".*")`, expression, strconv.Quote(name)))
	}
	add("start", start)
	add("start_time", "timestamp("+start+")")
	add("count", "increase("+count+"[5m])")
	add("seconds", "increase("+sum+"[5m])")
	add("requests", "sum by("+identity+")(increase("+requests+"[5m]))")
	add("canceled", "sum by("+identity+")(increase("+canceled+"[5m])) or on("+identity+") (0 * increase("+count+"[5m]))")
	add("inflight", inflight)
	add("inflight_min", "min_over_time("+inflight+"[5m])")
	add("samples", "count_over_time("+count+"[5m])")
	add("early_samples", "count_over_time("+count+"[1m] offset 4m)")
	for _, source := range []struct{ name, metric string }{{"seconds", sum}, {"inflight", inflight}} {
		add(source.name+"_samples", "count_over_time("+source.metric+"[5m])")
		add(source.name+"_early_samples", "count_over_time("+source.metric+"[1m] offset 4m)")
	}
	for _, source := range []struct{ name, metric string }{
		{"count_time", count}, {"seconds_time", sum}, {"inflight_time", inflight},
		{"requests_time", requests},
	} {
		add(source.name, "min by("+identity+")(timestamp("+source.metric+"))")
	}
	for _, source := range []struct{ name, metric string }{
		{"phase", "urnetwork_connect_control_http_phase_inflight{" + base + "}"},
		{"frame", "urnetwork_connect_control_frames_inflight{" + base + `,ingress="http"}`},
	} {
		add(source.name, source.metric+" and (timestamp("+source.metric+") >= time() - 90)")
	}
	return strings.Join(parts, " or ")
}

// The inventory denominator cannot be learned from surviving metric series.
func (controlRoutePressureProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	expected := map[string]bool{}
	for _, host := range env.cfg.logServiceHosts["api"] {
		for _, block := range env.cfg.logServiceBlocks["api"] {
			expected[host+"\x00"+block] = true
		}
	}
	if len(expected) == 0 {
		return []finding{controlRouteUnknown("API host/block inventory unavailable")}, nil
	}
	gateways := env.cfg.hostsWithRole("services")
	if len(gateways) == 0 {
		return []finding{controlRouteUnknown("No configured services gateway for Mimir")}, nil
	}
	command := "curl -fsS --max-time 15 --max-filesize 2097152 --data-urlencode " +
		shellSingleQuote("query="+controlRoutePressureQuery(env.cfg.env)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	out, _, err := shellFirstServiceGateway(ctx, env.runner, gateways, nil, command)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []finding{controlRouteUnknown("Bounded Mimir query unavailable")}, nil
	}
	processes, err := parseControlRoutePressure(out, env.cfg.env, env.now())
	if err != nil {
		return []finding{controlRouteUnknown("Invalid or incomplete Mimir response")}, nil
	}
	return evaluateControlRoutePressure(processes, expected, env.now()), nil
}

// Reject duplicate/unknown partitions rather than summing incompatible sources.
func parseControlRoutePressure(payload, environment string, now time.Time) ([]*controlRouteProcess, error) {
	if len(payload) > controlRouteResponseMax {
		return nil, fmt.Errorf("response exceeds bounded limit")
	}
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(payload), &response); err != nil || response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 {
		return nil, fmt.Errorf("invalid response")
	}
	processes := map[string]*controlRouteProcess{}
	for _, series := range response.Data.Result {
		labels := series.Metric
		if labels["env"] != environment || labels["job"] != "api" || labels["host"] == "" || labels["block"] == "" || labels["instance"] == "" {
			return nil, fmt.Errorf("invalid source identity")
		}
		key := labels["host"] + "\x00" + labels["block"] + "\x00" + labels["instance"]
		process := processes[key]
		if process == nil {
			process = &controlRouteProcess{host: labels["host"], block: labels["block"], instance: labels["instance"], values: map[string]float64{}}
			processes[key] = process
		}
		name := labels["monitor_metric"]
		switch name {
		case "start", "start_time", "count", "seconds", "requests", "canceled", "inflight", "inflight_min", "samples", "early_samples", "count_time", "seconds_time", "inflight_time", "requests_time", "seconds_samples", "seconds_early_samples", "inflight_samples", "inflight_early_samples":
		case "phase", "frame":
			partition := labels["phase"]
			allowed := controlRoutePhases[:]
			if name == "frame" {
				partition = labels["message"]
				allowed = controlRouteMessages[:]
				if labels["ingress"] != "http" {
					process.invalid = true
				}
			}
			valid := false
			for _, item := range allowed {
				valid = valid || partition == item
			}
			if !valid {
				process.invalid = true
			}
			name += ":" + partition
		default:
			process.invalid = true
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || now.Sub(observedAt) > controlRouteFreshness || observedAt.Sub(now) > 30*time.Second {
			process.invalid = true
			continue
		}
		if _, exists := process.values[name]; exists {
			process.invalid = true
		}
		process.values[name] = value
	}
	result := []*controlRouteProcess{}
	for _, process := range processes {
		result = append(result, process)
	}
	return result, nil
}

// Newest start wins even when its metrics are incomplete; a draining process
// cannot supply missing cells for its replacement. Equal starts are ambiguous.
func evaluateControlRoutePressure(processes []*controlRouteProcess, expected map[string]bool, now time.Time) []finding {
	current := map[string]*controlRouteProcess{}
	ambiguous := map[string]bool{}
	unknownStart := map[string]bool{}
	for _, process := range processes {
		slot := process.host + "\x00" + process.block
		if !expected[slot] {
			continue
		}
		if process.values["start"] <= 0 {
			unknownStart[slot] = true
			continue
		}
		old := current[slot]
		if old == nil || old.values["start"] < process.values["start"] {
			current[slot] = process
			ambiguous[slot] = false
		} else if old.values["start"] == process.values["start"] && old.instance != process.instance {
			ambiguous[slot] = true
		}
	}
	findings := []finding{}
	gaps := []string{}
	slots := []string{}
	for slot := range expected {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		process := current[slot]
		if process == nil || ambiguous[slot] || unknownStart[slot] || !controlRouteComplete(process, now) {
			gaps = append(gaps, strings.ReplaceAll(slot, "\x00", "/"))
			continue
		}
		values := process.values
		diagnostics, complete := controlRouteDiagnostics(process)
		if !complete {
			gaps = append(gaps, process.host+"/"+process.block+"[phase/frame telemetry unavailable]")
		}
		if values["count"] < 100 {
			continue
		}
		mean := values["seconds"] / values["count"]
		share := values["canceled"] / values["requests"]
		if share < 0.20 || mean < 5 {
			continue
		}
		tier, sustain := tierWarn, 1
		if share >= 0.50 && mean >= 10 && values["inflight_min"] >= 100 {
			tier, sustain = tierPage, 2
		}
		findings = append(findings, finding{
			probeId: "runtime/control-route-pressure", tier: tier, class: "control-route-pressure",
			target: process.host, frame: process.block, sustain: sustain,
			symptom:   "Traffic-bearing control requests are canceled while spending substantial time inside the API handler.",
			mechanism: "Control-plane residence can delay provide-secret registration and contract replies. A provider_unresponsive probe state can include these failures; this is not evidence that the provider exit is dark.",
			baseline:  "Per newest API process over 5m: WARN requires >=100 completions, >=20% canceled and >=5s mean; PAGE additionally requires >=50% canceled, >=10s mean and >=100 inflight throughout sampled range, sustained twice.",
			observed:  fmt.Sprintf("requests_5m=%.0f canceled_5m=%.0f canceled_share=%.1f%% mean_seconds=%.3f inflight=%.0f inflight_min_5m=%.0f %s", values["requests"], values["canceled"], 100*share, mean, values["inflight"], values["inflight_min"], diagnostics),
			evidence:  "Reset-aware counter increases and underlying source timestamps are joined on the exact newest process; the process and range must span five minutes. Cancellation includes every recorded status, including none and 200.",
			context:   "Mean is not p95; canceled/none is not an observed HTTP 499. Legitimate holds, caller churn or response loss can contribute. HTTP 200 and frame handler_ok may carry application rejection; completions are not ACKs or distinct providers. Phases do not isolate PG pool wait.",
			action:    "Compare authenticate versus controller phase residence/inflight, then HTTP-ingress frame completion outcomes and frame residence by fixed message. Corroborate with separate PG pool/CPU and same-source probe DNS path, registration, guard and durable measured coverage. Do not weaken provider/credit guards or infer a DNS-server fault from this signal.",
			verify:    "Require two fresh clean-generation cadences below pressure thresholds and a sustained increase in acknowledged distinct measured providers; neither an RPC reduction nor buffered completions certify the four-hour scan.",
			playbook:  "SIGNALS.md §2.19e",
		})
	}
	if len(gaps) > 0 {
		findings = append(findings, controlRouteUnknown("unobservable_slots="+strings.Join(gaps, ",")))
	}
	return findings
}

// Keep sampling gaps and process restart out of ratio arithmetic. The first
// minute of the range and at least five samples must be present independently.
func controlRouteComplete(process *controlRouteProcess, now time.Time) bool {
	if process.invalid {
		return false
	}
	for _, name := range []string{"start", "start_time", "count", "seconds", "requests", "canceled", "inflight", "inflight_min", "samples", "early_samples", "count_time", "seconds_time", "inflight_time", "requests_time", "seconds_samples", "seconds_early_samples", "inflight_samples", "inflight_early_samples"} {
		if _, ok := process.values[name]; !ok {
			return false
		}
	}
	v := process.values
	nowSeconds := float64(now.UnixNano()) / float64(time.Second)
	for _, name := range []string{"start_time", "count_time", "seconds_time", "inflight_time", "requests_time"} {
		if nowSeconds-v[name] > controlRouteFreshness.Seconds() || v[name]-nowSeconds > 30 {
			return false
		}
	}
	if v["start"] <= 0 || nowSeconds-v["start"] < 300 || v["samples"] < 5 || v["early_samples"] < 1 || v["canceled"] > v["requests"] || v["inflight_min"] > v["inflight"] {
		return false
	}
	if v["seconds_samples"] < 5 || v["seconds_early_samples"] < 1 || v["inflight_samples"] < 5 || v["inflight_early_samples"] < 1 {
		return false
	}
	// Concurrent scrape boundaries can differ slightly, not by an entire cohort.
	return math.Abs(v["requests"]-v["count"]) <= math.Max(5, 0.03*v["count"])
}

// Missing new collectors never erase pressure proven by the older route metric.
func controlRouteDiagnostics(process *controlRouteProcess) (string, bool) {
	parts := []string{}
	complete := true
	for _, group := range []struct {
		name       string
		partitions []string
	}{{"phase", controlRoutePhases[:]}, {"frame", controlRouteMessages[:]}} {
		for _, partition := range group.partitions {
			value, ok := process.values[group.name+":"+partition]
			complete = complete && ok
			if ok {
				parts = append(parts, fmt.Sprintf("%s_%s_inflight=%.0f", group.name, partition, value))
			}
		}
	}
	if !complete {
		parts = append(parts, "phase_frame_coverage=incomplete")
	}
	return strings.Join(parts, " "), complete
}

// An unavailable source is never a healthy, zero-cancellation control.
func controlRouteUnknown(observed string) finding {
	return finding{
		probeId: "runtime/control-route-pressure", tier: tierWarn, class: "control-route-pressure-unobservable",
		target: "api-fleet", frame: "coverage", sustain: 1,
		symptom:   "Control route pressure cannot be fully observed for every newest API slot.",
		mechanism: "Missing inventory, stale samples, a young replacement, incomplete range or absent phase/frame collectors cannot stand in for a quiet healthy control route.",
		baseline:  "Every active API host/block has one newest fresh process, a complete five-minute route range and bounded phase/frame gauges.",
		observed:  observed,
		evidence:  "Inventory is independent of surviving Mimir series; old draining processes cannot supply a replacement's missing evidence. Valid hot slots still alert alongside this coverage warning.",
		context:   "This is telemetry or rollout uncertainty, not provider fault or permission to change quality/credit gates.",
		action:    "Check exact API source/image and metric delivery, allow a new process one full five-minute range, then rerun. Do not infer fleet health from only observable siblings.",
		verify:    "Every configured API slot supplies a fresh nonambiguous source and complete range; retain any independently proven pressure alert.",
		playbook:  "SIGNALS.md §2.19e",
	}
}
