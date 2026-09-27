// SIGNALS.md §2.9b observes picker responses separately from FindProviders2.
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
	providerPickerWindow   = 5 * time.Minute
	providerPickerMaxRows  = 8192
	providerPickerMaxBytes = 4 * 1024 * 1024
	providerPickerMaxSlots = 32
)

// Endpoint outcomes have their own identity; neither cache size nor HTTP 200
// establishes that a usable initial picker row was returned.
func NewProviderPickerSignal() Signal {
	return &signalAdapter{number: "2.9b", key: "provider-picker", name: "App location picker availability", probe: providerPickerProbe{}}
}

type providerPickerProbe struct{}

func (providerPickerProbe) id() string             { return "mimir/provider-picker" }
func (providerPickerProbe) tier() string           { return tierPage }
func (providerPickerProbe) cadence() time.Duration { return time.Minute }

// Inventory comes from the same API placement authority as the log collector.
// Excluded/unenrolled desired slots remain unknown rather than disappearing.
type providerPickerScope struct {
	slots         map[string]bool
	hosts, blocks []string
	excluded      map[string]bool
}

func pickerScope(env *probeEnv) providerPickerScope {
	scope := providerPickerScope{slots: map[string]bool{}, excluded: map[string]bool{}, blocks: env.cfg.logServiceBlocks["api"]}
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
	for _, name := range env.cfg.logServiceHosts["api"] {
		allowed := configured[name] && !scope.excluded[name]
		if allowed {
			scope.hosts = append(scope.hosts, name)
		}
		for _, block := range scope.blocks {
			scope.slots[name+"\x00"+block] = allowed
		}
	}
	return scope
}

func pickerFields() []string {
	return []string{"initial/nonempty", "initial/empty", "initial/error", "search/nonempty", "search/empty", "search/error", "direct/nonempty", "direct/empty", "direct/error", "read/initial", "read/filters"}
}

// One bounded query pairs exact process generations, raw source timestamps,
// reset witnesses and range presence. No fallback manufactures lazy zeroes.
func providerPickerQuery(environment string, scope providerPickerScope) string {
	pattern := func(values []string) string {
		parts := append([]string(nil), values...)
		sort.Strings(parts)
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		return strconv.Quote(strings.Join(parts, "|"))
	}
	selector := fmt.Sprintf("env=%s,job=\"api\",host=~%s,block=~%s", strconv.Quote(environment), pattern(scope.hosts), pattern(scope.blocks))
	tag := func(expression, field string) string {
		return "label_replace(" + expression + ",\"monitor_picker\"," + strconv.Quote(field) + ",\"\",\"\")"
	}
	start := "process_start_time_seconds{" + selector + "}"
	parts := []string{tag("count_over_time("+start+"[5m])", "presence")}
	for _, field := range append([]string{"start"}, pickerFields()...) {
		metric := start
		if field != "start" {
			components := strings.Split(field, "/")
			if components[0] == "read" {
				metric = "urnetwork_provider_picker_read_errors_total{" + selector + ",phase=" + strconv.Quote(components[1]) + "}"
			} else {
				metric = "urnetwork_provider_picker_outcomes_total{" + selector + ",surface=" + strconv.Quote(components[0]) + ",outcome=" + strconv.Quote(components[1]) + "}"
			}
			parts = append(parts, tag("resets("+metric+"[5m])", "resets/"+field))
		}
		for _, bound := range []string{"now", "prior"} {
			value := metric
			if bound == "prior" {
				value += " offset 5m"
			}
			parts = append(parts, tag(value, bound+"/"+field), tag("timestamp("+value+")", bound+"/"+field+"/time"))
		}
	}
	return strings.Join(parts, " or ")
}

type pickerProcessKey struct{ host, block, instance string }
type pickerSample struct {
	value, sourceTime float64
	hasValue, hasTime bool
}
type pickerProcess struct {
	samples  map[string]pickerSample
	resets   map[string]float64
	presence bool
}
type pickerEvidence struct {
	deltas           map[string]float64
	complete         bool
	paired, expected int
	reason           string
}

func (p providerPickerProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope := pickerScope(env)
	unavailable := func(reason string) ([]finding, error) {
		return []finding{providerPickerVisibility(reason, 0, len(scope.slots))}, nil
	}
	if len(scope.slots) == 0 || len(scope.slots) > providerPickerMaxSlots || len(scope.hosts) == 0 || len(scope.blocks) == 0 {
		return unavailable("inventory-unavailable-or-over-bound")
	}
	var gateway *host
	for _, h := range env.cfg.hostsWithRole("services") {
		if !h.disabled && !scope.excluded[h.name] {
			gateway = h
			break
		}
	}
	if gateway == nil {
		return unavailable("gateway-unavailable")
	}
	now := env.now()
	command := "curl -fsS --max-time 15 --max-filesize " + strconv.Itoa(providerPickerMaxBytes) + " --data-urlencode " + shellSingleQuote("query="+providerPickerQuery(env.cfg.env, scope)) + " --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(now.Unix(), 10)) + " 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	out, err := env.runner.shell(ctx, gateway, command)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return unavailable("bounded-source-unavailable")
	}
	evidence := parseProviderPicker(out, env.cfg.env, now, scope)
	return providerPickerFindings(evidence), nil
}

// Reject an invalid envelope without echoing its labels, errors or body.
func parseProviderPicker(raw, environment string, now time.Time, scope providerPickerScope) pickerEvidence {
	evidence := pickerEvidence{deltas: map[string]float64{}, expected: len(scope.slots), reason: "incomplete-process-window"}
	invalid := func() pickerEvidence {
		return pickerEvidence{deltas: map[string]float64{}, expected: len(scope.slots), reason: "invalid-source-response"}
	}
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if len(raw) > providerPickerMaxBytes || json.Unmarshal([]byte(raw), &response) != nil || response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 || len(response.Infos) != 0 || len(response.Data.Result) > providerPickerMaxRows {
		return invalid()
	}
	processes := map[pickerProcessKey]*pickerProcess{}
	known := map[string]bool{"start": true}
	for _, field := range pickerFields() {
		known[field] = true
	}
	for _, row := range response.Data.Result {
		at, value, err := mimirInstantValue(row.Value)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 9007199254740991 || at.Unix() != now.Unix() {
			return invalid()
		}
		key := pickerProcessKey{host: row.Metric["host"], block: row.Metric["block"], instance: row.Metric["instance"]}
		if row.Metric["env"] != environment || row.Metric["job"] != "api" || !scope.slots[key.host+"\x00"+key.block] || key.instance == "" || len(key.instance) > 512 {
			return invalid()
		}
		process := processes[key]
		if process == nil {
			process = &pickerProcess{samples: map[string]pickerSample{}, resets: map[string]float64{}}
			processes[key] = process
		}
		label := row.Metric["monitor_picker"]
		if label == "presence" {
			if process.presence || value < 1 || math.Trunc(value) != value {
				return invalid()
			}
			process.presence = true
			continue
		}
		isTime := strings.HasSuffix(label, "/time")
		label = strings.TrimSuffix(label, "/time")
		parts := strings.SplitN(label, "/", 2)
		if len(parts) != 2 || !known[parts[1]] {
			return invalid()
		}
		if parts[0] == "resets" {
			if isTime || parts[1] == "start" || math.Trunc(value) != value {
				return invalid()
			}
			if _, exists := process.resets[parts[1]]; exists {
				return invalid()
			}
			process.resets[parts[1]] = value
			continue
		}
		if parts[0] != "now" && parts[0] != "prior" {
			return invalid()
		}
		sample := process.samples[label]
		if isTime {
			if sample.hasTime {
				return invalid()
			}
			sample.sourceTime, sample.hasTime = value, true
		} else {
			if sample.hasValue {
				return invalid()
			}
			sample.value, sample.hasValue = value, true
		}
		process.samples[label] = sample
	}
	bySlot := map[string]int{}
	pairedSlots := map[string]int{}
	for key, process := range processes {
		slot := key.host + "\x00" + key.block
		bySlot[slot]++
		if bySlot[slot] > 4 {
			return invalid()
		}
		if !pickerProcessPaired(process, now) {
			continue
		}
		evidence.paired++
		pairedSlots[slot]++
		for _, field := range pickerFields() {
			evidence.deltas[field] += process.samples["now/"+field].value - process.samples["prior/"+field].value
		}
	}
	evidence.complete = len(scope.slots) > 0
	for slot, allowed := range scope.slots {
		evidence.complete = evidence.complete && allowed && bySlot[slot] == 1 && pairedSlots[slot] == 1
	}
	if evidence.complete {
		evidence.reason = ""
	}
	return evidence
}

// One complete scrape at both bounds and no reset are required for a delta.
// Range presence also prevents a short-lived generation from proving recovery.
func pickerProcessPaired(process *pickerProcess, now time.Time) bool {
	fresh := func(sample pickerSample, bound time.Time) bool {
		age := float64(bound.UnixNano())/1e9 - sample.sourceTime
		return sample.hasValue && sample.hasTime && sample.sourceTime > 0 && age >= -30 && age <= 90
	}
	current, previous := process.samples["now/start"], process.samples["prior/start"]
	if !process.presence || !fresh(current, now) || !fresh(previous, now.Add(-providerPickerWindow)) || current.value <= 0 || current.value != previous.value || current.value > previous.sourceTime || current.sourceTime <= previous.sourceTime {
		return false
	}
	for _, field := range pickerFields() {
		a, b := process.samples["now/"+field], process.samples["prior/"+field]
		reset, exists := process.resets[field]
		if !exists || reset != 0 || !fresh(a, now) || !fresh(b, now.Add(-providerPickerWindow)) || a.sourceTime != current.sourceTime || b.sourceTime != previous.sourceTime || a.value < b.value {
			return false
		}
	}
	return true
}

func providerPickerVisibility(reason string, paired, expected int) finding {
	return finding{probeId: "mimir/provider-picker", tier: tierWarn, class: "provider-picker-unobservable", target: "api-fleet", sustain: 1,
		symptom: "App location picker availability cannot be fully observed", observed: fmt.Sprintf("reason=%s paired_processes=%d expected_slots=%d", reason, paired, expected),
		mechanism: "Missing, stale, reset or mixed-generation picker counters cannot prove an empty or healthy picker. HTTP 200 and response bytes are transport evidence only.",
		baseline:  "Complete desired API placement with eleven preinitialized children paired over five minutes, fresh process/start and source clocks, no reset, and at least 20 successful initial outcomes for health.",
		evidence:  "Only fixed reason and count fields are retained; raw response labels, cache keys and error bodies are not rendered.",
		context:   "Unknown coverage does not erase an independently observed failing subset or prove that the app list is empty. A quiet initial endpoint cannot certify recovery.",
		action:    "Verify API picker producer rollout, complete permitted API placement, paired process/start and raw sample clocks, and Mimir admission/query continuity. Do not manufacture zero counters or replay customer requests.",
		verify:    "Require a complete fresh five-minute producer window and real initial-list traffic; absence of a warning alone is not recovery.",
		playbook:  "SIGNALS.md §2.9b"}
}

func providerPickerFindings(e pickerEvidence) []finding {
	findings := []finding{}
	observed := fmt.Sprintf("initial_nonempty=%.0f initial_empty=%.0f initial_error=%.0f search_nonempty=%.0f search_empty=%.0f search_error=%.0f direct_nonempty=%.0f direct_empty=%.0f direct_error=%.0f read_initial=%.0f read_filters=%.0f paired_processes=%d expected_slots=%d complete=%t window=5m counts=observed-process-subset", e.deltas["initial/nonempty"], e.deltas["initial/empty"], e.deltas["initial/error"], e.deltas["search/nonempty"], e.deltas["search/empty"], e.deltas["search/error"], e.deltas["direct/nonempty"], e.deltas["direct/empty"], e.deltas["direct/error"], e.deltas["read/initial"], e.deltas["read/filters"], e.paired, e.expected, e.complete)
	initial := e.deltas["initial/nonempty"] + e.deltas["initial/empty"]
	add := func(class, severity, symptom string) {
		findings = append(findings, finding{probeId: "mimir/provider-picker", tier: severity, class: class, target: "api-fleet", sustain: 1, symptom: symptom, observed: observed,
			mechanism: "These are initial/search/direct picker model outcomes, not FindProviders2 connection candidates. Initial rows count countries, promoted groups and devices as rendered by the shared SDK; cities/regions alone do not populate the initial picker. Search emptiness can be legitimate. Counts are the paired observed-process subset, not unique users or a complete fleet denominator under partial coverage.",
			baseline:  "Initial empty PAGE requires 20 successful GET outcomes and 80% empty over five minutes. Any read/request error warns; 20 request errors page. Legitimate empty search/direct responses are not supply failures.",
			evidence:  "Eleven fixed producer children are paired with exact process/start identity, both underlying source clocks and reset/range-presence witnesses. Only finite aggregate counts leave the reducer.",
			context:   "No caller/target identity or request join is available. Initial emptiness can reflect legitimate country restrictions; source completeness does not prove UI delivery or absence of rejected metric samples.",
			action:    "Treat repeated empty initial app lists as a product availability failure. Check the initial-location cache and caller filter/alias reads, exact API artifact and request shape without exposing keys, addresses or IDs. Non-missing Redis errors must surface, not become empty successful lists. A read error can be WRONGTYPE, cancellation or decoding, not necessarily Redis outage. Preserve eligibility and country-exclusion guards. See SIGNALS.md §2.9b; selection lists are independently covered by §2.9a.",
			verify:    "Require two complete fresh five-minute windows with at least 20 initial outcomes, below the empty threshold and no read/request errors, plus the reported app surface recovering. Model return does not prove HTTP delivery or a particular device rendered it.",
			playbook:  "SIGNALS.md §2.9b"})
	}
	if initial >= 20 && e.deltas["initial/empty"]/initial >= .8 {
		add("provider-picker-effective-empty", tierPage, "App initial location picker is effectively empty")
	}
	errors := e.deltas["initial/error"] + e.deltas["search/error"] + e.deltas["direct/error"]
	if errors > 0 || e.deltas["read/initial"]+e.deltas["read/filters"] > 0 {
		severity := tierWarn
		if errors >= 20 {
			severity = tierPage
		}
		add("provider-picker-read-or-request-error", severity, "App location picker read or request failures are observed")
	}
	if !e.complete {
		findings = append(findings, providerPickerVisibility(e.reason, e.paired, e.expected))
	} else if initial < 20 {
		findings = append(findings, providerPickerVisibility("initial-traffic-below-health-floor", e.paired, e.expected))
	}
	// Classes recover independently; an unrelated read WARN must not retain a
	// disproven empty PAGE. Partial/no-traffic evidence cannot disprove either.
	if e.complete && initial >= 20 {
		findings = append(findings, healthyFinding("mimir/provider-picker", tierWarn, "provider-picker-unobservable", "api-fleet"))
		if e.deltas["initial/empty"]/initial < .8 {
			findings = append(findings, healthyFinding("mimir/provider-picker", tierPage, "provider-picker-effective-empty", "api-fleet"))
		}
		if errors == 0 && e.deltas["read/initial"]+e.deltas["read/filters"] == 0 {
			findings = append(findings, healthyFinding("mimir/provider-picker", tierWarn, "provider-picker-read-or-request-error", "api-fleet"))
		}
	}
	return findings
}
