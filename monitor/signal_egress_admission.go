package monitor

// SIGNALS.md §2.19a observes actual due selection and post-call submission
// boundaries. Counters cannot reconstruct a provider's overwritten history.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	egressAdmissionResponseMax = 2 * 1024 * 1024
	egressAdmissionFreshness   = 90 * time.Second
	egressAdmissionMaxInterval = 3 * time.Minute
)

// Each new instance owns only its previous complete process snapshots. A
// restarted observer must warm up again; no durable history is fabricated.
func NewEgressAdmissionSignal() Signal {
	return &signalAdapter{
		number: "2.19a", key: "egress-admission", name: "Egress selection and submission boundaries",
		probe: &egressAdmissionProbe{},
	}
}

// Concurrent checks serialize only the in-memory reduction, never transport.
type egressAdmissionProbe struct {
	stateLock  sync.Mutex
	previous   map[egressAdmissionIdentity]egressAdmissionSnapshot
	observedAt time.Time
}

func (*egressAdmissionProbe) id() string             { return "runtime/egress-admission" }
func (*egressAdmissionProbe) tier() string           { return tierWarn }
func (*egressAdmissionProbe) cadence() time.Duration { return time.Minute }

// Exact process identity is private join state, never an exported metric label
// or a rendered provider identifier.
type egressAdmissionIdentity struct {
	environment string
	job         string
	host        string
	block       string
	instance    string
}

// A value and its underlying source timestamp must each occur exactly once.
type egressAdmissionSample struct {
	value      float64
	sourceTime float64
	valueSeen  bool
	timeSeen   bool
}

// Only fixed metric keys enter this map; ignored-label duplicates invalidate it.
type egressAdmissionProcess struct {
	samples map[string]egressAdmissionSample
	invalid bool
}

// A single scrape owns the bundle. Start time binds counter deltas to one
// executable process, including reuse of an instance label after restart.
type egressAdmissionSnapshot struct {
	start      float64
	sourceTime float64
	counters   map[string]float64
}

var egressAdmissionLanes = [...]string{"no-location", "stale-location", "stale-health", "missing-health"}
var egressAdmissionOutcomes = [...]string{"acknowledged", "unsupported", "canceled", "error_or_unknown"}
var egressAdmissionFullResults = [...]string{"attempted", "submitted", "skipped", "failed"}
var egressAdmissionPassErrors = [...]string{"blackhole_due", "full_due", "pins", "blackhole_run", "blackhole_submit", "full_run", "canceled"}

// One bounded instant query returns fixed families and their actual source
// timestamps. The instant evaluation timestamp alone cannot prove freshness.
func egressAdmissionQuery(environment string) string {
	parts := []string{}
	for _, family := range []struct{ name, metric, job, extraSelector string }{
		{name: "rss", metric: "process_resident_memory_bytes", job: "api|taskworker"},
		{name: "start", metric: "process_start_time_seconds", job: "api|taskworker"},
		{name: "enabled", metric: "urnetwork_egress_due_observation_enabled", job: "api"},
		{name: "requests", metric: "urnetwork_egress_due_requests_total", job: "api"},
		{name: "selected", metric: "urnetwork_egress_due_selected_total", job: "api"},
		{name: "enabled", metric: "urnetwork_egress_probe_submission_observation_enabled", job: "taskworker"},
		{name: "submission", metric: "urnetwork_egress_probe_submission_outcomes_total", job: "taskworker"},
		{name: "full", metric: "urnetwork_egress_probe_pass_providers_total", job: "taskworker", extraSelector: `schedule="full"`},
		{name: "pass_error", metric: "urnetwork_egress_probe_pass_errors_total", job: "taskworker"},
	} {
		extraSelector := ""
		if family.extraSelector != "" {
			extraSelector = "," + family.extraSelector
		}
		selector := fmt.Sprintf(`%s{env=%s,job=~%s%s}`, family.metric, strconv.Quote(environment), strconv.Quote(family.job), extraSelector)
		for _, part := range []string{"value", "timestamp"} {
			value := selector
			if part == "timestamp" {
				value = "timestamp(" + selector + ")"
			}
			parts = append(parts, fmt.Sprintf(
				`label_replace(label_replace(%s,"monitor_egress_family",%s,"job",".*"),"monitor_egress_part",%s,"job",".*")`,
				value, strconv.Quote(family.name), strconv.Quote(part),
			))
		}
	}
	return strings.Join(parts, " or ")
}

// Source failures and overflow return fixed text, never remote bodies or labels.
func (self *egressAdmissionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	queryUrl := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(egressAdmissionQuery(env.cfg.env))
	out, _, err := shellFirstServiceGateway(ctx, env.runner, env.cfg.hostsWithRole("services"), nil,
		"curl -fsS --max-time 15 --max-filesize "+strconv.Itoa(egressAdmissionResponseMax)+" '"+queryUrl+"'")
	if err != nil || len(out) > egressAdmissionResponseMax {
		return []finding{egressAdmissionUnobservable("bounded-source-unavailable", 0, 0)}, nil
	}
	processes, err := parseEgressAdmission(out, env.cfg.env, env.now())
	if err != nil {
		return []finding{egressAdmissionUnobservable("invalid-source-response", 0, 0)}, nil
	}
	return self.observe(env.now(), processes), nil
}

// Rejects incomplete, duplicate, unknown-label-domain, or source-stale bundles
// without rendering the offending value. Remote warnings also fail closed.
func parseEgressAdmission(out, environment string, now time.Time) (map[egressAdmissionIdentity]*egressAdmissionProcess, error) {
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
	}
	if len(out) > egressAdmissionResponseMax || json.Unmarshal([]byte(out), &response) != nil ||
		response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 {
		return nil, fmt.Errorf("invalid bounded egress observation")
	}
	processes := map[egressAdmissionIdentity]*egressAdmissionProcess{}
	for _, series := range response.Data.Result {
		identity := egressAdmissionIdentity{
			environment: series.Metric["env"], job: series.Metric["job"],
			host: series.Metric["host"], block: series.Metric["block"], instance: series.Metric["instance"],
		}
		if identity.environment != environment || (identity.job != "api" && identity.job != "taskworker") ||
			identity.host == "" || identity.block == "" || identity.instance == "" {
			return nil, fmt.Errorf("invalid egress process join")
		}
		process := processes[identity]
		if process == nil {
			process = &egressAdmissionProcess{samples: map[string]egressAdmissionSample{}}
			processes[identity] = process
		}
		key := series.Metric["monitor_egress_family"]
		switch key {
		case "selected":
			key += "/" + series.Metric["lane"] + "/" + series.Metric["expired"]
		case "submission":
			key += "/" + series.Metric["kind"] + "/" + series.Metric["outcome"]
		case "full":
			key += "/" + series.Metric["result"]
		case "pass_error":
			key += "/" + series.Metric["step"]
		}
		known := false
		for _, expected := range egressAdmissionKeys(identity.job) {
			known = known || expected == key
		}
		at, value, err := mimirInstantValue(series.Value)
		if !known || err != nil || now.Sub(at) > egressAdmissionFreshness || at.Sub(now) > 30*time.Second ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			process.invalid = true
			continue
		}
		sample := process.samples[key]
		switch series.Metric["monitor_egress_part"] {
		case "value":
			if sample.valueSeen {
				process.invalid = true
			}
			sample.value, sample.valueSeen = value, true
		case "timestamp":
			if sample.timeSeen {
				process.invalid = true
			}
			sample.sourceTime, sample.timeSeen = value, true
		default:
			process.invalid = true
		}
		process.samples[key] = sample
	}
	return processes, nil
}

// The capability is distinct from activity; all zero-valued fixed counters are
// required so an old or partially instrumented executable cannot look healthy.
func egressAdmissionKeys(job string) []string {
	keys := []string{"rss", "start", "enabled"}
	if job == "api" {
		keys = append(keys, "requests")
		for _, lane := range egressAdmissionLanes {
			for _, expired := range []string{"false", "true"} {
				keys = append(keys, "selected/"+lane+"/"+expired)
			}
		}
	} else {
		for _, kind := range []string{"health", "attempt"} {
			for _, outcome := range egressAdmissionOutcomes {
				keys = append(keys, "submission/"+kind+"/"+outcome)
			}
		}
		for _, result := range egressAdmissionFullResults {
			keys = append(keys, "full/"+result)
		}
		for _, step := range egressAdmissionPassErrors {
			keys = append(keys, "pass_error/"+step)
		}
	}
	return keys
}

// Requires one coherent fresh scrape, including source timestamps for zeros.
func (self *egressAdmissionProcess) snapshot(job string, now time.Time) (egressAdmissionSnapshot, bool) {
	snapshot := egressAdmissionSnapshot{counters: map[string]float64{}}
	if self.invalid {
		return snapshot, false
	}
	for _, key := range egressAdmissionKeys(job) {
		sample := self.samples[key]
		if !sample.valueSeen || !sample.timeSeen || sample.sourceTime < float64(now.Unix())-egressAdmissionFreshness.Seconds() ||
			sample.sourceTime > float64(now.Unix())+30 {
			return snapshot, false
		}
		if snapshot.sourceTime != 0 && snapshot.sourceTime != sample.sourceTime {
			return snapshot, false
		}
		snapshot.sourceTime = sample.sourceTime
		switch key {
		case "rss":
			if sample.value <= 0 {
				return snapshot, false
			}
		case "start":
			if sample.value <= 0 || sample.value > sample.sourceTime {
				return snapshot, false
			}
			snapshot.start = sample.value
		case "enabled":
			if sample.value != 1 {
				return snapshot, false
			}
		default:
			if sample.value != math.Trunc(sample.value) || sample.value > 9_007_199_254_740_991 ||
				(key == "selected/no-location/true" && sample.value != 0) {
				return snapshot, false
			}
			snapshot.counters[key] = sample.value
		}
	}
	return snapshot, true
}

// Preserves confirmed failures from stable complete processes even if another
// process is unobservable. Overlapping generations never supply a joined delta.
func (self *egressAdmissionProbe) observe(now time.Time, processes map[egressAdmissionIdentity]*egressAdmissionProcess) []finding {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	current := map[egressAdmissionIdentity]egressAdmissionSnapshot{}
	blockCounts := map[egressAdmissionIdentity]int{}
	roles := map[string]int{}
	for identity := range processes {
		block := identity
		block.instance = ""
		blockCounts[block]++
		roles[identity.job]++
	}
	unobservable, complete := 0, 0
	deltas := map[string]float64{}
	intervalValid := !self.observedAt.IsZero() && now.After(self.observedAt) && now.Sub(self.observedAt) <= egressAdmissionMaxInterval
	for identity, process := range processes {
		block := identity
		block.instance = ""
		snapshot, valid := process.snapshot(identity.job, now)
		if !valid || blockCounts[block] != 1 {
			unobservable++
			continue
		}
		current[identity] = snapshot
		previous, exists := self.previous[identity]
		if !intervalValid || !exists || previous.start != snapshot.start || snapshot.sourceTime <= previous.sourceTime {
			unobservable++
			continue
		}
		reset := false
		selectedDelta := float64(0)
		for key, value := range snapshot.counters {
			reset = reset || value < previous.counters[key]
			if strings.HasPrefix(key, "selected/") {
				selectedDelta += value - previous.counters[key]
			}
		}
		// Cross-family scrape interleaving can split a handler observation.
		// Do not turn such an unpaired selection into a coherent admission delta.
		if reset || (selectedDelta > 0 && snapshot.counters["requests"] == previous.counters["requests"]) {
			unobservable++
			continue
		}
		complete++
		for key, value := range snapshot.counters {
			deltas[key] += value - previous.counters[key]
		}
	}
	if now.After(self.observedAt) {
		self.previous, self.observedAt = current, now
	}
	findings := []finding{}
	if unobservable > 0 || roles["api"] == 0 || roles["taskworker"] == 0 {
		findings = append(findings, egressAdmissionUnobservable("incomplete-mixed-reset-or-warming", complete, unobservable))
	}
	for _, lane := range egressAdmissionLanes {
		if expired := deltas["selected/"+lane+"/true"]; expired > 0 {
			findings = append(findings, finding{
				probeId: self.id(), tier: tierWarn, class: "egress-admission-expired", target: "egress-fleet", frame: lane, sustain: 1,
				symptom:   "The due handler selected evidence whose absolute deadline had already passed",
				mechanism: "An actual selected-return event is observed, not an offered queue head. It proves overdue admission but not which provider, why it waited, successful response delivery, or durable refresh.",
				baseline:  "No expired selected-return events in the observed interval",
				observed:  fmt.Sprintf("lane=%s expired_selected=%.0f complete_process_intervals=%d", lane, expired, complete),
				action:    "Correlate direct §2.19 due/backoff/dark state and task outcomes; inspect API artifact/config convergence. Do not change scheduling or edit evidence rows from this aggregate alone.",
				verify:    "Require two complete fresh traffic-bearing process intervals with observed due-request activity and no new expired selections plus direct §2.19 deadline health. Quiet zero traffic is not recovery proof; current zero expiry does not reconstruct a historical miss.",
				playbook:  "SIGNALS.md §2.19a",
			})
		}
	}
	for _, kind := range []string{"health", "attempt"} {
		unsupported := deltas["submission/"+kind+"/unsupported"]
		canceled := deltas["submission/"+kind+"/canceled"]
		unknown := deltas["submission/"+kind+"/error_or_unknown"]
		if unsupported+canceled+unknown > 0 {
			findings = append(findings, finding{
				probeId: self.id(), tier: tierWarn, class: "egress-submission-unacknowledged", target: "egress-fleet", frame: kind, sustain: 1,
				symptom:   "Completed egress reporter calls returned without acknowledgment",
				mechanism: "These are post-call outcomes, separate from pre-submit health measurements and attempts. Error-or-unknown may include a write whose acknowledgment was lost; cancellation or unsupported endpoints remain non-fatal and do not prove persistence failure.",
				baseline:  "No unsupported, canceled, or error-or-unknown reporter returns in the observed interval",
				observed:  fmt.Sprintf("kind=%s acknowledged=%.0f unsupported=%.0f canceled=%.0f error_or_unknown=%.0f complete_process_intervals=%d", kind, deltas["submission/"+kind+"/acknowledged"], unsupported, canceled, unknown, complete),
				action:    "Check exact API/Taskworker capabilities and bounded aggregate transport/task controls; preserve non-fatal semantics and do not retry or edit provider rows solely from these counters.",
				verify:    "Require two complete fresh intervals without unacknowledged returns, acknowledged traffic for the affected kind, and direct §2.19 state checks. Quiet zero traffic is not recovery proof.",
				playbook:  "SIGNALS.md §2.19a",
			})
		}
	}
	// A completed full pass normally submits at least one result when it opens
	// provider tunnels.  A nontrivial attempted cohort with no submission and
	// no successful reporter call is the incident shape in which a shared
	// control-plane/context failure makes healthy providers appear dark.  It is
	// deliberately independent of per-provider blackhole verdicts: those can
	// be stale, and tunnel construction is not readiness.
	attempted := deltas["full/attempted"]
	submitted := deltas["full/submitted"]
	failed := deltas["full/failed"]
	if attempted >= 8 && submitted == 0 && failed >= attempted {
		canceled := deltas["pass_error/canceled"]
		findings = append(findings, finding{
			probeId: self.id(), tier: tierPage, class: "egress-full-no-submission", target: "egress-fleet", frame: "control-plane", sustain: 1,
			symptom:   "Full egress probes attempted a provider cohort but submitted no results",
			mechanism: "A full-pass attempted count with zero submitted and every attempted probe failed proves probe-pipeline progress without a usable result. A concurrent canceled task/pass counter supports a budget or lifecycle cancellation branch, but does not prove DNS, the load balancer, or providers failed. Tunnel construction alone is not readiness.",
			baseline:  "Every traffic-bearing full-probe interval has at least one submitted result, or a bounded partial-failure cohort that does not consume every attempted provider.",
			observed:  fmt.Sprintf("full_attempted=%.0f full_submitted=%.0f full_failed=%.0f pass_canceled=%.0f complete_process_intervals=%d", attempted, submitted, failed, canceled, complete),
			action:    "First inspect the prober's available transfer credit after durable escrow reservations, then its persisted identity/bootstrap state, task deadline/cancellation path, and bounded platform/API dial path from each executing Taskworker. Keep provider blackhole verdicts and actual tunnel health independent; do not mark providers bad, delete verdicts, or relax selection gates from this aggregate.",
			verify:    "After the proved shared boundary is repaired, require two complete traffic-bearing intervals with submitted full-probe results, no renewed full-no-submission finding, advancing §2.19/§2.23 evidence, and product provider-list recovery. Quiet probes or a fresh tunnel constructor alone are not recovery.",
			playbook:  "SIGNALS.md §2.19a, §2.23, §2.9a, and §1.2",
		})
	}
	return findings
}

// Fixed evidence describes a visibility boundary, never a healthy rollout or a
// negative assertion about failures in missing processes.
func egressAdmissionUnobservable(reason string, complete, incomplete int) finding {
	return finding{
		probeId: "runtime/egress-admission", tier: tierWarn, class: "egress-admission-unobservable", target: "egress-fleet", sustain: 1,
		symptom:   "Egress selection/submission intervals are not completely observable",
		mechanism: "Each observed API and Taskworker process needs a complete executable-owned bundle and two fresh advancing scrapes in the same generation. Missing, duplicate, stale, reset, overlapping or mixed telemetry cannot supply a healthy delta. Entirely absent process discovery remains owned by provenance and scrape-continuity probes.",
		baseline:  "Both roles present; one complete fresh process per host/block; two monotonic same-generation observations no more than three minutes apart",
		observed:  fmt.Sprintf("reason=%s complete_process_intervals=%d incomplete_process_intervals=%d", reason, complete, incomplete),
		action:    "Verify source adapters and exact API/Taskworker artifact ancestry. Deploy approved owning builds only when executable capability is proved missing. If capability is present, diagnose source transport, scrape freshness, joins, resets and overlap instead of redeploying from absent metrics. A config-only rollout cannot add these metrics. Preserve direct §2.19 alerts and any confirmed failures from complete processes.",
		verify:    "After convergence, obtain two complete advancing observations, then two clean traffic-bearing intervals and direct deadline/submission controls; missing metrics do not prove an old scheduler or failed persistence.",
		playbook:  "SIGNALS.md §2.19a",
	}
}
