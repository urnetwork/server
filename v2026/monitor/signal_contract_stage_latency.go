package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §2.30 maps to signal_contract_stage_latency.go and
// signal_contract_stage_latency_test.go. A client cannot carry traffic until
// its contract exists, and when the database queues every stage of contract
// creation slows together. The mean time per contract reply is the rate of
// urnetwork_contract_creation_completed_stage_seconds_total over the rate of
// urnetwork_contract_creation_completed_total{outcome="contract_reply"},
// summed over every producing process. "For 10 minutes" is read from Mimir
// history (the minimum of the rolling mean over the hold), so a monitor
// restart cannot learn an active incident as healthy. Stage seconds include
// canceled, failed and rejected calls, so a call that returns without a reply
// raises the mean; no replies with returned calls is an infinite mean.
// Only fixed stage and outcome labels and inventory slot names leave Mimir.
func NewContractStageLatencySignal() Signal {
	return NewContractStageLatencySignalWithSettings(DefaultContractStageLatencySettings())
}

// The signal with explicit thresholds.
func NewContractStageLatencySignalWithSettings(settings *ContractStageLatencySettings) Signal {
	return &signalAdapter{
		number: "2.30", key: "contract-stage-latency", name: "Contract-creation stage latency",
		probe: contractStageLatencyProbe{settings: settings},
	}
}

const (
	contractStageLatencyProbeId = "mimir/contract-stage-latency"
	contractStageSecondsMetric  = "urnetwork_contract_creation_completed_stage_seconds_total"
	contractStageCountMetric    = "urnetwork_contract_creation_completed_total"
	contractStageEnabledMetric  = "urnetwork_contract_creation_stage_timing_enabled"
	contractStageReplyOutcome   = "contract_reply"
)

// The thresholds and bounds of §2.30.
type ContractStageLatencySettings struct {
	// rolling window of each mean
	Window time.Duration
	// the mean must stay over a band this long; read from Mimir history at
	// HoldStep resolution
	Hold     time.Duration
	HoldStep time.Duration
	// mean time per contract reply that warns, and that pages
	WarnMean time.Duration
	PageMean time.Duration
	// fewest returned calls in Window for a verdict
	MinReturnedCalls float64
	// one stage holding at least this share of the total is a query or plan
	// problem in that stage rather than pool queueing
	ConcentratedStageShare float64
	// the services whose processes create contracts; an optional service with
	// no placement creates no requirement
	Services         []string
	OptionalServices []string
	// the one fleet read, and the capability gauge's freshness on every slot
	FleetReadSettings
	// the most stages and slot gaps one alert lists
	MaxListed int
}

// The defaults of SIGNALS.md §2.30.
func DefaultContractStageLatencySettings() *ContractStageLatencySettings {
	return &ContractStageLatencySettings{
		Window:                 5 * time.Minute,
		Hold:                   10 * time.Minute,
		HoldStep:               time.Minute,
		WarnMean:               250 * time.Millisecond,
		PageMean:               time.Second,
		MinReturnedCalls:       100,
		ConcentratedStageShare: 0.7,
		Services:               []string{"api", "connect", "proxy", "taskworker", "alt"},
		OptionalServices:       []string{"alt"},
		FleetReadSettings:      DefaultFleetReadSettings(),
		MaxListed:              8,
	}
}

type contractStageLatencyProbe struct {
	settings *ContractStageLatencySettings
}

func (contractStageLatencyProbe) id() string             { return contractStageLatencyProbeId }
func (contractStageLatencyProbe) tier() string           { return tierWarn }
func (contractStageLatencyProbe) cadence() time.Duration { return time.Minute }

func promDuration(value time.Duration) string {
	return strconv.FormatInt(int64(value/time.Second), 10) + "s"
}

// The fleet mean seconds per contract reply over window, summed over every
// process and ingress. Shared with §14.8's loop read.
func contractStageMeanExpression(environment string, window time.Duration) string {
	base := "env=" + strconv.Quote(environment)
	return "sum(rate(" + contractStageSecondsMetric + "{" + base + "}[" + promDuration(window) + "])) / sum(rate(" +
		contractStageCountMetric + "{" + base + ",outcome=" + strconv.Quote(contractStageReplyOutcome) + "}[" + promDuration(window) + "]))"
}

func contractStageLatencyQuery(environment string, settings *ContractStageLatencySettings, inventory fleetSlotInventory) string {
	base := "env=" + strconv.Quote(environment)
	window := promDuration(settings.Window)
	mean := contractStageMeanExpression(environment, settings.Window)
	hold := "(" + mean + ")[" + promDuration(settings.Hold) + ":" + promDuration(settings.HoldStep) + "]"
	parts := []string{
		fleetPart("stage_seconds", "sum by (stage) (increase("+contractStageSecondsMetric+"{"+base+"}["+window+"]))"),
		fleetPart("outcomes", "sum by (outcome) (increase("+contractStageCountMetric+"{"+base+"}["+window+"]))"),
		fleetPart("hold_mean", "min_over_time("+hold+")"),
		fleetPart("hold_points", "count_over_time("+hold+")"),
		fleetPart("hold_expected", "count_over_time(vector(1)["+promDuration(settings.Hold)+":"+promDuration(settings.HoldStep)+"])"),
	}
	control := base + `,job="api",route=` + strconv.Quote(controlRouteLabel)
	parts = append(parts,
		fleetPart("control_mean", "sum(increase(urnetwork_http_request_duration_seconds_sum{"+control+"}["+window+"])) / sum(increase(urnetwork_http_request_duration_seconds_count{"+control+"}["+window+"]))"),
		fleetPart("pgbouncer_waiting", pgbouncerWaitingExpression(environment, settings.Freshness)),
	)
	if selector := inventory.selector(environment); selector != "" {
		parts = append(parts,
			fleetSlotFreshnessPart("enabled", contractStageEnabledMetric, selector),
			fleetPart("enabled_value", "min by (job,host,block) ("+contractStageEnabledMetric+"{"+selector+"})"),
		)
	}
	return strings.Join(parts, " or ")
}

// Queued PgBouncer clients over exporter series no older than freshness.
func pgbouncerWaitingExpression(environment string, freshness time.Duration) string {
	series := `pgbouncer_pool_clients_waiting{env=` + strconv.Quote(environment) + `,job="pgbouncer"}`
	return "sum(" + series + " and (timestamp(" + series + ") >= time() - " + strconv.FormatInt(int64(freshness/time.Second), 10) + "))"
}

// The parsed fleet read. Absent optional context stays NaN.
type contractStageObservation struct {
	stageSeconds   map[string]float64
	outcomeCounts  map[string]float64
	holdMean       float64
	holdPoints     float64
	holdExpected   float64
	controlMean    float64
	pgbouncerWait  float64
	enabledValues  map[string]float64
	invalidReasons []string
}

func newContractStageObservation(samples []fleetSample) *contractStageObservation {
	observation := &contractStageObservation{
		stageSeconds: map[string]float64{}, outcomeCounts: map[string]float64{}, enabledValues: map[string]float64{},
		holdMean: math.NaN(), holdPoints: math.NaN(), holdExpected: math.NaN(), controlMean: math.NaN(), pgbouncerWait: math.NaN(),
	}
	reasons := map[string]bool{}
	scalars := map[string]*float64{
		"hold_mean": &observation.holdMean, "hold_points": &observation.holdPoints, "hold_expected": &observation.holdExpected,
		"control_mean": &observation.controlMean, "pgbouncer_waiting": &observation.pgbouncerWait,
	}
	seenScalars := map[string]bool{}
	for _, sample := range samples {
		switch sample.part {
		case "stage_seconds", "outcomes":
			label, values := sample.labels["stage"], observation.stageSeconds
			if sample.part == "outcomes" {
				label, values = sample.labels["outcome"], observation.outcomeCounts
			}
			if !contractStageLabelValid(label) || len(sample.labels) != 1 {
				reasons["unrecognized_label"] = true
				continue
			}
			if _, duplicate := values[label]; duplicate {
				reasons["duplicate_series"] = true
			}
			if math.IsNaN(sample.value) || math.IsInf(sample.value, 0) || sample.value < 0 {
				reasons["invalid_value"] = true
				continue
			}
			values[label] = sample.value
		case "enabled_value":
			observation.enabledValues[sample.labels["job"]+"\x00"+sample.labels["host"]+"\x00"+sample.labels["block"]] = sample.value
		default:
			if strings.HasPrefix(sample.part, "fresh/") {
				continue
			}
			target, known := scalars[sample.part]
			if !known || len(sample.labels) != 0 {
				reasons["unrecognized_part"] = true
				continue
			}
			if seenScalars[sample.part] {
				reasons["duplicate_series"] = true
			}
			seenScalars[sample.part] = true
			*target = sample.value
		}
	}
	for reason := range reasons {
		observation.invalidReasons = append(observation.invalidReasons, reason)
	}
	sort.Strings(observation.invalidReasons)
	return observation
}

// Stage and outcome labels are fixed server words.
func contractStageLabelValid(label string) bool {
	if label == "" || 48 < len(label) {
		return false
	}
	for _, c := range label {
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func (self contractStageLatencyProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultContractStageLatencySettings()
	}
	target := env.cfg.env + "/contract-creation"
	at := fleetEvaluationTime(env.now(), settings.EvaluationLag)
	inventory := newFleetSlotInventory(env, settings.Services, settings.OptionalServices)
	out, gateway, err := readFleetMimir(ctx, env, contractStageLatencyQuery(env.cfg.env, settings, inventory), at, settings.FleetReadSettings)
	if err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		return []finding{contractStageLatencyUnknown(target, "source_unavailable "+observationFailureDetails(err, classifyObservationError(err)))}, nil
	}
	samples, err := parseFleetSamples(out, at, settings.FleetReadSettings)
	if err != nil {
		return []finding{contractStageLatencyUnknown(target, "invalid_response metrics_gateway="+gateway.name)}, nil
	}
	observation := newContractStageObservation(samples)
	// a fresh capability gauge that is not one is an artifact without timing
	coverage := newFleetSlotCoverage(inventory, samples, []string{"enabled"}, at, settings.FleetReadSettings, func(slot fleetSlot) string {
		if observation.enabledValues[slot.service+"\x00"+slot.host+"\x00"+slot.block] != 1 {
			return "capability-off"
		}
		return ""
	})
	return evaluateContractStageLatency(settings, target, gateway.name, observation, coverage), nil
}

func evaluateContractStageLatency(
	settings *ContractStageLatencySettings,
	target string,
	gatewayName string,
	observation *contractStageObservation,
	coverage fleetSlotCoverage,
) []finding {
	totalSeconds := 0.0
	for _, seconds := range observation.stageSeconds {
		totalSeconds += seconds
	}
	returned := 0.0
	for _, count := range observation.outcomeCounts {
		returned += count
	}
	replies := observation.outcomeCounts[contractStageReplyOutcome]
	stages := make([]string, 0, len(observation.stageSeconds))
	for stage := range observation.stageSeconds {
		stages = append(stages, stage)
	}
	sort.Slice(stages, func(i, j int) bool {
		left, right := observation.stageSeconds[stages[i]], observation.stageSeconds[stages[j]]
		if left != right {
			return left > right
		}
		return stages[i] < stages[j]
	})
	milliseconds := func(seconds float64) string {
		if math.IsNaN(seconds) {
			return "unknown"
		}
		if math.IsInf(seconds, 1) {
			return "unbounded"
		}
		return strconv.FormatFloat(seconds*1000, 'f', 1, 64)
	}
	stageText := []string{}
	for i, stage := range stages {
		if settings.MaxListed <= i {
			stageText = append(stageText, fmt.Sprintf("and %d more", len(stages)-i))
			break
		}
		perReply := math.Inf(1)
		if 0 < replies {
			perReply = observation.stageSeconds[stage] / replies
		}
		share := 0.0
		if 0 < totalSeconds {
			share = observation.stageSeconds[stage] / totalSeconds
		}
		stageText = append(stageText, fmt.Sprintf("%s=%sms(%.0f%%)", stage, milliseconds(perReply), 100*share))
	}
	windowMean := math.Inf(1)
	if 0 < replies {
		windowMean = totalSeconds / replies
	} else if totalSeconds == 0 {
		windowMean = math.NaN()
	}
	concentrated := "none"
	if 0 < len(stages) && 0 < totalSeconds && settings.ConcentratedStageShare <= observation.stageSeconds[stages[0]]/totalSeconds {
		concentrated = stages[0]
	}
	outcomes := make([]string, 0, len(observation.outcomeCounts))
	for outcome, count := range observation.outcomeCounts {
		outcomes = append(outcomes, fmt.Sprintf("%s=%.0f", outcome, count))
	}
	sort.Strings(outcomes)
	listed := func(values []string) string {
		if len(values) == 0 {
			return "none"
		}
		return strings.Join(values, ",")
	}
	observed := fmt.Sprintf(
		"hold_min_mean_ms=%s window_mean_ms=%s window=%s hold=%s replies=%.0f returned_calls=%.0f outcomes=%s stages=%s concentrated_stage=%s control_route_mean_ms=%s pgbouncer_clients_waiting=%s %s metrics_gateway=%s",
		milliseconds(observation.holdMean), milliseconds(windowMean), settings.Window, settings.Hold, replies, returned,
		listed(outcomes), listed(stageText), concentrated,
		milliseconds(observation.controlMean), contractStageCount(observation.pgbouncerWait), coverage.observed(settings.MaxListed), gatewayName,
	)

	reasons := append([]string(nil), observation.invalidReasons...)
	if len(observation.stageSeconds) == 0 || len(observation.outcomeCounts) == 0 {
		reasons = append(reasons, "stage_series_missing")
	}
	if returned < settings.MinReturnedCalls {
		reasons = append(reasons, "insufficient_returned_calls")
	}
	holdComplete := !math.IsNaN(observation.holdPoints) && !math.IsNaN(observation.holdExpected) &&
		0 < observation.holdExpected && observation.holdExpected <= observation.holdPoints
	if !holdComplete {
		reasons = append(reasons, "hold_incomplete")
	}
	if math.IsNaN(observation.holdMean) {
		reasons = append(reasons, "hold_mean_unknown")
	}
	verdictKnown := holdComplete && !math.IsNaN(observation.holdMean) && settings.MinReturnedCalls <= returned &&
		len(observation.stageSeconds) != 0 && len(observation.outcomeCounts) != 0 && len(observation.invalidReasons) == 0
	if !coverage.full() {
		reasons = append(reasons, "slot_coverage_incomplete")
	}

	findings := []finding{}
	if 0 < len(reasons) {
		findings = append(findings, contractStageLatencyUnknown(target, "visibility_reason="+strings.Join(reasons, ";")+" "+observed))
	}
	if !verdictKnown {
		return findings
	}
	if observation.holdMean <= settings.WarnMean.Seconds() {
		if len(reasons) == 0 {
			findings = append(findings, healthyFinding(contractStageLatencyProbeId, tierWarn, "contract-stage-latency", target))
		}
		return findings
	}
	tier := tierWarn
	if settings.PageMean.Seconds() < observation.holdMean {
		tier = tierPage
	}
	causeQualifier := "Stage totals alone do not identify a query, pool, host or network cause."
	mechanism := "Several contract-creation stages have elevated completed-call wall time per contract reply. This spread is compatible with pool queueing, but client CPU scheduling, network round trips and mixed call outcomes can also contribute. " + causeQualifier
	if concentrated != "none" {
		mechanism = fmt.Sprintf("Stage %s holds most of the completed-call wall time. Concentration names the application stage to inspect; pool acquisition, lock waits, query execution, client scheduling and network round trips within that stage remain possible. %s", concentrated, causeQualifier)
	}
	findings = append(findings, finding{
		probeId: contractStageLatencyProbeId, tier: tier, class: "contract-stage-latency",
		target: target, sustain: 1,
		symptom: fmt.Sprintf("Contract creation has taken at least %s ms per contract reply for %s (WARN over %s, PAGE over %s)",
			milliseconds(observation.holdMean), settings.Hold, settings.WarnMean, settings.PageMean),
		mechanism: mechanism,
		baseline:  fmt.Sprintf("The %s mean time per contract reply stays at or under %s throughout every %s hold; on 2026-10-15 it was about 50 ms, with relationship 20 ms, other 8 ms and every other stage under 5 ms.", settings.Window, settings.WarnMean, settings.Hold),
		observed:  observed,
		evidence:  "One Mimir instant query evaluated behind now: per-stage and per-outcome counter increases summed over every producing process and ingress, the minimum of the rolling per-reply mean over the hold, its point count against the expected count, and each desired slot's contract-stage capability gauge. Stage and outcome labels are fixed server words; no client, contract or query text leaves Mimir.",
		context:   "Means hide tails, and the code exports no per-stage histogram. The stage spans are exclusive application wall residence for returned calls, including canceled, rejected and panicking calls; they are not PostgreSQL CPU or wait residence. Optional control-route and queued-client context is not a same-attempt causal join. A low context value does not rule out pool pressure, and a high value does not prove it caused this stage. Slow contract creation can feed the hosted identity-churn loop (§14.8).",
		action:    "Read §2.11a pool starvation and queued clients, §2.2 wait events and §5.6 first. For the same affected interval, distinguish pool acquisition/residence, lock or query work (§2.3, §5.8), client CPU pressure (§8.14a) and network timing before choosing the owning repair. A concentrated stage narrows the application span; broad stage growth suggests shared pressure without selecting its cause. Preserve the existing PgBouncer and host owners; do not restart Connect, API or the proxies fleet-wide, since every restart recreates windows and contracts.",
		verify:    fmt.Sprintf("The per-reply mean stays at or under %s for a full %s hold with complete slot coverage, PgBouncer queued clients return to 0 and the /connect/control mean returns to single-digit milliseconds.", settings.WarnMean, settings.Hold),
		playbook:  "SIGNALS.md §2.30 and §5.12",
	})
	return findings
}

func contractStageCount(value float64) string {
	if math.IsNaN(value) {
		return "unknown"
	}
	return strconv.FormatFloat(value, 'f', 0, 64)
}

// An unavailable, partial or idle source is never a healthy fast mean.
func contractStageLatencyUnknown(target string, observed string) finding {
	return finding{
		probeId: contractStageLatencyProbeId, tier: tierWarn, class: "contract-stage-latency-unobservable",
		target: target, sustain: 2,
		symptom:   "Contract-creation stage latency cannot be established for every producing process",
		mechanism: "The Mimir read failed, a desired api, connect, proxy, taskworker or alt slot lacks a fresh contract-stage capability gauge, the rolling mean has missing points in its hold, or too few calls returned to judge. A slow stage on a missing process cannot be ruled out.",
		baseline:  "Every desired producing slot exports urnetwork_contract_creation_stage_timing_enabled 1 within the freshness bound, the hold has every expected point, and enough calls return to judge the mean.",
		observed:  observed,
		evidence:  "Coverage comes from active services.yml placement, not from surviving series; a draining or healthy sibling cannot stand in for a missing slot. A proven slow mean over the observed processes is reported beside this, never instead of it.",
		context:   "This is telemetry or inventory uncertainty, not evidence of fast contract creation. Contract throughput itself is §1.1 (contract-rate).",
		action:    "Restore the producing processes' metrics or the Mimir path, allow a replaced process one full hold, and check that a running artifact without the stage-timing collector is not still serving.",
		verify:    "Every desired slot reports a fresh capability gauge and the hold is complete for two cadences.",
		playbook:  "SIGNALS.md §2.30",
	}
}
