// SIGNALS.md §2.9c observes request-local selection invariants, not global
// provider supply or the unrelated location-picker response.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const providerSelectionMaxBytes = 4 * 1024 * 1024

// Retains the existing provider-count thresholds and distinguishes proven
// selection inconsistencies from missing cache metadata in completed zeroes.
func NewProviderSelectionSignal() Signal {
	return &signalAdapter{number: "2.9c", key: "provider-selection", name: "Provider selection boundary diagnostics", probe: providerSelectionProbe{}}
}

type providerSelectionProbe struct{}

func (providerSelectionProbe) id() string             { return "mimir/provider-selection" }
func (providerSelectionProbe) tier() string           { return tierPage }
func (providerSelectionProbe) cadence() time.Duration { return time.Minute }

// Exact process joins remain private. The query retains source timestamps,
// both ends of the process generation and counter-reset witnesses.
func providerSelectionQuery(environment string, scope providerPickerScope) string {
	pattern := func(values []string) string {
		parts := append([]string(nil), values...)
		sort.Strings(parts)
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		return strconv.Quote(strings.Join(parts, "|"))
	}
	selector := `env=` + strconv.Quote(environment) + `,job="api",host=~` + pattern(scope.hosts) + `,block=~` + pattern(scope.blocks)
	parts := []string{}
	add := func(tag, expression string) {
		parts = append(parts, "label_replace(("+expression+"),\"monitor_selection\","+strconv.Quote(tag)+",\"\",\"\")")
	}
	for _, field := range []struct{ name, metric string }{
		{name: "start", metric: "process_start_time_seconds"},
		{name: "schema", metric: "urnetwork_findproviders2_selection_schema_version"},
	} {
		for _, bound := range []string{"now", "prior"} {
			metric := field.metric + "{" + selector + "}"
			if bound == "prior" {
				metric += " offset 5m"
			}
			add(bound+"_"+field.name, metric)
			add(bound+"_"+field.name+"_time", "timestamp("+metric+")")
		}
	}
	metric := "urnetwork_findproviders2_selection_outcomes_total{" + selector + "}"
	// Lazy label partitions remain exported after traffic moves elsewhere.
	// Keep response capacity for this window's positive evidence; process
	// and schema witnesses above still establish every quiet slot's authority.
	positive := "(increase(" + metric + "[5m]) > 0)"
	add("count", positive)
	add("count_time", "timestamp("+metric+") and "+positive)
	add("resets", "resets("+metric+"[5m]) and "+positive)
	add("samples", "count_over_time("+metric+"[5m]) and "+positive)
	return strings.Join(parts, " or ")
}

type providerSelectionKey struct{ targetKind, requestClass, family, rankMode, outcome, reason string }

// Contains only finite vocabulary validated before it reaches alert output.
func (self providerSelectionKey) frame() string {
	return strings.Join([]string{self.targetKind, self.family, self.rankMode, self.requestClass}, "/")
}

type providerSelectionProcess struct {
	fields   map[string]float64
	outcomes map[providerSelectionKey]map[string]float64
}

type providerSelectionEvidence struct {
	counts           map[providerSelectionKey]float64
	paired, expected int
	complete         bool
	shapePaired      int
	shapeComplete    bool
	reason           string
}

// Does not echo untrusted metric labels or source error bodies on failure.
func parseProviderSelection(raw, environment string, now time.Time, scope providerPickerScope) providerSelectionEvidence {
	evidence := providerSelectionEvidence{counts: map[providerSelectionKey]float64{}, expected: len(scope.slots), reason: "incomplete-process-window"}
	invalid := func() providerSelectionEvidence {
		return providerSelectionEvidence{expected: len(scope.slots), reason: "invalid-source-response"}
	}
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if len(raw) > providerSelectionMaxBytes || json.Unmarshal([]byte(raw), &response) != nil || response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 || len(response.Infos) != 0 || len(response.Data.Result) > 8192 {
		return invalid()
	}
	processes := map[pickerProcessKey]*providerSelectionProcess{}
	contains := func(v string, allowed string) bool { return slices.Contains(strings.Split(allowed, "|"), v) }
	for _, row := range response.Data.Result {
		at, value, err := mimirInstantValue(row.Value)
		if err != nil || at.Unix() != now.Unix() || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 9007199254740991 {
			return invalid()
		}
		key := pickerProcessKey{host: row.Metric["host"], block: row.Metric["block"], instance: row.Metric["instance"]}
		if row.Metric["env"] != environment || row.Metric["job"] != "api" || !scope.slots[key.host+"\x00"+key.block] || key.instance == "" || len(key.instance) > 512 {
			return invalid()
		}
		process := processes[key]
		if process == nil {
			process = &providerSelectionProcess{fields: map[string]float64{}, outcomes: map[providerSelectionKey]map[string]float64{}}
			processes[key] = process
		}
		field := row.Metric["monitor_selection"]
		var fields map[string]float64
		switch field {
		case "now_start", "now_start_time", "prior_start", "prior_start_time", "now_schema", "now_schema_time", "prior_schema", "prior_schema_time":
			fields = process.fields
		case "count", "count_time", "resets", "samples":
			outcome := providerSelectionKey{targetKind: row.Metric["target_kind"], requestClass: row.Metric["request_class"], family: row.Metric["ip_family"], rankMode: row.Metric["rank_mode"], outcome: row.Metric["outcome"], reason: row.Metric["reason"]}
			if !contains(outcome.targetKind, "none|direct|country|region|city|location_unknown|group|best_available|mixed") || !contains(outcome.requestClass, "default_minimum|count_zero|count_small|count_positive|forced_minimum") || !contains(outcome.family, "any|v4|v6|dualstack|unknown") || !contains(outcome.rankMode, "quality|speed|online|unknown") {
				return invalid()
			}
			validReason := false
			switch outcome.outcome {
			case "nonempty":
				validReason = contains(outcome.reason, "returned|returned_3_9|returned_10_plus|returned_small_direct|returned_small_mixed_direct|returned_small_requested|returned_small_cache_unknown|returned_small_eligible|returned_small_sample|returned_small_filtered_hard|returned_small_filtered_network|returned_small_filtered_family|returned_small_filtered_explicit|returned_small_filtered_client_ids|returned_small_filtered_destinations|returned_small_filtered_explicit_mixed|returned_small_filtered_mixed")
			case "zero":
				validReason = contains(outcome.reason, "intentional_zero|no_specs|direct_excluded|unresolved_target|backfill_unavailable|cache_page_gap|cache_missing|cache_empty|unsupported_rank|eligible_not_selected|filtered_hard|filtered_network|filtered_family|filtered_explicit|filtered_client_ids|filtered_destinations|filtered_explicit_mixed|filtered_mixed|unclassified_zero")
			case "error", "canceled":
				validReason = contains(outcome.reason, "validate|caller_location|load_primary|load_backfill|hard_exclusions|filter|directory|select|record_matches")
			}
			if !validReason {
				return invalid()
			}
			fields = process.outcomes[outcome]
			if fields == nil {
				fields = map[string]float64{}
				process.outcomes[outcome] = fields
			}
		default:
			return invalid()
		}
		if _, exists := fields[field]; exists {
			return invalid()
		}
		fields[field] = value
	}
	pairedSlots := map[string]int{}
	fresh := func(value float64, boundary time.Time) bool {
		age := float64(boundary.Unix()) - value
		return value > 0 && -30 <= age && age <= 90
	}
	for key, process := range processes {
		fields := process.fields
		schema := fields["now_schema"]
		if len(fields) != 8 || (schema != 1 && schema != 2) || fields["prior_schema"] != schema || fields["now_start"] != fields["prior_start"] || fields["now_start"] <= 0 || fields["now_start"] > float64(now.Add(-5*time.Minute).Unix()) {
			continue
		}
		if !fresh(fields["now_schema_time"], now) || !fresh(fields["now_start_time"], now) || !fresh(fields["prior_schema_time"], now.Add(-5*time.Minute)) || !fresh(fields["prior_start_time"], now.Add(-5*time.Minute)) {
			continue
		}
		valid := true
		for outcome, values := range process.outcomes {
			if len(values) != 4 || !fresh(values["count_time"], now) || values["resets"] != 0 || values["samples"] < 2 || math.Trunc(values["samples"]) != values["samples"] {
				valid = false
			}
			if outcome.outcome == "nonempty" && (outcome.reason == "returned") != (schema == 1) {
				valid = false
			}
			if schema == 1 && contains(outcome.reason, "filtered_client_ids|filtered_destinations|filtered_explicit_mixed") {
				valid = false
			}
		}
		if !valid {
			continue
		}
		pairedSlots[key.host+"\x00"+key.block]++
		evidence.paired++
		if schema == 2 {
			evidence.shapePaired++
		}
		for outcome, values := range process.outcomes {
			evidence.counts[outcome] += values["count"]
		}
	}
	evidence.complete = 0 < evidence.expected && len(pairedSlots) == evidence.expected
	for _, count := range pairedSlots {
		if count != 1 {
			evidence.complete = false
		}
	}
	if evidence.complete {
		evidence.reason = "complete"
	}
	evidence.shapeComplete = evidence.complete && evidence.shapePaired == evidence.expected
	return evidence
}

// Uses inventory-enabled API slots, not surviving series, as the denominator.
func (providerSelectionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope := pickerScope(env)
	now := env.now()
	reduce := func(evidence providerSelectionEvidence) ([]finding, error) {
		observation := newProviderSelectionObservation(env.cfg.env, now, evidence)
		env.providerSelectionObservation = &observation
		return providerSelectionFindings(evidence), nil
	}
	unknown := func(reason string) ([]finding, error) {
		return reduce(providerSelectionEvidence{expected: len(scope.slots), reason: reason})
	}
	if len(scope.slots) == 0 || len(scope.slots) > 32 || len(scope.hosts) == 0 || len(scope.blocks) == 0 {
		return unknown("inventory-unavailable-or-over-bound")
	}
	command := "curl -fsS --max-time 15 --max-filesize " + strconv.Itoa(providerSelectionMaxBytes) + " --data-urlencode " + shellSingleQuote("query="+providerSelectionQuery(env.cfg.env, scope)) + " --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(now.Unix(), 10)) + " 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	out, _, err := shellFirstServiceGateway(ctx, env.runner, env.cfg.hostsWithRole("services"), nil, command)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return unknown("bounded-source-unavailable")
	}
	return reduce(parseProviderSelection(out, env.cfg.env, now, scope))
}

// Observational zero reasons do not create a new generic scarcity threshold.
// A missing positive-count page or an eligible-but-unselected zero is narrower.
func providerSelectionFindings(evidence providerSelectionEvidence) []finding {
	findings := []finding{}
	if !evidence.complete || !evidence.shapeComplete {
		reason, frame := evidence.reason, ""
		if evidence.complete {
			reason, frame = "incomplete-shape-schema-window", "response-shape"
		}
		findings = append(findings, finding{
			probeId: "mimir/provider-selection", tier: tierWarn, class: "provider-selection-unavailable", target: "api-fleet", frame: frame, sustain: 1,
			symptom:   "Provider-selection stage and zero-reason attribution is incomplete",
			mechanism: "A fresh supported producer and unchanged process generation must cover both ends of the five-minute window for every inventory-enabled API slot. Schema 1 retains zero-reason authority; schema 2 is required for small-list and exclusion-source attribution. Old, missing, excluded, restarted, stale or reset evidence remains unknown.",
			baseline:  "One fresh same-generation schema-1 or schema-2 process per inventory-enabled API host/block, plus valid observed counter partitions; complete response-shape attribution requires schema 2 everywhere.",
			observed:  fmt.Sprintf("paired_processes=%d shape_paired_processes=%d expected_slots=%d reason=%s", evidence.paired, evidence.shapePaired, evidence.expected, reason),
			evidence:  "Bounded Mimir query; private process join, fixed schema and source timestamps. No target IDs or raw producer strings enter this alert.",
			context:   "Observed-subset invariant findings remain valid, but a missing partition cannot become healthy zero. Destination-tail exclusions can be healthy-window refill requests; client-ID exclusions can be runtime removals or durable policy. Neither proves client health, and schema-2 reasons never suppress the independent provider-count finding. Lazy counter series can omit a first event, and quiet traffic does not establish location availability.",
			action:    "Verify emitting API ancestry for the selection diagnostic schema, complete the authorized rollout and inspect scrape/generation/reset coverage. Do not change selection filters to restore telemetry.",
			verify:    "Require a complete post-convergence five-minute process window and two fresh observations; verify request outcomes separately.", playbook: "SIGNALS.md §2.9c",
		})
	}
	keys := make([]providerSelectionKey, 0, len(evidence.counts))
	for key := range evidence.counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].frame()+"/"+keys[i].reason < keys[j].frame()+"/"+keys[j].reason })
	for _, key := range keys {
		count := evidence.counts[key]
		if key.outcome != "zero" || key.requestClass == "count_zero" || key.requestClass == "forced_minimum" {
			continue
		}
		class, tier, threshold := "", tierWarn, 20.0
		switch key.reason {
		case "eligible_not_selected":
			class, tier, threshold = "provider-selection-empty-despite-eligible", tierPage, 3
		case "cache_page_gap":
			class = "provider-selection-cache-page-gap"
		case "cache_missing":
			if count >= threshold {
				findings = append(findings, providerSelectionMissingCacheFinding(evidence, key, count))
			}
			continue
		}
		if class == "" || count < threshold {
			continue
		}
		findings = append(findings, finding{
			probeId: "mimir/provider-selection", tier: tier, class: class, target: "api-fleet", frame: key.frame(), sustain: 2,
			symptom:   "Provider selection returned zero across a proven internal boundary",
			mechanism: "eligible_not_selected means a positive discovery count and known rank retained candidates after every request filter but returned none. cache_page_gap means positive cached page counts referenced absent pages in a zero response. Neither means global provider scarcity.",
			baseline:  fmt.Sprintf("At least %.0f matching completed zero responses in five minutes, sustained for two one-minute observations; intentional zero and ForceMinimum requests excluded.", threshold),
			observed:  fmt.Sprintf("requests=%.3f reason=%s target_kind=%s ip_family=%s rank_mode=%s request_class=%s paired_processes=%d expected_slots=%d scope_complete=%t", count, key.reason, key.targetKind, key.family, key.rankMode, key.requestClass, evidence.paired, evidence.expected, evidence.complete),
			evidence:  "Request-local stage and cache/filter observations from the same completed response, aggregated by fixed vocabulary; no customer, provider, network, target ID, address or request ID.",
			context:   "Backfill remains within the requested location/group, not the global online pool. A publication/expiry race can transiently miss a page; country/region/city metadata can be unknown on old caches. Sampled candidate pools are not full supply, and returned entries do not prove route success. Counts cover observed same-generation processes and may undercount newly appearing lazy labels.",
			action:    "Inspect the exact API/cache writer artifacts and the matching stage durations. For page gaps verify atomic publication/expiry and reader fallback; for eligible candidates verify count/rank/backfill selection. Preserve hard, network, family and caller exclusions; never clear caches or widen targets to silence the signal.",
			verify:    "Two complete post-boundary five-minute windows have no matching invariant failures, with positive naturally occurring selection and independent route success. Generic zero-tail attribution still needs the bounded reason distribution and request-intent baseline.", playbook: "SIGNALS.md §2.9c and §2.9a",
		})
	}
	return findings
}

// Missing target metadata is an observed response boundary, not proof of a
// publication defect or an unavailable global provider population.
func providerSelectionMissingCacheFinding(evidence providerSelectionEvidence, key providerSelectionKey, count float64) finding {
	return finding{
		probeId: "mimir/provider-selection", tier: tierWarn, class: "provider-selection-cache-missing", target: "api-fleet", frame: key.frame(), sustain: 2,
		symptom:   "Completed provider selections returned zero with missing target cache metadata",
		mechanism: "The reader observed at least one same-target cache read without usable count metadata after resolving the caller alias. The priority reason can cover a primary, alternate or online read; it does not prove every target was missing or identify the publisher, expiry, membership or request-intent cause.",
		baseline:  "At least 20 ordinary positive-intent completed zero responses in five minutes, sustained for two one-minute observations. Intentional zero-count and ForceMinimum requests are excluded.",
		observed:  fmt.Sprintf("requests=%.3f reason=cache_missing target_kind=%s ip_family=%s rank_mode=%s request_class=%s paired_processes=%d expected_slots=%d scope_complete=%t", count, key.targetKind, key.family, key.rankMode, key.requestClass, evidence.paired, evidence.expected, evidence.complete),
		evidence:  "The completed response supplies the fixed cache_missing reason. The optional provider-selection JSONL output retains every validated reason partition, source completeness and the exact evaluation window without request, group, caller or process identities.",
		context:   "A nonexistent or empty group can produce this boundary, as can missing publication or alias metadata; none is established by this aggregate. A nonempty country cache or a different group is not an affected-target control. Partial sources and absent lazy reason partitions cannot establish recovery. The independent provider-count and picker findings remain active.",
		action:    "Privately bind one naturally failing request to its actual group or location, resolved caller location and request intent. Use bounded exact-target metadata and membership evidence, then repair the proven publication or intent owner. Preserve target scope and all safety filters.",
		verify:    "Require positive naturally occurring responses for the same target and caller, two complete fresh reason windows and independent route success. Missing alerts, quiet traffic, unrelated healthy groups or source-unavailable windows cannot close the incident.",
		playbook:  "SIGNALS.md §2.9c and §2.9a",
	}
}
