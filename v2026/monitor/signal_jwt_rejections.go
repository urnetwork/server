// Read signature-class JWT rejection rates without inspecting credentials or
// treating public TLS success as evidence about a process's verification keys.
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
	jwtRejectionsWindow         = 5 * time.Minute
	jwtRejectionsMaxSlots       = 128
	jwtRejectionsMaxGenerations = 4
	jwtRejectionsMaxRows        = 1024
	jwtRejectionsMaxSamples     = 64
	jwtRejectionsMaxBytes       = 4 * 1024 * 1024
	jwtRejectionsCounter        = "urnetwork_auth_jwt_rejections_total"
	jwtRejectionsStart          = "process_start_time_seconds"
)

// SIGNALS.md §18.2a implements the defined total and per-verifier arms. The
// distributed-host step arm has no approved baseline and remains unimplemented.
func NewJwtRejectionsSignal() Signal {
	return &signalAdapter{
		number: "18.2a", key: "jwt-rejections", name: "JWT signature rejection rates",
		probe: jwtRejectionsProbe{},
	}
}

// Every run reads a whole source window; no persisted counters or tick state.
type jwtRejectionsProbe struct{}

func (jwtRejectionsProbe) id() string               { return "auth/jwt-rejections" }
func (jwtRejectionsProbe) tier() string             { return tierWarn }
func (jwtRejectionsProbe) cadence() time.Duration   { return time.Minute }
func (jwtRejectionsProbe) runBudget() time.Duration { return 60 * time.Second }

// Desired placement is independent of the metric response. Each bool says
// whether the slot can be observed under current host exclusions.
type jwtRejectionsSlot struct{ service, host, block string }
type jwtRejectionsScope struct {
	slots                   map[jwtRejectionsSlot]bool
	services, hosts, blocks []string
	complete                bool
}

// These Server service roles own the session rejection producer. Alt remains
// optional; non-Server frontends and Grafana do not export the session counter.
func jwtRejectionsService(service string) bool {
	switch service {
	case "api", "connect", "alt", "proxy", "taskworker", "competitionworker":
		return true
	default:
		return false
	}
}

// An absent optional Alt service creates no requirement. An enrolled service
// with missing placement, and temporary exclusions, cannot become empty health.
func jwtRejectionsInventory(env *probeEnv) jwtRejectionsScope {
	scope := jwtRejectionsScope{slots: map[jwtRejectionsSlot]bool{}, complete: len(env.cfg.logServices) > 0}
	excluded := map[string]bool{}
	if runner, ok := env.runner.(*hostScopeRunner); ok {
		for _, name := range runner.excludedNames() {
			excluded[name] = true
		}
	}
	enabled, disabled := map[string]bool{}, map[string]bool{}
	for _, h := range env.cfg.scopeHosts() {
		enabled[h.name] = enabled[h.name] || !h.disabled
		disabled[h.name] = disabled[h.name] || h.disabled
	}
	services, hosts, blocks := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, service := range env.cfg.logServices {
		if !jwtRejectionsService(service) {
			continue
		}
		serviceHosts, hasHosts := env.cfg.logServiceHosts[service]
		serviceBlocks, hasBlocks := env.cfg.logServiceBlocks[service]
		if service == "alt" && hasHosts && len(serviceHosts) == 0 {
			continue
		}
		if !hasHosts || !hasBlocks || len(serviceBlocks) == 0 || len(serviceHosts) == 0 {
			scope.complete = false
			continue
		}
		for _, name := range serviceHosts {
			if disabled[name] && !enabled[name] {
				continue
			}
			allowed := enabled[name] && !disabled[name] && !excluded[name]
			for _, block := range serviceBlocks {
				scope.slots[jwtRejectionsSlot{service: service, host: name, block: block}] = allowed
				if allowed {
					services[service], hosts[name], blocks[block] = true, true, true
				}
			}
		}
	}
	for value := range services {
		scope.services = append(scope.services, value)
	}
	for value := range hosts {
		scope.hosts = append(scope.hosts, value)
	}
	for value := range blocks {
		scope.blocks = append(scope.blocks, value)
	}
	sort.Strings(scope.services)
	sort.Strings(scope.hosts)
	sort.Strings(scope.blocks)
	return scope
}

// A raw range selector returns actual Grafana ingress timestamps, not query
// evaluation timestamps. The empty cause alternative admits only start gauges;
// the decoder separately requires signature on every rejection counter.
func jwtRejectionsQuery(environment string, scope jwtRejectionsScope) string {
	pattern := func(values []string) string {
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = regexp.QuoteMeta(value)
		}
		return strconv.Quote(strings.Join(parts, "|"))
	}
	return fmt.Sprintf(`{__name__=~"process_start_time_seconds|urnetwork_auth_jwt_rejections_total",env=%s,service=~%s,host=~%s,block=~%s,cause=~"signature|"}[5m]`,
		strconv.Quote(environment), pattern(scope.services), pattern(scope.hosts), pattern(scope.blocks))
}

// Source identities stay private; findings carry only finite aggregate counts.
type jwtRejectionsProcessKey struct {
	slot     jwtRejectionsSlot
	instance string
}
type jwtRejectionsSample struct{ at, value float64 }
type jwtRejectionsProcess struct {
	starts, counters []jwtRejectionsSample
	rate             float64
	hasRate          bool
	invalid          bool
}
type jwtRejectionsEvidence struct {
	expected, observed, qualified, verifierBreaches int
	totalRate, maxVerifierRate                      float64
	complete                                        bool
	reason                                          string
}

// A bounded host census and two authenticated metric reads share one finite
// run budget. Native metric reads retain the settings generation fence.
func (self jwtRejectionsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope := jwtRejectionsInventory(env)
	evidence := jwtRejectionsEvidence{expected: len(scope.slots), reason: "inventory-unavailable-or-over-bound"}
	if len(scope.slots) == 0 || len(scope.slots) > jwtRejectionsMaxSlots || len(scope.hosts) == 0 || len(scope.services) == 0 || len(scope.blocks) == 0 {
		return jwtRejectionsFindings(evidence), nil
	}
	now := env.now().UTC().Truncate(time.Second)
	census := readJwtRejectionsCensus(ctx, env, scope, now)
	if ctx.Err() != nil {
		if census.err != nil {
			return nil, &signalContextError{contextErr: ctx.Err(), sourceErr: census.err}
		}
		return nil, ctx.Err()
	}
	if errors.Is(census.err, context.Canceled) {
		return nil, &signalContextError{contextErr: context.Canceled, sourceErr: census.err}
	}
	out, err := readProviderMimirQuery(ctx, env, jwtRejectionsQuery(env.cfg.env, scope), now, jwtRejectionsMaxBytes)
	if ctx.Err() != nil {
		if err != nil {
			return nil, &signalContextError{contextErr: ctx.Err(), sourceErr: err}
		}
		return nil, ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return nil, &signalContextError{contextErr: context.Canceled, sourceErr: err}
	}
	if err != nil {
		evidence.reason = "bounded-source-unavailable"
		findings := jwtRejectionsFindings(evidence)
		errorClass := classifyObservationError(err)
		findings[0].observed += " " + observationFailureDetails(err, errorClass) + mimirQueryFailureDetails(err)
		findings[0].action = observationFailureAction(err, errorClass, findings[0].action)
		return findings, nil
	}
	// Keep the rate expression explicit and independently reviewable. It shares
	// the same environment/placement and evaluation time as the raw witnesses.
	rateQuery := jwtRejectionsRateQuery(env.cfg.env, scope)
	rateOut, rateErr := readProviderMimirQuery(ctx, env, rateQuery, now, jwtRejectionsMaxBytes)
	if ctx.Err() != nil {
		if rateErr != nil {
			return nil, &signalContextError{contextErr: ctx.Err(), sourceErr: rateErr}
		}
		return nil, ctx.Err()
	}
	if errors.Is(rateErr, context.Canceled) {
		return nil, &signalContextError{contextErr: context.Canceled, sourceErr: rateErr}
	}
	evidence = parseJwtRejections(out, rateOut, env.cfg.env, now, scope, census)
	findings := jwtRejectionsFindings(evidence)
	for i := range findings {
		if findings[i].class != "jwt-rejections-unobservable" {
			continue
		}
		for _, sourceErr := range []error{census.err, rateErr} {
			if sourceErr != nil {
				findings[i].observed += " " + observationFailureDetails(sourceErr, classifyObservationError(sourceErr)) + mimirQueryFailureDetails(sourceErr)
			}
		}
	}
	return findings, nil
}

// Preserve the catalog's actual PromQL rate semantics; raw witnesses below
// qualify the returned value and never replace it with a local estimator.
func jwtRejectionsRateQuery(environment string, scope jwtRejectionsScope) string {
	query := jwtRejectionsQuery(environment, scope)
	query = strings.Replace(query, `__name__=~"process_start_time_seconds|urnetwork_auth_jwt_rejections_total",`, "", 1)
	query = strings.Replace(query, `cause=~"signature|"`, `cause="signature"`, 1)
	return "rate(" + jwtRejectionsCounter + query + ")"
}

// Validate each row independently after the envelope. Partial query warnings
// block completeness while preserving independently valid positive processes.
func parseJwtRejections(raw, rateRaw, environment string, now time.Time, scope jwtRejectionsScope, census jwtRejectionsCensus) jwtRejectionsEvidence {
	evidence := jwtRejectionsEvidence{expected: len(scope.slots), reason: "invalid-source-response"}
	var response struct {
		mimirRangeResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if len(raw) > jwtRejectionsMaxBytes || json.Unmarshal([]byte(raw), &response) != nil || response.Status != "success" || response.Data.ResultType != "matrix" || len(response.Data.Result) > jwtRejectionsMaxRows {
		return evidence
	}
	evidence.complete = scope.complete && census.complete && len(response.Warnings) == 0 && len(response.Infos) == 0
	evidence.reason = "incomplete-process-window"
	processes := map[jwtRejectionsProcessKey]*jwtRejectionsProcess{}
	for _, row := range response.Data.Result {
		labels := row.Metric
		key := jwtRejectionsProcessKey{slot: jwtRejectionsSlot{service: labels["service"], host: labels["host"], block: labels["block"]}, instance: labels["instance"]}
		if labels["env"] != environment || labels["job"] != key.slot.service || !scope.slots[key.slot] || len(key.instance) != 8 {
			evidence.complete = false
			continue
		}
		validInstance := true
		for _, c := range key.instance {
			if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f')) {
				validInstance = false
			}
		}
		if !validInstance {
			evidence.complete = false
			continue
		}
		process := processes[key]
		if process == nil {
			process = &jwtRejectionsProcess{}
			processes[key] = process
		}
		metric := labels["__name__"]
		valid := (metric == jwtRejectionsStart && labels["cause"] == "") || (metric == jwtRejectionsCounter && labels["cause"] == "signature")
		for name, value := range labels {
			if len(value) > 512 || (name != "__name__" && name != "env" && name != "job" && name != "service" && name != "host" && name != "block" && name != "instance" && name != "cause") {
				valid = false
			}
		}
		samples, ok := jwtRejectionsSamples(row.Values, now)
		if !valid || !ok {
			process.invalid = true
			continue
		}
		if metric == jwtRejectionsStart {
			if process.starts != nil {
				process.invalid = true
			}
			process.starts = samples
		} else {
			if process.counters != nil {
				process.invalid = true
			}
			process.counters = samples
		}
	}
	evidence.observed = len(processes)
	bySlot := map[jwtRejectionsSlot][]jwtRejectionsProcessKey{}
	for key := range processes {
		bySlot[key.slot] = append(bySlot[key.slot], key)
	}
	for slot, allowed := range scope.slots {
		keys := bySlot[slot]
		if !allowed || len(keys) == 0 || len(keys) > jwtRejectionsMaxGenerations {
			evidence.complete = false
			if len(keys) > jwtRejectionsMaxGenerations {
				for _, key := range keys {
					processes[key].invalid = true
				}
			}
		}
		// A second nonce for the same slot/start cannot be distinguished from
		// duplicate publication. Neither copy may inflate the rate.
		starts := map[float64][]jwtRejectionsProcessKey{}
		for _, key := range keys {
			process := processes[key]
			if len(process.starts) > 0 {
				start := process.starts[0].value
				starts[start] = append(starts[start], key)
			}
		}
		for _, sameStart := range starts {
			if len(sameStart) > 1 {
				for _, key := range sameStart {
					processes[key].invalid = true
				}
			}
		}
	}
	if !readJwtRejectionsRates(rateRaw, environment, now, scope, processes) {
		evidence.complete = false
	}
	for slot, starts := range census.starts {
		for _, start := range starts {
			matches := 0
			for _, key := range bySlot[slot] {
				process := processes[key]
				if len(process.starts) > 0 && math.Abs(process.starts[0].value-start) <= 0.001 {
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
		if a.slot.service != b.slot.service {
			return a.slot.service < b.slot.service
		}
		if a.slot.host != b.slot.host {
			return a.slot.host < b.slot.host
		}
		if a.slot.block != b.slot.block {
			return a.slot.block < b.slot.block
		}
		return a.instance < b.instance
	})
	compensation := float64(0)
	for _, key := range keys {
		process := processes[key]
		witnessRate, ok := jwtRejectionsRate(process, now)
		if !process.hasRate || math.Abs(process.rate-witnessRate) > 1e-9*math.Max(1, witnessRate) {
			ok = false
		}
		if !ok {
			evidence.complete = false
			continue
		}
		currentMatches := 0
		for _, start := range census.starts[key.slot] {
			if math.Abs(process.starts[0].value-start) <= 0.001 {
				currentMatches++
			}
		}
		if currentMatches != 1 {
			evidence.complete = false
		}
		rate := process.rate
		evidence.qualified++
		adjusted := rate - compensation
		next := evidence.totalRate + adjusted
		compensation = (next - evidence.totalRate) - adjusted
		evidence.totalRate = next
		if key.slot.service == "connect" || key.slot.service == "alt" || key.slot.service == "proxy" {
			evidence.maxVerifierRate = math.Max(evidence.maxVerifierRate, rate)
			if rate > 1 {
				evidence.verifierBreaches++
			}
		}
	}
	if evidence.complete {
		evidence.reason = ""
	}
	return evidence
}

// Accept one engine rate per exact process; duplicate or malformed rate rows
// quarantine their owner while valid sibling values retain positive authority.
func readJwtRejectionsRates(raw, environment string, now time.Time, scope jwtRejectionsScope, processes map[jwtRejectionsProcessKey]*jwtRejectionsProcess) bool {
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if len(raw) > jwtRejectionsMaxBytes || json.Unmarshal([]byte(raw), &response) != nil || response.Status != "success" || response.Data.ResultType != "vector" || len(response.Data.Result) > jwtRejectionsMaxRows {
		return false
	}
	complete := len(response.Warnings) == 0 && len(response.Infos) == 0
	for _, row := range response.Data.Result {
		labels := row.Metric
		key := jwtRejectionsProcessKey{slot: jwtRejectionsSlot{service: labels["service"], host: labels["host"], block: labels["block"]}, instance: labels["instance"]}
		process := processes[key]
		if process == nil {
			complete = false
			continue
		}
		at, value, err := mimirInstantValue(row.Value)
		valid := err == nil && at.Equal(now) && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 9007199254740991 && labels["env"] == environment && labels["job"] == key.slot.service && labels["cause"] == "signature" && scope.slots[key.slot]
		for name, label := range labels {
			if len(label) > 512 || (name != "env" && name != "job" && name != "service" && name != "host" && name != "block" && name != "instance" && name != "cause") {
				valid = false
			}
		}
		if !valid || process.hasRate {
			process.invalid = true
			complete = false
			continue
		}
		process.rate, process.hasRate = value, true
	}
	return complete
}

// Source ingress points are finite, strictly ordered, inside the requested
// window and bounded before allocation. A nonnumeric or duplicate point is not
// dropped silently; it invalidates the owning process window.
func jwtRejectionsSamples(raw [][]json.RawMessage, now time.Time) ([]jwtRejectionsSample, bool) {
	if len(raw) < 2 || len(raw) > jwtRejectionsMaxSamples {
		return nil, false
	}
	end := float64(now.Unix())
	start := end - jwtRejectionsWindow.Seconds()
	samples := make([]jwtRejectionsSample, 0, len(raw))
	for _, point := range raw {
		at, value, err := mimirInstantValue(point)
		stamp := float64(at.UnixNano()) / 1e9
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 9007199254740991 || stamp <= start || stamp > end || (len(samples) > 0 && stamp <= samples[len(samples)-1].at) {
			return nil, false
		}
		samples = append(samples, jwtRejectionsSample{at: stamp, value: value})
	}
	return samples, true
}

// Pair every counter point with the same ingress scrape's process start. The
// 15-second publisher permits 30-second endpoint lag and 45-second interior
// gaps; larger omissions, any reset, warm process or changing start are unknown.
// For complete monotone windows, use Prometheus's counter-rate extrapolation
// including the nonnegative counter's zero-point bound, without reset repair.
func jwtRejectionsRate(process *jwtRejectionsProcess, now time.Time) (float64, bool) {
	if process.invalid || len(process.starts) < 2 || len(process.starts) != len(process.counters) {
		return 0, false
	}
	first, last := process.counters[0], process.counters[len(process.counters)-1]
	windowStart := float64(now.Unix()) - jwtRejectionsWindow.Seconds()
	if first.at-windowStart > 30 || float64(now.Unix())-last.at > 30 || last.at-first.at < 240 {
		return 0, false
	}
	start := process.starts[0].value
	if start <= 0 || start > windowStart {
		return 0, false
	}
	for i, counter := range process.counters {
		if process.starts[i].at != counter.at || process.starts[i].value != start {
			return 0, false
		}
		if i > 0 && (counter.value < process.counters[i-1].value || counter.at-process.counters[i-1].at > 45) {
			return 0, false
		}
	}
	delta := last.value - first.value
	interval := last.at - first.at
	averageInterval := interval / float64(len(process.counters)-1)
	toStart, toEnd := first.at-windowStart, float64(now.Unix())-last.at
	threshold := averageInterval * 1.1
	if toStart >= threshold {
		toStart = averageInterval / 2
	}
	if toEnd >= threshold {
		toEnd = averageInterval / 2
	}
	if delta > 0 {
		toStart = math.Min(toStart, interval*(first.value/delta))
	}
	rate := delta * (interval + toStart + toEnd) / interval / jwtRejectionsWindow.Seconds()
	return rate, !math.IsNaN(rate) && !math.IsInf(rate, 0)
}

// A valid partial cohort can prove either strict breach. It cannot clear an
// incident, and neither arm diagnoses a key mismatch or authorizes a restart.
func jwtRejectionsFindings(evidence jwtRejectionsEvidence) []finding {
	findings := []finding{}
	observed := fmt.Sprintf("window=5m expected_slots=%d observed_generations=%d qualified_generations=%d source_complete=%t observed_signature_rate_per_s=%.6f max_verifier_rate_per_s=%.6f verifier_breaches=%d", evidence.expected, evidence.observed, evidence.qualified, evidence.complete, evidence.totalRate, evidence.maxVerifierRate, evidence.verifierBreaches)
	if evidence.totalRate > 10 || evidence.verifierBreaches > 0 {
		findings = append(findings, finding{
			probeId: "auth/jwt-rejections", tier: tierWarn, class: "jwt-signature-rejection-rate", target: "jwt-fleet", sustain: 1,
			symptom:   "JWT signature-class rejections exceed a defined five-minute rate threshold",
			mechanism: "The signature cause includes token parsing, signing-method and signature failures. The rate is an authentication-rejection symptom; it does not by itself identify stale verification keys or a certificate rotation fault.",
			baseline:  "The total signature rate is at most 10/s and every Connect, Alt or Proxy process is at most 1/s over five minutes. The comparisons are strictly greater.",
			observed:  observed,
			evidence:  "Two bounded Mimir reads use one evaluation time: the actual rate(counter[5m]) value and raw source/start witnesses. Rates sum only unique qualified processes whose source history agrees with the engine value. A separate bounded Docker/kernel census witnesses current generations; missing children are never zero-filled.",
			context:   "False-positive qualifier: a client-controlled malformed-token loop can raise this counter; an API-only 2/s loop and expired/no_active_row/credential_rotated causes do not meet these arms or prove key mismatch. False-negative qualifier: unseen, reset, stopped, mixed, lazy or partial process telemetry can hide rejected tokens; positive subset rates do not certify fleet completeness.",
			action:    "Correlate the verifier service cohort and rollout timing with private process/key-generation evidence through the existing JWT cause owner. Preserve token and key privacy; any key change or service restart requires separate authorization.",
			verify:    "Require a complete fresh five-minute window for the same desired cohort, including all observed generations and signature children, with both rates at or below threshold. A public TLS success, quiet replacement or missing counter cannot establish recovery.",
			playbook:  "SIGNALS.md §18.2a",
		})
	}
	if !evidence.complete {
		findings = append(findings, finding{
			probeId: "auth/jwt-rejections", tier: tierWarn, class: "jwt-rejections-unobservable", target: "jwt-fleet", sustain: 1,
			symptom:   "JWT rejection rates lack a complete process observation window",
			mechanism: "A missing expected slot, lazy signature series, partial query, duplicate publisher, source gap, reset or changed generation prevents a complete five-minute observation. Current surviving series cannot redefine the desired service cohort.",
			baseline:  "Every inventory-enabled JWT service slot, independently enumerated running process and observed publisher generation has one complete co-scraped five-minute start/counter history; optional undeployed Alt creates no denominator.",
			observed:  "reason=" + evidence.reason + " " + observed,
			evidence:  "Only fixed reasons, bounded counts and finite rates are rendered; response errors, labels, tokens, kids, signing keys and raw log lines remain absent.",
			context:   "Unknown is not zero or healthy. Independent authoritative rate breaches remain visible while another process, slot or query segment is unavailable. The current Docker/kernel census can expose an unpublished running generation; historical generations absent from every available source remain an independent deployment-history boundary.",
			action:    "Restore the configured service cohort and complete signature/start publication, inspect Mimir admission/query continuity, and reconcile running generations independently. Do not create token traffic, restart services or infer zero from an absent lazy child.",
			verify:    "Require a complete fresh five-minute process window and resolve independent running-generation coverage before treating quiet telemetry as fleet recovery.",
			playbook:  "SIGNALS.md §18.2a",
		})
	} else if len(findings) == 0 {
		findings = append(findings, healthyFinding("auth/jwt-rejections", tierWarn, "jwt-signature-rejection-rate", "jwt-fleet"))
	}
	return findings
}
