// Measures announced public-provider transports using complete native cohorts.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Signal provider-connections implements SIGNALS.md §1.1a. The gauge counts
// transports, including concurrent carriers for one provider. Empty release
// buckets are deleted; neither missing buckets nor missing processes are zero.
func NewProviderConnectionsSignal() Signal {
	return &signalAdapter{
		number: "1.1a", key: "provider-connections", name: "Provider transport supply",
		probe: &providerConnectionsProbe{},
	}
}

// Only prospectively observed, native-qualified minute samples are retained.
// This bounded local window is lost on restart and never persists alert state.
type providerConnectionsProbe struct {
	stateLock     sync.Mutex
	running       chan struct{}
	window        providerConnectionsNativeWindow
	lastFailure   *finding
	lastFailureAt time.Time
}

func (*providerConnectionsProbe) id() string               { return "mimir/provider-connections" }
func (*providerConnectionsProbe) tier() string             { return tierPage }
func (*providerConnectionsProbe) cadence() time.Duration   { return time.Minute }
func (*providerConnectionsProbe) runBudget() time.Duration { return 50 * time.Second }

const (
	providerConnectionsCollapseRatio = 0.5
	providerConnectionsBaselineFloor = 1000.0
	// Twelve complete five-minute samples, not an hour of continuous coverage.
	providerConnectionsBaselineMinimumPoints = 12.0
	providerConnectionsFreshness             = 3 * time.Minute
	providerConnectionsMaximum               = float64(1<<53 - 1)
	providerConnectionsMaxHosts              = 4
	providerConnectionsMaxSlots              = 64
	providerConnectionsMaxProcesses          = 256
	providerConnectionsMaxRows               = 16384
	providerConnectionsMaxBytes              = 4 * 1024 * 1024
	providerConnectionsIdentityLabels        = "env,job,service,host,block,instance"
)

// Desired placement survives exclusions; a missing or disabled host is unknown.
func providerConnectionsScope(env *probeEnv) ([]*host, []string, string) {
	names, blocks := env.cfg.logServiceHosts["connect"], env.cfg.logServiceBlocks["connect"]
	if len(names) == 0 || len(names) > providerConnectionsMaxHosts || len(blocks) == 0 || len(blocks)*len(names) > providerConnectionsMaxSlots {
		return nil, nil, "slot_inventory_unavailable_or_over_bound"
	}
	excluded := map[string]bool{}
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		for _, name := range scoped.excludedNames() {
			excluded[name] = true
		}
	}
	known := map[string]*host{}
	for _, h := range env.cfg.scopeHosts() {
		known[h.name] = h
	}
	selected, seen := []*host{}, map[string]bool{}
	for _, name := range names {
		h := known[name]
		if h == nil || !h.hasRole("services") || h.disabled || excluded[name] || seen[name] {
			return nil, nil, "slot_inventory_incomplete"
		}
		seen[name] = true
		selected = append(selected, h)
	}
	seen = map[string]bool{}
	for _, block := range blocks {
		if block == "" || seen[block] {
			return nil, nil, "slot_inventory_incomplete"
		}
		seen[block] = true
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].name < selected[j].name })
	blocks = append([]string(nil), blocks...)
	sort.Strings(blocks)
	return selected, blocks, ""
}

// Each push gets one receiver timestamp (server/grafana.go; warp/grafana/push.go).
// Same-push joining removes deleted app_version buckets retained by lookback.
// Every independently fresh process counts, including a live draining process.
// A fleet history point exists only when all desired slots and every observed
// process have independent start/RSS and at least one coherent transport bucket.
func providerConnectionsQuery(environment string, hosts []*host, blocks []string) string {
	quoted := func(values []string) string {
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = regexp.QuoteMeta(value)
		}
		return strconv.Quote(strings.Join(parts, "|"))
	}
	names := []string{}
	for _, h := range hosts {
		names = append(names, h.name)
	}
	scope := fmt.Sprintf(`{env=%s,job="connect",service="connect",host=~%s,block=~%s}`, strconv.Quote(environment), quoted(names), quoted(blocks))
	start, rss, gauge := "process_start_time_seconds"+scope, "process_resident_memory_bytes"+scope, "urnetwork_connect_provider_connections"+scope
	identity := providerConnectionsIdentityLabels
	group := func(op, labels, expression string) string { return op + " by (" + labels + ") (" + expression + ")" }
	at := func(expression string) string { return "timestamp(" + expression + ")" }
	namedStart := strings.Replace(start, "{", `{instance!="",`, 1)
	namedGauge := strings.Replace(gauge, "{", `{instance!="",app_version!="",`, 1)
	startAt, rssAt := group("max", identity, at(start)), group("max", identity, at(rss))
	native := "(" + group("max", identity, start) + " > 0) and (" + group("max", identity, start) + " <= " + startAt + " + 30)" +
		" and (" + startAt + " >= time() - 180) and (" + startAt + " <= time() + 30)" +
		" and (" + startAt + " == " + rssAt + ") and (" + group("max", identity, rss) + " >= 0)" +
		" and (" + group("max", identity, rss) + " <= 9007199254740991) and (" + group("max", identity, rss) + " == floor(" + group("max", identity, rss) + "))" +
		" and (" + group("count", identity, start) + " == 1) and (" + group("count", identity, namedStart) + " == 1) and (" + group("count", identity, rss) + " == 1)"
	coherent := "(" + gauge + " and (" + at(gauge) + " == on (" + identity + ") group_left() " + startAt + "))"
	badValues := "(" + gauge + " <= 0) or (" + gauge + " > 9007199254740991) or (" + gauge + " != floor(" + gauge + "))"
	bad := "((" + badValues + ") and on (" + identity + ",app_version) " + group("group", identity+",app_version", coherent) + ") or (" + group("count", identity+",app_version", coherent) + " > 1)"
	bad += " or (" + group("group", identity+",app_version", coherent) + " unless " + group("group", identity+",app_version", namedGauge) + ")"
	totals := group("sum", identity, coherent)
	qualified := "(" + totals + " and on (" + identity + ") (" + native + ") unless on (" + identity + ") " + group("count", identity, bad) + ")"
	// A malformed or unmatched recent process remains in the roster. Reject
	// the point rather than summing just the process identities that matched.
	roster := group("group", identity, "("+at(start)+" >= time() - 180) or ("+at(rss)+" >= time() - 180) or ("+at(gauge)+" >= time() - 180)")
	fleet := "(sum(" + totals + " and on (" + identity + ") " + roster + ") unless count(" + roster + " unless on (" + identity + ") " + qualified + ") and (count(" + group("group", "host,block", roster) + ") == " + strconv.Itoa(len(hosts)*len(blocks)) + "))"
	tag := func(expression, metric string) string {
		return "label_replace(" + expression + ",\"monitor_metric\"," + strconv.Quote(metric) + ",\"\",\"\")"
	}
	parts := []string{
		tag("max_over_time("+fleet+"[5m:1m])", "best_5m"),
		tag("count_over_time("+fleet+"[5m:1m])", "points_5m"),
		tag("count_over_time(vector(1)[5m:1m])", "expected_points_5m"),
		tag("quantile_over_time(0.9,"+fleet+"[24h:5m])", "p90_24h"),
		tag("count_over_time("+fleet+"[24h:5m])", "points_24h"),
	}
	for _, field := range []struct{ name, selector string }{{name: "start", selector: start}, {name: "rss", selector: rss}, {name: "transports", selector: gauge}} {
		parts = append(parts, tag(field.selector, field.name), tag(at(field.selector), field.name+"_time"))
	}
	return strings.Join(parts, " or ")
}

type providerConnectionsIdentity struct{ host, block, instance string }
type providerConnectionsSample struct {
	value, at           float64
	valueSeen, timeSeen bool
}
type providerConnectionsProcess struct {
	samples map[string]providerConnectionsSample
}
type providerConnectionsFrame struct {
	values    map[string]float64
	invalid   map[string]bool
	processes map[providerConnectionsIdentity]*providerConnectionsProcess
}

// Fixed schema failures never include raw response, annotation or label text.
// Nonempty annotations are unqualified for this float-only query, not a claim
// that every warning means data loss. Empty annotation arrays are accepted.
func parseProviderConnectionsFrame(raw, environment string, now time.Time, hosts []*host, blocks []string) (*providerConnectionsFrame, string) {
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > providerConnectionsMaxBytes || !providerConnectionsJsonShape(raw) || decoder.Decode(&response) != nil || response.Status != "success" || response.Error != "" || response.Data.ResultType != "vector" || len(response.Data.Result) > providerConnectionsMaxRows {
		return nil, "invalid_response"
	}
	if len(response.Warnings) != 0 || len(response.Infos) != 0 {
		return nil, "response_annotation_unqualified"
	}
	frame := &providerConnectionsFrame{values: map[string]float64{}, invalid: map[string]bool{}, processes: map[providerConnectionsIdentity]*providerConnectionsProcess{}}
	slots := map[string]bool{}
	for _, h := range hosts {
		for _, block := range blocks {
			slots[h.name+"\x00"+block] = true
		}
	}
	for _, series := range response.Data.Result {
		metric := series.Metric["monitor_metric"]
		field := strings.TrimSuffix(metric, "_time")
		processField := field == "start" || field == "rss" || field == "transports"
		if !processField {
			switch metric {
			case "best_5m", "points_5m", "expected_points_5m", "p90_24h", "points_24h":
			default:
				return nil, "unknown_metric_class"
			}
			if len(series.Metric) != 1 {
				return nil, "invalid_scalar_labels"
			}
			if _, exists := frame.values[metric]; exists || frame.invalid[metric] {
				return nil, "duplicate_metric"
			}
			value, ok := providerConnectionsScalar(series.Value, now)
			if !ok || value > providerConnectionsMaximum || (metric != "p90_24h" && math.Trunc(value) != value) {
				frame.invalid[metric] = true
				continue
			}
			frame.values[metric] = value
			continue
		}
		id := providerConnectionsIdentity{host: series.Metric["host"], block: series.Metric["block"], instance: series.Metric["instance"]}
		if series.Metric["env"] != environment || series.Metric["job"] != "connect" || series.Metric["service"] != "connect" || !slots[id.host+"\x00"+id.block] || id.instance == "" || len(id.instance) > 128 {
			return nil, "invalid_process_identity"
		}
		for name := range series.Metric {
			switch name {
			case "__name__", "env", "job", "service", "host", "block", "instance", "monitor_metric":
			case "app_version":
				if field != "transports" {
					return nil, "invalid_process_labels"
				}
			default:
				return nil, "invalid_process_labels"
			}
		}
		if field == "transports" {
			version := series.Metric["app_version"]
			if version == "" || len(version) > 128 {
				return nil, "invalid_process_labels"
			}
			field += "/" + version
		}
		process := frame.processes[id]
		if process == nil {
			process = &providerConnectionsProcess{samples: map[string]providerConnectionsSample{}}
			frame.processes[id] = process
		}
		if len(frame.processes) > providerConnectionsMaxProcesses {
			return nil, "process_bound"
		}
		sample := process.samples[field]
		value, ok := providerConnectionsScalar(series.Value, now)
		if !ok {
			return nil, "invalid_process_sample"
		}
		if strings.HasSuffix(metric, "_time") {
			if sample.timeSeen {
				return nil, "duplicate_process_sample"
			}
			sample.at, sample.timeSeen = value, true
		} else {
			if sample.valueSeen {
				return nil, "duplicate_process_sample"
			}
			sample.value, sample.valueSeen = value, true
		}
		process.samples[field] = sample
	}
	return frame, ""
}

// Reject duplicate object fields and trailing documents before typed decoding.
// Depth and the caller's byte limit bound hostile nesting and schema work.
func providerConnectionsJsonShape(raw string) bool {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var read func(int) bool
	read = func(depth int) bool {
		if depth > 16 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, nested := token.(json.Delim)
		if !nested {
			return true
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] || !read(depth+1) {
					return false
				}
				seen[name] = true
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !read(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !read(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

// Query time is pinned by the caller; require native numeric-time/string-value
// encoding before converting a clock. No NaN, infinity or overflowing Unix cast.
func providerConnectionsScalar(parts []json.RawMessage, now time.Time) (float64, bool) {
	var evaluated float64
	var encoded string
	if len(parts) != 2 || json.Unmarshal(parts[0], &evaluated) != nil || json.Unmarshal(parts[1], &encoded) != nil ||
		math.IsNaN(evaluated) || math.IsInf(evaluated, 0) || math.Abs(evaluated-float64(now.UnixMilli())/1000) > 0.001 {
		return 0, false
	}
	value, err := strconv.ParseFloat(encoded, 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// Qualifies the original receiver clock, not the instant evaluation timestamp.
func providerConnectionsFresh(sample providerConnectionsSample, now time.Time) bool {
	delta := float64(now.UnixMilli())/1000 - sample.at
	return sample.valueSeen && sample.timeSeen && sample.at > 0 && delta >= -30 && delta <= providerConnectionsFreshness.Seconds()
}

// Returns only complete same-push processes. Absent or zero-only gauge families
// stay unknown. Old buckets whose timestamp predates a newer push are deletions.
func (self *providerConnectionsFrame) current(now time.Time) (map[providerConnectionsIdentity]float64, string) {
	values := map[providerConnectionsIdentity]float64{}
	for id, process := range self.processes {
		recent := false
		for _, sample := range process.samples {
			if sample.valueSeen != sample.timeSeen {
				return nil, "process_timestamp_companion_missing"
			}
			recent = recent || (sample.timeSeen && sample.at >= float64(now.UnixMilli())/1000-providerConnectionsFreshness.Seconds())
		}
		if !recent {
			continue
		}
		value, reason := providerConnectionsProcessValue(process, now)
		if reason != "" {
			return nil, reason
		}
		values[id] = value
	}
	return values, ""
}

// Validates a single independently identified generation before aggregation.
func providerConnectionsProcessValue(process *providerConnectionsProcess, now time.Time) (float64, string) {
	start, rss := process.samples["start"], process.samples["rss"]
	if !providerConnectionsFresh(start, now) || !providerConnectionsFresh(rss, now) || start.value <= 0 || start.value > start.at+30 || start.at != rss.at || rss.value > providerConnectionsMaximum || math.Trunc(rss.value) != rss.value {
		return 0, "process_cohort_incomplete"
	}
	total, buckets := 0.0, 0
	for field, sample := range process.samples {
		if !strings.HasPrefix(field, "transports/") {
			continue
		}
		if !sample.valueSeen || !sample.timeSeen {
			return 0, "transport_bucket_incomplete"
		}
		if sample.at < start.at {
			continue
		}
		if sample.at != start.at || sample.value <= 0 || sample.value > providerConnectionsMaximum || math.Trunc(sample.value) != sample.value {
			return 0, "transport_bucket_invalid"
		}
		total += sample.value
		buckets++
	}
	if buckets == 0 || total > providerConnectionsMaximum {
		return 0, "transport_gauge_absent"
	}
	return total, ""
}

// Requires the existing sampled-point minimum without claiming continuous history.
func (self *providerConnectionsFrame) baseline() (float64, string) {
	level, levelOk := self.values["p90_24h"]
	points, pointsOk := self.values["points_24h"]
	if !levelOk || !pointsOk || points < providerConnectionsBaselineMinimumPoints || points > 289 {
		return 0, "baseline_incomplete"
	}
	return level, ""
}

// An observed five-minute best requires every aligned subquery step.
func (self *providerConnectionsFrame) best() (float64, bool) {
	best, bestOk := self.values["best_5m"]
	points, pointsOk := self.values["points_5m"]
	expected, expectedOk := self.values["expected_points_5m"]
	return best, bestOk && best > 0 && pointsOk && expectedOk && (expected == 5 || expected == 6) && points == expected
}

// Visibility belongs to the stable fleet identity and never clears an incident.
func providerConnectionsUnknown(target, reason string) finding {
	return finding{probeId: "mimir/provider-connections", tier: tierWarn, class: "provider-connections-unobservable", target: target, sustain: 2,
		symptom:   "Provider transport supply cannot be completely measured against its 24 h level.",
		mechanism: "Missing publishers and deleted zero buckets do not prove zero transports. A partial observed sum cannot prove whole-fleet collapse or recovery.",
		baseline:  "Native Connect job/service and configured host/block coverage, coherent per-process pushes, twelve complete baseline samples, and a complete five-minute measurement.",
		observed:  "visibility_reason=" + reason,
		evidence:  "Only fixed reasons and aggregate counts are public; raw process identities, labels and backend text are omitted from alerts.",
		context:   "The gauge counts transports, not unique providers. A current native census cannot reconstruct past missing publishers; native fallback needs six or seven prospective minute observations with a proved five-minute interval span.",
		action:    "Check the exact configured Connect processes, their stats publisher and the local Grafana push receiver/Mimir path. Restore missing gauges; use §2.11a and §5.12 for dependency readiness failures. Do not infer zero from absent gauge buckets.",
		verify:    "Complete coherent fleet measurements return for the full five-minute window with at least twelve complete baseline samples.", playbook: "SIGNALS.md §1.1a"}
}

// A complete native failure survives a separate current-source gap. Recovery
// requires current completeness too, so the notifier never clears on an unknown.
func (self *providerConnectionsProbe) check(ctx context.Context, env *probeEnv) (result []finding, resultErr error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Serialize one signal instance without letting queue time escape its owner.
	self.stateLock.Lock()
	if self.running == nil {
		self.running = make(chan struct{}, 1)
	}
	running := self.running
	self.stateLock.Unlock()
	select {
	case running <- struct{}{}:
		defer func() { <-running }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Parse/reduction time belongs to the same owner as transport. Preserve
	// its original source causes and previous witness if the deadline expires
	// before the final return, including for an unscoped public adapter.
	self.stateLock.Lock()
	priorScope, priorBest, priorAt := self.window.scope, self.window.provedBest, self.window.provedAt
	priorFailure, priorFailureAt := self.lastFailure, self.lastFailureAt
	self.stateLock.Unlock()
	var sourceCause error
	defer func() {
		if contextErr := ctx.Err(); contextErr != nil {
			self.stateLock.Lock()
			self.window.points = nil
			if self.window.scope == priorScope {
				self.window.provedBest, self.window.provedAt = priorBest, priorAt
				self.lastFailure, self.lastFailureAt = priorFailure, priorFailureAt
			}
			self.stateLock.Unlock()
			result = nil
			if cause := errors.Join(resultErr, sourceCause); cause != nil {
				resultErr = &signalContextError{contextErr: contextErr, sourceErr: cause}
			} else {
				resultErr = contextErr
			}
		}
	}()
	now, target := env.now().UTC(), env.cfg.env+"/connect-fleet"
	hosts, blocks, reason := providerConnectionsScope(env)
	if reason != "" {
		self.clearNative()
		return []finding{providerConnectionsUnknown(target, reason)}, nil
	}
	self.bindNativeScope(env.cfg.env, hosts, blocks)
	gateways := env.cfg.hostsWithRole("services")
	if len(gateways) == 0 {
		self.resetNative()
		return self.unknownFindings(target, "metrics_gateway_unavailable", now), nil
	}
	query := providerConnectionsQuery(env.cfg.env, hosts, blocks)
	if len(query) > 100000 {
		self.resetNative()
		return self.unknownFindings(target, "query_bound", now), nil
	}
	command := "curl -fsS --max-time 20 --max-filesize " + strconv.Itoa(providerConnectionsMaxBytes) + " --data-urlencode " + shellSingleQuote("query="+query) + " --data-urlencode " + shellSingleQuote("time="+strconv.FormatFloat(float64(now.UnixMilli())/1000, 'f', 3, 64)) + " 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	output, err := mimirShellParsed(ctx, env.runner, gateways[0], command, providerConnectionsMaxBytes)
	sourceCause = err
	if contextErr := ctx.Err(); contextErr != nil {
		self.resetNative()
		if err != nil {
			return nil, &signalContextError{contextErr: contextErr, sourceErr: err}
		}
		return nil, contextErr
	}
	if err != nil {
		self.resetNative()
		return self.unknownFindings(target, "bounded_source_unavailable", now), nil
	}
	frame, reason := parseProviderConnectionsFrame(output, env.cfg.env, now, hosts, blocks)
	if reason != "" {
		self.resetNative()
		return self.unknownFindings(target, reason, now), nil
	}
	level, baselineReason := frame.baseline()
	best, windowComplete := frame.best()
	processValues, currentReason := frame.current(now)
	current, slots := 0.0, map[string]bool{}
	for id, value := range processValues {
		current += value
		slots[id.host+"\x00"+id.block] = true
	}
	currentComplete := currentReason == "" && len(slots) == len(hosts)*len(blocks) && current <= providerConnectionsMaximum
	// An anomaly-only native census also detects processes missing completely
	// from Mimir. It can supply prospective zero only for observed empty slots.
	nativeNeeded := baselineReason == "" && (!currentComplete || !windowComplete || (level >= providerConnectionsBaselineFloor && (current < providerConnectionsCollapseRatio*level || best < providerConnectionsCollapseRatio*level)))
	nativeWindowComplete := false
	windowAt := now
	if nativeNeeded {
		observation := self.nativeSample(ctx, env, now, hosts, blocks, frame)
		sourceCause = errors.Join(sourceCause, observation.err)
		if contextErr := ctx.Err(); contextErr != nil {
			if observation.err != nil {
				return nil, &signalContextError{contextErr: contextErr, sourceErr: observation.err}
			}
			return nil, contextErr
		}
		if observation.reason == "" {
			current, currentComplete = observation.current, true
		} else {
			currentComplete, currentReason = false, observation.reason
		}
		if observation.complete {
			best, windowComplete, nativeWindowComplete, windowAt = observation.best, true, true, observation.at
		}
	} else {
		self.resetNative()
	}
	findings := []finding{}
	if baselineReason != "" {
		reason = baselineReason
	} else if !windowComplete {
		reason = "five_minute_window_incomplete"
	} else if !currentComplete {
		reason = currentReason
		if reason == "" {
			reason = "current_cohort_incomplete"
		}
	}
	if reason != "" {
		findings = append(findings, providerConnectionsUnknown(target, reason))
	} else {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "provider-connections-unobservable", target))
	}
	if baselineReason != "" || !windowComplete {
		return self.unknownFindings(target, reason, now), nil
	}
	if level < providerConnectionsBaselineFloor || best >= providerConnectionsCollapseRatio*level {
		if currentComplete {
			self.clearNativeFailure()
			findings = append(findings, healthyFinding(self.id(), tierPage, "provider-connections-collapse", target))
		}
		return findings, nil
	}
	if !nativeWindowComplete {
		priorBest, priorAt, proved := self.priorNativeFailure(now, level)
		if !proved {
			if reason == "" {
				findings = []finding{providerConnectionsUnknown(target, "native_five_minute_window_warming")}
			}
			return findings, nil
		}
		best, windowAt = priorBest, priorAt
		if reason == "" {
			findings = []finding{providerConnectionsUnknown(target, "current_native_window_incomplete")}
		}
	}
	currentText := "unknown"
	if currentComplete {
		currentText = strconv.FormatFloat(current, 'f', 0, 64)
	}
	failure := finding{probeId: self.id(), tier: tierPage, class: "provider-connections-collapse", target: target, sustain: 1,
		symptom:   fmt.Sprintf("Provider transports collapsed: the five-minute best was %.0f, %.1f%% of the 24 h level %.1f (90th percentile)", best, 100*best/level, level),
		mechanism: "A complete measured fleet transport total remains below half its prior level. Crashes, readiness failures and ingress faults can cause this symptom; transport totals do not identify its cause or count unique providers.",
		baseline:  "The same configured cohort has at least twelve complete five-minute baseline samples; the five-minute best remains at least half the 24 h 90th percentile.",
		observed:  fmt.Sprintf("observed_provider_transports=%s best_5m=%.0f p90_24h=%.1f baseline_points=%.0f current_complete=%t", currentText, best, level, frame.values["points_24h"], currentComplete) + " measured_window_end=" + windowAt.Format(time.RFC3339),
		evidence:  "Six or seven prospective native-joined censuses conservatively span five minutes and establish the whole live cohort; live draining and replacement transports are both counted.",
		context:   "The .5 threshold, 1000 baseline floor, one-minute cadence and five-minute best are unchanged. Receiver-held samples can lag cessation by three minutes. A collapse lasting roughly 21 hours lowers its own baseline. Native warmup never backfills historical absence.",
		action:    "Read the Connect blocks' warp journals for `Deploy fail ... not ready` and their /status (§13.4); if readiness fails on PostgreSQL follow §2.11a and §5.12 and hold deploys and restarts, otherwise §5.1.",
		verify:    "A complete current cohort and complete five-minute best recover above half the 24 h level; confirm accepted handshakes separately.", playbook: "SIGNALS.md §1.1a and §5.1"}
	self.stateLock.Lock()
	self.lastFailure, self.lastFailureAt = &failure, windowAt
	self.stateLock.Unlock()
	return append(findings, failure), nil
}
