// SIGNALS.md §2.19b metric recovery needs slot, generation and source-time
// authority. Positive aggregates and proof of complete recovery stay separate.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	sitePoolCoverageMaxRows     = 4096
	sitePoolCoverageMaxSlots    = 64
	sitePoolCoverageGenerations = 4
	sitePoolCoverageFreshness   = 90 * time.Second
)

type sitePoolMetricIdentity struct{ service, host, block, instance string }
type sitePoolMetricSample struct {
	value, sourceTime   float64
	valueSeen, timeSeen bool
}
type sitePoolMetricProcess struct {
	samples      map[string]sitePoolMetricSample
	presence     float64
	presenceSeen bool
	resets       map[string]float64
}
type sitePoolMetricScope struct {
	slots    map[string]map[string]bool
	hosts    map[string][]string
	blocks   map[string][]string
	excluded map[string]bool
	rows     int
}

// Desired inventory is retained even when a host is disabled or unenrolled.
// Those slots cannot be supplied by a healthy sibling.
func egressSitePoolMetricScope(env *probeEnv) sitePoolMetricScope {
	scope := sitePoolMetricScope{slots: map[string]map[string]bool{}, hosts: map[string][]string{}, blocks: map[string][]string{}, excluded: map[string]bool{}, rows: 6}
	if runner, ok := env.runner.(*hostScopeRunner); ok {
		for _, name := range runner.excludedNames() {
			scope.excluded[name] = true
		}
	}
	configured := map[string]bool{}
	for _, h := range env.cfg.scopeHosts() {
		configured[h.name] = true
		scope.excluded[h.name] = scope.excluded[h.name] || h.disabled
	}
	for _, service := range []string{"api", "taskworker"} {
		scope.slots[service] = map[string]bool{}
		scope.blocks[service] = append([]string(nil), env.cfg.logServiceBlocks[service]...)
		for _, name := range env.cfg.logServiceHosts[service] {
			allowed := configured[name] && !scope.excluded[name]
			if allowed {
				scope.hosts[service] = append(scope.hosts[service], name)
			}
			for _, block := range scope.blocks[service] {
				scope.slots[service][name+"\x00"+block] = allowed
			}
		}
		perProcess := 25
		if service == "taskworker" {
			perProcess = 15
		}
		scope.rows += sitePoolCoverageGenerations * perProcess * len(scope.slots[service])
	}
	return scope
}

// The fixed children are producer-owned. API children are lazy, so absent
// counters cannot be manufactured as zero by this reader.
func sitePoolMetricFields(service string) []string {
	if service == "api" {
		return []string{"borrowed-quality", "borrowed-speed", "answered-quality", "answered-speed"}
	}
	return []string{"guard-full", "guard-blackhole"}
}

func sitePoolMetricWindow(service string, settings *EgressSitePoolSettings) time.Duration {
	if service == "api" {
		return settings.BackfillWindow
	}
	return settings.GuardTripWindow
}

// One query retains positive lookback aggregates and a raw two-bound proof.
// Presence over the whole window catches generations absent at both bounds.
func egressSitePoolPairedMetricsQuery(environment string, settings *EgressSitePoolSettings, scope sitePoolMetricScope) string {
	parts := []string{}
	quotePattern := func(values []string) string {
		values = append([]string(nil), values...)
		sort.Strings(values)
		for i := range values {
			values[i] = regexp.QuoteMeta(values[i])
		}
		return strconv.Quote(strings.Join(values, "|"))
	}
	for _, service := range []string{"api", "taskworker"} {
		if len(scope.hosts[service]) == 0 || len(scope.blocks[service]) == 0 {
			continue
		}
		selector := fmt.Sprintf("env=%s,job=%s,host=~%s,block=~%s", strconv.Quote(environment), strconv.Quote(service), quotePattern(scope.hosts[service]), quotePattern(scope.blocks[service]))
		window := strconv.FormatInt(int64(sitePoolMetricWindow(service, settings)/time.Second), 10) + "s"
		aggregate := func(metric, dimension, values, part string) string {
			return fmt.Sprintf(`label_replace(sum by (%s) (increase(%s{%s,%s=~%s}[%s])),"monitor_egress_part",%s,"","")`, dimension, metric, selector, dimension, strconv.Quote(values), window, strconv.Quote(part))
		}
		if service == "api" {
			parts = append(parts, aggregate("urnetwork_provider_backfill_sum", "rank_mode", "quality|speed", "borrowed"), aggregate("urnetwork_provider_answered_total", "rank_mode", "quality|speed", "answered"))
		} else {
			parts = append(parts, aggregate("urnetwork_egress_probe_batch_guard_trips_total", "schedule", "full|blackhole", "guard_trips"))
		}
		label := func(expression, key string) string {
			return "label_replace(" + expression + ",\"monitor_egress_sample\"," + strconv.Quote(key) + ",\"\",\"\")"
		}
		start := "process_start_time_seconds{" + selector + "}"
		parts = append(parts, label("count_over_time("+start+"["+window+"])", "present"))
		fields := append([]string{"start"}, sitePoolMetricFields(service)...)
		for _, field := range fields {
			metric := start
			if field != "start" {
				components := strings.SplitN(field, "-", 2)
				name, dimension := "urnetwork_egress_probe_batch_guard_trips_total", "schedule"
				if components[0] == "borrowed" {
					name, dimension = "urnetwork_provider_backfill_sum", "rank_mode"
				}
				if components[0] == "answered" {
					name, dimension = "urnetwork_provider_answered_total", "rank_mode"
				}
				metric = name + "{" + selector + "," + dimension + "=" + strconv.Quote(components[1]) + "}"
				parts = append(parts, label("resets("+metric+"["+window+"])", "resets/"+field))
			}
			for _, bound := range []string{"now", "prior"} {
				value := metric
				if bound == "prior" {
					value += " offset " + window
				}
				parts = append(parts, label(value, bound+"/"+field), label("timestamp("+value+")", bound+"/"+field+"/time"))
			}
		}
	}
	return strings.Join(parts, " or ")
}

func unavailableEgressSitePoolMetrics(reason string) egressSitePoolMetrics {
	return egressSitePoolMetrics{reason: reason, borrowed: map[string]float64{}, answered: map[string]float64{}, guardTrips: map[string]float64{}}
}

// Read through one permitted gateway with one frozen evaluation time. The
// transport/output bounds fail closed, without retry or raw label disclosure.
func readEgressSitePoolMetrics(ctx context.Context, env *probeEnv, settings *EgressSitePoolSettings) egressSitePoolMetrics {
	scope := egressSitePoolMetricScope(env)
	slots := len(scope.slots["api"]) + len(scope.slots["taskworker"])
	if slots > sitePoolCoverageMaxSlots || scope.rows > sitePoolCoverageMaxRows || settings.BackfillWindow <= 0 || settings.GuardTripWindow <= 0 {
		return unavailableEgressSitePoolMetrics("inventory-or-window-over-bound")
	}
	query := egressSitePoolPairedMetricsQuery(env.cfg.env, settings, scope)
	if query == "" {
		return unavailableEgressSitePoolMetrics("desired-inventory-unavailable")
	}
	var gateway *host
	for _, candidate := range env.cfg.hostsWithRole("services") {
		if !scope.excluded[candidate.name] && !candidate.disabled {
			gateway = candidate
			break
		}
	}
	if gateway == nil {
		return unavailableEgressSitePoolMetrics("bounded-source-unavailable")
	}
	now := env.now()
	command := "curl -fsS --max-time 15 --max-filesize " + strconv.Itoa(settings.MaxResponseBytes) +
		" --data-urlencode " + shellSingleQuote("query="+query) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(now.Unix(), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	out, err := env.runner.shell(ctx, gateway, command)
	if err != nil || out == "" || len(out) > settings.MaxResponseBytes {
		return unavailableEgressSitePoolMetrics("bounded-source-unavailable")
	}
	return parseEgressSitePoolPairedMetrics(out, env.cfg.env, now, settings, scope)
}

// Fixed error text only: response labels and bodies never become findings.
func parseEgressSitePoolPairedMetrics(raw, environment string, now time.Time, settings *EgressSitePoolSettings, scope sitePoolMetricScope) egressSitePoolMetrics {
	invalid := func() egressSitePoolMetrics { return unavailableEgressSitePoolMetrics("invalid-source-response") }
	metrics := unavailableEgressSitePoolMetrics("incomplete-process-window")
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if len(raw) > settings.MaxResponseBytes || json.Unmarshal([]byte(raw), &response) != nil || response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) > 0 || len(response.Infos) > 0 || len(response.Data.Result) > scope.rows {
		return invalid()
	}
	processes := map[sitePoolMetricIdentity]*sitePoolMetricProcess{}
	for _, series := range response.Data.Result {
		at, value, err := mimirInstantValue(series.Value)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || at.Unix() != now.Unix() {
			return invalid()
		}
		if part := series.Metric["monitor_egress_part"]; part != "" {
			key := series.Metric["rank_mode"]
			values := metrics.borrowed
			switch part {
			case "borrowed", "answered":
				if key != "quality" && key != "speed" {
					return invalid()
				}
				if part == "answered" {
					values = metrics.answered
				}
			case "guard_trips":
				key = series.Metric["schedule"]
				values = metrics.guardTrips
				if key != "full" && key != "blackhole" {
					return invalid()
				}
			default:
				return invalid()
			}
			if _, exists := values[key]; exists {
				return invalid()
			}
			values[key] = value
			continue
		}
		identity := sitePoolMetricIdentity{service: series.Metric["job"], host: series.Metric["host"], block: series.Metric["block"], instance: series.Metric["instance"]}
		allowed := scope.slots[identity.service][identity.host+"\x00"+identity.block]
		if series.Metric["env"] != environment || !allowed || identity.instance == "" || len(identity.instance) > 512 {
			return invalid()
		}
		process := processes[identity]
		if process == nil {
			process = &sitePoolMetricProcess{samples: map[string]sitePoolMetricSample{}, resets: map[string]float64{}}
			processes[identity] = process
		}
		key := series.Metric["monitor_egress_sample"]
		if key == "present" {
			if process.presenceSeen || value <= 0 || math.Trunc(value) != value {
				return invalid()
			}
			process.presence, process.presenceSeen = value, true
			continue
		}
		isTime := strings.HasSuffix(key, "/time")
		key = strings.TrimSuffix(key, "/time")
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			return invalid()
		}
		field := parts[1]
		known := field == "start"
		for _, candidate := range sitePoolMetricFields(identity.service) {
			known = known || field == candidate
		}
		if !known {
			return invalid()
		}
		if parts[0] == "resets" {
			if field == "start" || isTime || math.Trunc(value) != value {
				return invalid()
			}
			if _, exists := process.resets[field]; exists {
				return invalid()
			}
			process.resets[field] = value
			continue
		}
		if parts[0] != "now" && parts[0] != "prior" {
			return invalid()
		}
		sample := process.samples[key]
		if isTime {
			if sample.timeSeen {
				return invalid()
			}
			sample.sourceTime, sample.timeSeen = value, true
		} else {
			if sample.valueSeen {
				return invalid()
			}
			sample.value, sample.valueSeen = value, true
		}
		process.samples[key] = sample
	}
	metrics.observable = true
	complete := func(service string) bool {
		expected := scope.slots[service]
		if len(expected) == 0 {
			return false
		}
		bySlot := map[string][]*sitePoolMetricProcess{}
		for identity, process := range processes {
			if identity.service == service {
				slot := identity.host + "\x00" + identity.block
				bySlot[slot] = append(bySlot[slot], process)
			}
		}
		for slot, allowed := range expected {
			candidates := bySlot[slot]
			if !allowed || len(candidates) != 1 || !egressSitePoolProcessComplete(now, sitePoolMetricWindow(service, settings), service, candidates[0]) {
				return false
			}
		}
		return true
	}
	metrics.backfillComplete = complete("api")
	for _, rank := range []string{"quality", "speed"} {
		_, borrowed := metrics.borrowed[rank]
		answered, exists := metrics.answered[rank]
		metrics.backfillComplete = metrics.backfillComplete && borrowed && exists && answered > 0
	}
	metrics.guardComplete = complete("taskworker")
	for _, schedule := range []string{"full", "blackhole"} {
		_, exists := metrics.guardTrips[schedule]
		metrics.guardComplete = metrics.guardComplete && exists
	}
	metrics.coverage = fmt.Sprintf("expected_api_slots=%d expected_taskworker_slots=%d backfill_complete=%t guard_complete=%t", len(scope.slots["api"]), len(scope.slots["taskworker"]), metrics.backfillComplete, metrics.guardComplete)
	if metrics.backfillComplete && metrics.guardComplete {
		metrics.reason = ""
	}
	return metrics
}

// Scrape identity must agree across fields; each counter is monotonic on its
// own. Independent atomics do not justify cross-counter delta inequalities.
func egressSitePoolProcessComplete(now time.Time, window time.Duration, service string, process *sitePoolMetricProcess) bool {
	fresh := func(sample sitePoolMetricSample, bound time.Time) bool {
		age := float64(bound.UnixNano())/1e9 - sample.sourceTime
		return sample.valueSeen && sample.timeSeen && sample.sourceTime > 0 && age <= sitePoolCoverageFreshness.Seconds() && age >= -30
	}
	start, prior := process.samples["now/start"], process.samples["prior/start"]
	if !process.presenceSeen || !fresh(start, now) || !fresh(prior, now.Add(-window)) || start.value <= 0 || start.value != prior.value || start.value > prior.sourceTime || start.sourceTime <= prior.sourceTime {
		return false
	}
	for _, field := range sitePoolMetricFields(service) {
		current, previous := process.samples["now/"+field], process.samples["prior/"+field]
		reset, exists := process.resets[field]
		if !exists || reset != 0 || !fresh(current, now) || !fresh(previous, now.Add(-window)) || current.sourceTime != start.sourceTime || previous.sourceTime != prior.sourceTime || current.value < previous.value {
			return false
		}
	}
	return true
}
