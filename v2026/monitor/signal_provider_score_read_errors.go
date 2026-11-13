// SIGNALS.md §2.9e observes score-loader phase failures independently of completed responses.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	providerScoreReadCounter      = "urnetwork_client_score_read_errors_total"
	providerScoreReadSuccess      = "urnetwork_findproviders2_load_seconds_count"
	providerScoreReadWindow       = 15 * time.Minute
	providerScoreReadMaxBytes     = 4 * 1024 * 1024
	providerScoreReadMaxRows      = 2048
	providerScoreReadMaxSamples   = 128
	providerScoreReadExactInteger = 9007199254740991.0
)

func NewProviderScoreReadErrorsSignal() Signal {
	return &signalAdapter{number: "2.9e", key: "provider-score-read-errors", name: "Provider score-loader read errors", probe: providerScoreReadErrorsProbe{}}
}

type providerScoreReadErrorsProbe struct{}

func (providerScoreReadErrorsProbe) id() string               { return "mimir/provider-score-read-errors" }
func (providerScoreReadErrorsProbe) tier() string             { return tierWarn }
func (providerScoreReadErrorsProbe) cadence() time.Duration   { return 15 * time.Minute }
func (providerScoreReadErrorsProbe) runBudget() time.Duration { return time.Minute }

// Reuse the accepted placement and native census contract, restricted to API.
// This copy changes no shared inventory and cannot enroll a service implicitly.
func providerScoreReadScope(env *probeEnv) jwtRejectionsScope {
	found := false
	for _, service := range env.cfg.logServices {
		found = found || service == "api"
	}
	if !found {
		return jwtRejectionsScope{slots: map[jwtRejectionsSlot]bool{}}
	}
	cfg := *env.cfg
	cfg.logServices = []string{"api"}
	copyEnv := *env
	copyEnv.cfg = &cfg
	return jwtRejectionsInventory(&copyEnv)
}

// A matrix retains every raw source timestamp and generation. No increase,
// aggregation, reset repair or completed-response denominator enters this read.
func providerScoreReadQuery(environment string, scope jwtRejectionsScope) string {
	pattern := func(values []string) string {
		parts := append([]string(nil), values...)
		sort.Strings(parts)
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		return strconv.Quote(strings.Join(parts, "|"))
	}
	return fmt.Sprintf(`{__name__=~"process_start_time_seconds|%s|%s",env=%s,job="api",service="api",host=~%s,block=~%s,phase=~"counts|samples|"}[15m]`, providerScoreReadCounter, providerScoreReadSuccess, strconv.Quote(environment), pattern(scope.hosts), pattern(scope.blocks))
}

type providerScoreReadProcess struct {
	fields  map[string][]jwtRejectionsSample
	invalid map[string]bool
}

type providerScoreReadEvidence struct {
	counts, samples, successes float64
	expected, observed, paired int
	complete, exercised        bool
	reason                     string
}

func (providerScoreReadErrorsProbe) check(ctx context.Context, env *probeEnv) (findings []finding, resultErr error) {
	var sourceErr error
	defer func() {
		if ended := ctx.Err(); ended != nil {
			findings = nil
			if sourceErr != nil {
				resultErr = &signalContextError{contextErr: ended, sourceErr: sourceErr}
			} else {
				resultErr = ended
			}
		}
	}()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	scope := providerScoreReadScope(env)
	evidence := providerScoreReadEvidence{expected: len(scope.slots), reason: "inventory-unavailable-or-over-bound"}
	if len(scope.slots) == 0 || len(scope.slots) > jwtRejectionsMaxSlots || len(scope.hosts) == 0 || len(scope.hosts) > 16 || len(scope.blocks) == 0 {
		return providerScoreReadFindings(evidence), nil
	}
	current := func() bool {
		if ctx.Err() != nil {
			return false
		}
		if env.cfg.routerGenerationCheck == nil {
			return true
		}
		ok, err := env.cfg.routerGenerationCheck(ctx)
		sourceErr = errors.Join(sourceErr, err)
		return ok && err == nil && ctx.Err() == nil
	}
	if !current() {
		evidence.reason = "settings-generation"
		return providerScoreReadFindings(evidence), nil
	}
	now := env.now().UTC().Truncate(time.Second)
	// This shared context includes queue time and all four joined workers.
	// The existing census program and its per-command ceilings are unchanged.
	censusCtx, cancelCensus := context.WithTimeout(ctx, 30*time.Second)
	census := readJwtRejectionsCensus(censusCtx, env, scope, now)
	sourceErr = errors.Join(sourceErr, census.err, censusCtx.Err())
	if censusCtx.Err() != nil {
		census.complete = false
	}
	cancelCensus()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !current() {
		evidence.reason = "settings-generation"
		return providerScoreReadFindings(evidence), nil
	}
	raw, err := readProviderMimirQuery(ctx, env, providerScoreReadQuery(env.cfg.env, scope), now, providerScoreReadMaxBytes)
	sourceErr = errors.Join(sourceErr, err)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		evidence.reason = "bounded-source-unavailable"
		findings = providerScoreReadFindings(evidence)
		findings[0].observed += " " + observationFailureDetails(sourceErr, classifyObservationError(sourceErr)) + mimirQueryFailureDetails(err)
		return findings, nil
	}
	if !current() {
		evidence.reason = "settings-generation"
		return providerScoreReadFindings(evidence), nil
	}
	evidence = parseProviderScoreReads(raw, env.cfg.env, now, env.now().UTC(), scope, census)
	findings = providerScoreReadFindings(evidence)
	if sourceErr != nil {
		for i := range findings {
			if findings[i].class == "provider-score-read-errors-unobservable" {
				findings[i].observed += " " + observationFailureDetails(sourceErr, classifyObservationError(sourceErr))
			}
		}
	}
	return findings, nil
}

// Native Prometheus matrix points encode a numeric timestamp and string value.
// Validate those domains before clock conversion; counters are exact integers.
func providerScoreReadPoints(raw [][]json.RawMessage, now time.Time, counter bool) ([]jwtRejectionsSample, bool) {
	if len(raw) < 2 || len(raw) > providerScoreReadMaxSamples {
		return nil, false
	}
	end := float64(now.Unix())
	points := make([]jwtRejectionsSample, 0, len(raw))
	for _, point := range raw {
		var at float64
		var encoded string
		if len(point) != 2 || json.Unmarshal(point[0], &at) != nil || json.Unmarshal(point[1], &encoded) != nil {
			return nil, false
		}
		value, err := strconv.ParseFloat(encoded, 64)
		if err != nil || math.IsNaN(at) || math.IsInf(at, 0) || at <= end-providerScoreReadWindow.Seconds() || at > end || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > providerScoreReadExactInteger || counter && math.Trunc(value) != value || len(points) > 0 && at <= points[len(points)-1].at {
			return nil, false
		}
		points = append(points, jwtRejectionsSample{at: at, value: value})
	}
	return points, true
}

func providerScoreReadStart(process *providerScoreReadProcess, now, completed time.Time) (float64, bool) {
	points := process.fields["start"]
	if process.invalid["*"] || process.invalid["start"] || len(points) < 2 {
		return 0, false
	}
	first, last := points[0], points[len(points)-1]
	begin, end := float64(now.Unix())-providerScoreReadWindow.Seconds(), float64(now.Unix())
	if first.at-begin > 30 || end-last.at > 30 || float64(completed.UnixMilli())/1000-last.at > 90 || last.at-first.at < providerScoreReadWindow.Seconds()-60 || first.value <= 0 || first.value > begin {
		return 0, false
	}
	for i, point := range points {
		if point.value != first.value || i > 0 && point.at-points[i-1].at > 45 {
			return 0, false
		}
	}
	return first.value, true
}

func providerScoreReadDelta(process *providerScoreReadProcess, field string) (float64, bool) {
	points, starts := process.fields[field], process.fields["start"]
	if process.invalid[field] || len(points) != len(starts) || len(points) < 2 {
		return 0, false
	}
	for i, point := range points {
		if point.at != starts[i].at || i > 0 && point.value < points[i-1].value {
			return 0, false
		}
	}
	return points[len(points)-1].value - points[0].value, true
}

// Quarantine malformed owners, not independent complete sibling witnesses.
// A complete native roster and positive read activity are needed for recovery;
// a proved phase increment survives an unrelated missing phase or process.
func parseProviderScoreReads(raw, environment string, now, completed time.Time, scope jwtRejectionsScope, census jwtRejectionsCensus) providerScoreReadEvidence {
	evidence := providerScoreReadEvidence{expected: len(scope.slots), reason: "invalid-source-response"}
	var response struct {
		mimirRangeResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if len(raw) > providerScoreReadMaxBytes || !providerConnectionsJsonShape(raw) || json.Unmarshal([]byte(raw), &response) != nil || response.Status != "success" || response.Data.ResultType != "matrix" || len(response.Warnings) != 0 || len(response.Infos) != 0 || len(response.Data.Result) > providerScoreReadMaxRows || completed.Before(now) || completed.Sub(now) > time.Minute {
		return evidence
	}
	evidence.complete, evidence.exercised, evidence.reason = scope.complete && census.complete, true, "incomplete-process-window"
	processes := map[jwtRejectionsProcessKey]*providerScoreReadProcess{}
	for _, row := range response.Data.Result {
		labels := row.Metric
		key := jwtRejectionsProcessKey{slot: jwtRejectionsSlot{service: labels["service"], host: labels["host"], block: labels["block"]}, instance: labels["instance"]}
		validIdentity := labels["env"] == environment && labels["job"] == "api" && key.slot.service == "api" && scope.slots[key.slot] && len(key.instance) == 8
		for _, c := range key.instance {
			validIdentity = validIdentity && (c >= '0' && c <= '9' || c >= 'a' && c <= 'f')
		}
		if !validIdentity {
			evidence.complete = false
			continue
		}
		process := processes[key]
		if process == nil {
			process = &providerScoreReadProcess{fields: map[string][]jwtRejectionsSample{}, invalid: map[string]bool{}}
			processes[key] = process
		}
		field := ""
		switch labels["__name__"] {
		case jwtRejectionsStart:
			if labels["phase"] == "" {
				field = "start"
			}
		case providerScoreReadSuccess:
			if labels["phase"] == "" {
				field = "success"
			}
		case providerScoreReadCounter:
			if labels["phase"] == "counts" || labels["phase"] == "samples" {
				field = labels["phase"]
			}
		}
		for name, value := range labels {
			if len(value) > 512 || (name != "__name__" && name != "env" && name != "job" && name != "service" && name != "host" && name != "block" && name != "instance" && name != "phase") {
				field = ""
			}
		}
		if field == "" {
			process.invalid["*"] = true
			continue
		}
		points, valid := providerScoreReadPoints(row.Values, now, field != "start")
		if !valid || process.fields[field] != nil {
			process.invalid[field] = true
		}
		if process.fields[field] == nil {
			process.fields[field] = points
		}
	}
	evidence.observed = len(processes)
	bySlot := map[jwtRejectionsSlot][]jwtRejectionsProcessKey{}
	for key := range processes {
		bySlot[key.slot] = append(bySlot[key.slot], key)
	}
	for slot, allowed := range scope.slots {
		keys := bySlot[slot]
		if !allowed || len(keys) == 0 || len(keys) > jwtRejectionsMaxGenerations || len(census.starts[slot]) == 0 {
			evidence.complete = false
		}
		for i, key := range keys {
			if len(keys) > jwtRejectionsMaxGenerations {
				processes[key].invalid["*"] = true
			}
			a := processes[key].fields["start"]
			for _, other := range keys[i+1:] {
				b := processes[other].fields["start"]
				if len(a) > 0 && len(b) > 0 && math.Abs(a[0].value-b[0].value) <= 0.001 {
					processes[key].invalid["*"] = true
					processes[other].invalid["*"] = true
				}
			}
		}
		for _, nativeStart := range census.starts[slot] {
			matches := 0
			for _, key := range keys {
				start, valid := providerScoreReadStart(processes[key], now, completed)
				if valid && math.Abs(start-nativeStart) <= 0.001 {
					matches++
				}
			}
			if matches != 1 {
				evidence.complete = false
			}
		}
	}
	keys := make([]jwtRejectionsProcessKey, 0, len(processes))
	for key := range processes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.slot.host != b.slot.host {
			return a.slot.host < b.slot.host
		}
		if a.slot.block != b.slot.block {
			return a.slot.block < b.slot.block
		}
		return a.instance < b.instance
	})
	for _, key := range keys {
		process := processes[key]
		start, valid := providerScoreReadStart(process, now, completed)
		matches := 0
		for _, nativeStart := range census.starts[key.slot] {
			if math.Abs(start-nativeStart) <= 0.001 {
				matches++
			}
		}
		if !valid || matches != 1 {
			evidence.complete = false
			continue
		}
		paired := true
		for _, field := range []string{"counts", "samples", "success"} {
			delta, ok := providerScoreReadDelta(process, field)
			if !ok {
				paired = false
				continue
			}
			total := &evidence.counts
			if field == "samples" {
				total = &evidence.samples
			} else if field == "success" {
				total = &evidence.successes
				evidence.exercised = evidence.exercised && delta > 0
			}
			if delta > providerScoreReadExactInteger-*total {
				paired = false
				continue
			}
			*total += delta
		}
		if paired {
			evidence.paired++
		} else {
			evidence.complete = false
		}
	}
	if evidence.paired == 0 {
		evidence.complete = false
	}
	if evidence.complete {
		evidence.reason = ""
	}
	return evidence
}

func providerScoreReadFindings(e providerScoreReadEvidence) []finding {
	findings := []finding{}
	observed := fmt.Sprintf("scope=api-score-loader counts_failed_pipelines=%.0f samples_failed_pipelines=%.0f successful_primary_loads=%.0f expected_slots=%d observed_processes=%d paired_processes=%d window=15m", e.counts, e.samples, e.successes, e.expected, e.observed, e.paired)
	if e.counts > 0 || e.samples > 0 {
		findings = append(findings, finding{
			probeId: "mimir/provider-score-read-errors", tier: tierWarn, class: "provider-score-read-errors", target: "api-fleet", frame: "score-load", sustain: 2,
			symptom:   "Provider score-loader pipelines returned non-missing read errors",
			mechanism: "The counts or samples counter increased between paired raw samples of the same native-matched API process generation. Primary and optional other-rank backfill loads share these counters; cancellation, deadlines or client lifecycle can cause a counted error.",
			baseline:  "No observed phase increment in a complete exercised process window. The warning requires two consecutive fifteen-minute observations with a proven positive delta; it has no error-rate denominator.",
			observed:  observed,
			evidence:  "Only finite phase totals and coverage counts leave the reducer. Process-start and both phase counters retain source timestamps; each admitted positive has monotone samples paired with an unchanged start and the read-only native process census.",
			context:   "These are failed loader pipelines, not failed keys, unique requests, provider scarcity or proof of a fleet Redis outage. Optional backfill can fail while retaining a valid primary response. PING and decoding failures are outside these counters. Successful primary loads are a separate activity control, never an attempted-phase denominator.",
			action:    "Correlate the same interval with the existing read-path source failures, caller cancellation/deadline, connection lifecycle and independent Redis latency/availability evidence. Keep provider-count, cache population and score-writer findings separate. Do not clear score keys, relax selection gates or generate requests to change the counters.",
			verify:    "Require two complete native-matched process windows with zero phase deltas and positive successful-primary-load controls before notifier recovery. Restore missing process telemetry separately; quiet traffic or completed-response counts alone do not prove reader recovery.",
			playbook:  "SIGNALS.md §2.9e and §2.9a",
		})
	}
	if !e.complete || len(findings) == 0 && !e.exercised {
		reason := e.reason
		if reason == "" {
			reason = "no-successful-primary-control"
		}
		findings = append(findings, finding{
			probeId: "mimir/provider-score-read-errors", tier: tierWarn, class: "provider-score-read-errors-unobservable", target: "api-fleet", frame: "score-load", sustain: 2,
			symptom:   "Provider score-reader observation is incomplete",
			mechanism: "The configured API inventory, native process census, source window, process generation, paired phase counters or successful-primary-read activity did not establish a complete recovery observation.",
			baseline:  "Every enabled expected API slot and every current native generation has complete same-push process/start and both phase histories, no reset or gap, and successful primary read activity for recovery.",
			observed:  observed + " reason=" + reason,
			evidence:  "Missing, stale, malformed, duplicate, reset or generation-mismatched data remains unknown. Independently proved positive phase deltas remain visible beside this warning; private labels and raw source/error text are omitted.",
			context:   "Counter preinitialization proves a zero is exported before failures; an absent counter is not zero. A completed-response denominator can omit primary read failures and cannot certify source coverage. This visibility class grants no notifier recovery credit.",
			action:    "Restore the existing stats-push, authenticated Mimir and read-only process-census boundaries for the configured API placement. Verify actual deployed metric support; do not treat an image tag, one visible sibling or healthy score-writer phase as complete reader coverage.",
			verify:    "Observe complete unchanged process generations over a later fifteen-minute window. Require paired zero error deltas plus positive successful-primary-read controls for recovery; do not manufacture traffic or infer nonempty provider supply.",
			playbook:  "SIGNALS.md §2.9e",
		})
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("mimir/provider-score-read-errors", tierWarn, "provider-score-read-errors", "api-fleet"))
	}
	return findings
}
