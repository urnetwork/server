// SIGNALS.md §25.1: one bounded history read of the eligible returned-call
// failure share. Desired placement, all eager outcome rows, source clocks and
// counter continuity qualify each job independently. Neither cancellations nor
// a generated reply identify a backend cause or customer delivery.
package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const contractFailureShareProbeId = "mimir/contract-failure-share"

// The authored share policy; the seasonal reply floor is a separate proposal.
type ContractFailureShareSettings struct {
	// Evaluation spacing; default one minute. Zero/negative is invalid; the
	// scheduler receives the default cadence until check reports invalid policy.
	Cadence time.Duration
	// Counter rate lookback; default five minutes, positive whole seconds, at
	// most one hour. Zero is invalid.
	Window time.Duration
	// Sampled warning hold; default ten minutes. Must cover PageHold and be
	// a positive multiple of HoldStep; zero is invalid.
	WarnHold time.Duration
	// Sampled page hold; default five minutes, a positive multiple of HoldStep.
	// Zero is invalid.
	PageHold time.Duration
	// Requested historical sampling spacing; default one minute, positive
	// whole seconds. At most 60 intervals fit WarnHold; zero is invalid.
	HoldStep time.Duration
	// Eligible returned calls per second at every hold point; default 20.
	// Must be finite and positive; zero is invalid.
	MinEligibleRate float64
	// Inclusive warning share in (0, PageShare); default .05, zero invalid.
	WarnShare float64
	// Inclusive page share in (WarnShare, 1]; default .20, zero invalid.
	PageShare float64
	// Desired producing services; defaults api/proxy/connect/taskworker/alt.
	// Empty, duplicate or unrecognized services are invalid.
	Services []string
	// Unplaced services creating no requirement; default alt. Empty means all
	// desired services are required.
	OptionalServices []string
	// Delayed query/source-clock and intake bounds use fleet defaults except
	// 4 MiB/65,536 points. Freshness/query/byte/point zero values are invalid;
	// zero lag and future skew are allowed.
	FleetReadSettings
}

// Defaults implement only the explicit three-outcome share arm of §25.1.
func DefaultContractFailureShareSettings() *ContractFailureShareSettings {
	read := DefaultFleetReadSettings()
	read.MaxResponseBytes = 4 * 1024 * 1024
	read.MaxRows = 65536
	return &ContractFailureShareSettings{
		Cadence: time.Minute, Window: 5 * time.Minute, WarnHold: 10 * time.Minute, PageHold: 5 * time.Minute,
		HoldStep: time.Minute, MinEligibleRate: 20, WarnShare: .05, PageShare: .20,
		Services:         []string{"api", "proxy", "connect", "taskworker", "alt"},
		OptionalServices: []string{"alt"}, FleetReadSettings: read,
	}
}

// Registers the per-service share without claiming the proposed seasonal floor.
func NewContractFailureShareSignal() Signal {
	return NewContractFailureShareSignalWithSettings(DefaultContractFailureShareSettings())
}

// Explicit settings keep thresholds and read bounds local to this signal.
func NewContractFailureShareSignalWithSettings(settings *ContractFailureShareSettings) Signal {
	return &signalAdapter{number: "25.1", key: "contract-failure-share", name: "Returned contract failure share", probe: contractFailureShareProbe{settings: settings}}
}

// No probe state is learned from the monitor's lifetime; holds come from history.
type contractFailureShareProbe struct{ settings *ContractFailureShareSettings }

// Stable source identity for findings and exact-class clear holds.
func (contractFailureShareProbe) id() string { return contractFailureShareProbeId }

// Missing source is a warning, independent of incident severity.
func (contractFailureShareProbe) tier() string { return tierWarn }

// Owning cadence schedules evaluations; the authored default is one minute.
// Invalid nonpositive cadence cannot reach the scheduler as a busy loop.
func (self contractFailureShareProbe) cadence() time.Duration {
	if self.settings == nil || self.settings.Cadence <= 0 {
		return DefaultContractFailureShareSettings().Cadence
	}
	return self.settings.Cadence
}

// Invalid policy cannot become a vacuous healthy hold or an unbounded read.
func (self *ContractFailureShareSettings) valid() bool {
	if self == nil {
		return false
	}
	serviceBools := map[string]bool{}
	for _, service := range self.Services {
		if serviceBools[service] || (service != "api" && service != "proxy" && service != "connect" && service != "taskworker" && service != "alt") {
			return false
		}
		serviceBools[service] = true
	}
	return self.Cadence > 0 && self.Window > 0 && self.Window <= time.Hour && self.PageHold > 0 && self.WarnHold >= self.PageHold &&
		self.HoldStep >= time.Second && self.WarnHold/self.HoldStep <= 60 &&
		self.WarnHold%self.HoldStep == 0 && self.PageHold%self.HoldStep == 0 &&
		self.Window%time.Second == 0 && self.HoldStep%time.Second == 0 &&
		self.MinEligibleRate > 0 && !math.IsInf(self.MinEligibleRate, 0) &&
		self.WarnShare > 0 && self.WarnShare < self.PageShare && self.PageShare <= 1 &&
		self.Freshness > 0 && self.FutureSkew >= 0 && self.EvaluationLag >= 0 &&
		self.QueryTimeout >= time.Second && self.MaxResponseBytes > 0 && self.MaxResponseBytes <= 4*1024*1024 &&
		self.MaxRows > 0 && self.MaxRows <= 65536 && len(self.Services) > 0
}

// The original process/outcome labels survive every branch. Prior source clocks
// and resets qualify rate windows without converting absent eager rows to zero.
func contractFailureShareQuery(environment string, settings *ContractFailureShareSettings, inventory fleetSlotInventory) string {
	selector := inventory.selector(environment)
	if selector == "" {
		return "vector(0)"
	}
	counter := contractStageCountMetric + "{" + selector + "}"
	window := promDuration(settings.Window)
	start := "process_start_time_seconds{" + selector + "}"
	return strings.Join([]string{
		fleetPart("rate", "rate("+counter+"["+window+"])"),
		fleetPart("clock", "timestamp("+counter+")"),
		fleetPart("prior_clock", "timestamp("+counter+" offset "+window+")"),
		fleetPart("resets", "resets("+counter+"["+window+"])"),
		fleetPart("samples", "count_over_time("+counter+"["+window+"])"),
		fleetPart("start", start),
		fleetPart("start_clock", "timestamp("+start+")"),
		fleetPart("prior_start", start+" offset "+window),
		fleetPart("prior_start_clock", "timestamp("+start+" offset "+window+")"),
	}, " or ")
}

// A process's eager counter cell, with fixed finite query-part values.
type contractFailureCell struct{ partFloats map[string]float64 }

// One process on one inventory slot; multiple publishers make the slot unknown.
type contractFailureProcess struct {
	startFloats  map[string]float64
	outcomeCells map[string]*contractFailureCell
}

// A qualified per-job point. Excluded rates remain explicit, not denominator mass.
type contractFailurePoint struct {
	known              bool
	requestedEvalUnix  int64
	eligibleRate       float64
	failureRate        float64
	excludedErrorRate  float64
	excludedRejectRate float64
}

// Private contributions are summed together after every slot has been parsed;
// neither map order nor intermediate rounding selects a threshold verdict.
type contractFailureRates struct {
	eligibleFloats []float64
	failureFloats  []float64
	errorFloats    []float64
	rejectFloats   []float64
}

// Sorting nonnegative source rates and retaining addition residuals makes the
// rounded total independent of map/series order. Threshold comparisons remain
// exact float comparisons: no epsilon admits a genuinely below-floor rate.
func contractFailureRateSum(rateFloats []float64) float64 {
	orderedFloats := append([]float64(nil), rateFloats...)
	sort.Float64s(orderedFloats)
	sum, residual := 0.0, 0.0
	for _, rate := range orderedFloats {
		next := sum + rate
		if math.Abs(sum) >= math.Abs(rate) {
			residual += (sum - next) + rate
		} else {
			residual += (rate - next) + sum
		}
		sum = next
	}
	return sum + residual
}

// Only these eagerly emitted producer enums participate in the schema.
func contractFailureOutcome(outcome string) bool {
	return outcome == "contract_reply" || outcome == "canceled" || outcome == "panic" || outcome == "error" || outcome == "protocol_reject"
}

// Fixed slot keys never render returned labels into public text.
func contractFailureSlotKey(service, host, block string) string {
	return service + "\x00" + host + "\x00" + block
}

// Qualifies all desired slots at one requested evaluation time. Source proof is
// per process; a complete sibling cannot replace a missing desired slot.
func contractFailureSharePoints(samples []fleetSample, inventory fleetSlotInventory, environment string, at time.Time, settings *ContractFailureShareSettings) map[string]contractFailurePoint {
	desiredKeySlots := map[string]fleetSlot{}
	servicePoints := map[string]contractFailurePoint{}
	serviceRates := map[string]*contractFailureRates{}
	invalidServiceBools := map[string]bool{}
	slotProcessMaps := map[string]map[string]*contractFailureProcess{}
	for _, slot := range inventory.slots {
		desiredKeySlots[contractFailureSlotKey(slot.service, slot.host, slot.block)] = slot
		servicePoints[slot.service] = contractFailurePoint{known: true, requestedEvalUnix: at.Unix()}
		if serviceRates[slot.service] == nil {
			serviceRates[slot.service] = &contractFailureRates{}
		}
		if !slot.allowed {
			invalidServiceBools[slot.service] = true
		}
	}
	for _, service := range inventory.missingServices {
		invalidServiceBools[service] = true
		servicePoints[service] = contractFailurePoint{requestedEvalUnix: at.Unix()}
	}
	for _, sample := range samples {
		service := sample.labels["job"]
		slotKey := contractFailureSlotKey(service, sample.labels["host"], sample.labels["block"])
		if _, desired := desiredKeySlots[slotKey]; !desired {
			// Selector cross-products may return undeclared tuples. They are
			// ignored and can never satisfy desired inventory.
			continue
		}
		processId := sample.labels["instance"]
		validProcess := len(processId) == 8
		for _, c := range processId {
			validProcess = validProcess && (c >= '0' && c <= '9' || c >= 'a' && c <= 'f')
		}
		startPart := sample.part == "start" || sample.part == "start_clock" || sample.part == "prior_start" || sample.part == "prior_start_clock"
		validLabels := len(sample.labels) == 6
		if !startPart {
			validLabels = len(sample.labels) == 8 && (sample.labels["ingress"] == "http" || sample.labels["ingress"] == "internal") && contractFailureOutcome(sample.labels["outcome"])
		}
		if sample.labels["env"] != environment || sample.labels["service"] != service || !validProcess || !validLabels || math.IsNaN(sample.value) || math.IsInf(sample.value, 0) || sample.value < 0 {
			invalidServiceBools[service] = true
			continue
		}
		if slotProcessMaps[slotKey] == nil {
			slotProcessMaps[slotKey] = map[string]*contractFailureProcess{}
		}
		process := slotProcessMaps[slotKey][processId]
		if process == nil {
			process = &contractFailureProcess{startFloats: map[string]float64{}, outcomeCells: map[string]*contractFailureCell{}}
			slotProcessMaps[slotKey][processId] = process
		}
		values := process.startFloats
		if !startPart {
			if sample.part != "rate" && sample.part != "clock" && sample.part != "prior_clock" && sample.part != "resets" && sample.part != "samples" {
				invalidServiceBools[service] = true
				continue
			}
			key := sample.labels["ingress"] + "/" + sample.labels["outcome"]
			cell := process.outcomeCells[key]
			if cell == nil {
				cell = &contractFailureCell{partFloats: map[string]float64{}}
				process.outcomeCells[key] = cell
			}
			values = cell.partFloats
		}
		if _, duplicate := values[sample.part]; duplicate {
			invalidServiceBools[service] = true
		}
		values[sample.part] = sample.value
	}
	for key, slot := range desiredKeySlots {
		processes := slotProcessMaps[key]
		if len(processes) != 1 {
			invalidServiceBools[slot.service] = true
			continue
		}
		for _, process := range processes {
			start := process.startFloats["start"]
			startClock := process.startFloats["start_clock"]
			if len(process.startFloats) != 4 || start != process.startFloats["prior_start"] || !contractFailureClock(process.startFloats["prior_start_clock"], at.Add(-settings.Window), settings) || start <= 0 || float64(at.Add(-settings.Window).Unix()) < start || !contractFailureClock(startClock, at, settings) || len(process.outcomeCells) != 10 {
				invalidServiceBools[slot.service] = true
				continue
			}
			for key, cell := range process.outcomeCells {
				values := cell.partFloats
				if len(values) != 5 || !contractFailureClock(values["clock"], at, settings) || !contractFailureClock(values["prior_clock"], at.Add(-settings.Window), settings) || values["resets"] != 0 || values["samples"] < 2 {
					invalidServiceBools[slot.service] = true
					continue
				}
				rates := serviceRates[slot.service]
				outcome := strings.Split(key, "/")[1]
				rate := values["rate"]
				switch outcome {
				case "contract_reply":
					rates.eligibleFloats = append(rates.eligibleFloats, rate)
				case "canceled", "panic":
					rates.eligibleFloats = append(rates.eligibleFloats, rate)
					rates.failureFloats = append(rates.failureFloats, rate)
				case "error":
					rates.errorFloats = append(rates.errorFloats, rate)
				case "protocol_reject":
					rates.rejectFloats = append(rates.rejectFloats, rate)
				}
			}
		}
	}
	for service, point := range servicePoints {
		if rates := serviceRates[service]; rates != nil {
			point.eligibleRate = contractFailureRateSum(rates.eligibleFloats)
			point.failureRate = contractFailureRateSum(rates.failureFloats)
			point.excludedErrorRate = contractFailureRateSum(rates.errorFloats)
			point.excludedRejectRate = contractFailureRateSum(rates.rejectFloats)
		}
		for _, rate := range []float64{point.eligibleRate, point.failureRate, point.excludedErrorRate, point.excludedRejectRate} {
			if math.IsNaN(rate) || math.IsInf(rate, 0) {
				invalidServiceBools[service] = true
			}
		}
		servicePoints[service] = point
	}
	for service := range invalidServiceBools {
		point := servicePoints[service]
		point.known = false
		servicePoints[service] = point
	}
	return servicePoints
}

// Source timestamps refer to scrape samples, not the query's evaluation clock.
func contractFailureClock(value float64, at time.Time, settings *ContractFailureShareSettings) bool {
	return float64(at.Add(-settings.Freshness).Unix()) <= value && value <= float64(at.Add(settings.FutureSkew).Unix())
}

// One bounded range request preserves the shared native intake and cancellation
// contracts. A failed request publishes no ratios, partial payload or raw error.
func (self contractFailureShareProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultContractFailureShareSettings()
	}
	if !settings.valid() {
		return []finding{contractFailureUnknown(env.cfg.env, "invalid_policy")}, nil
	}
	inventory := newFleetSlotInventory(env, settings.Services, settings.OptionalServices)
	at := fleetEvaluationTime(env.now(), settings.EvaluationLag)
	start := at.Add(-settings.WarnHold)
	raw, _, err := readFleetMimirRange(ctx, env, contractFailureShareQuery(env.cfg.env, settings, inventory), start, at, settings.HoldStep, settings.FleetReadSettings)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return contractFailureUnavailable(env.cfg.env, inventory, "source_unavailable"), nil
	}
	series, err := parseFleetSeries(raw, start, at, settings.HoldStep, settings.FleetReadSettings)
	if err != nil {
		return contractFailureUnavailable(env.cfg.env, inventory, "invalid_response"), nil
	}
	servicePoints := map[string][]contractFailurePoint{}
	for tick := start; !at.Before(tick); tick = tick.Add(settings.HoldStep) {
		for service, point := range contractFailureSharePoints(fleetSamplesAt(series, tick), inventory, env.cfg.env, tick, settings) {
			servicePoints[service] = append(servicePoints[service], point)
		}
	}
	findings := []finding{}
	for _, service := range settings.Services {
		points, present := servicePoints[service]
		if !present {
			continue
		}
		findings = append(findings, evaluateContractFailureShare(env.cfg.env+"/"+service, points, settings)...)
	}
	return findings, nil
}

// A missing request cannot supply absence credit to any desired job.
func contractFailureUnavailable(environment string, inventory fleetSlotInventory, reason string) []finding {
	findings := []finding{contractFailureUnknown(environment, reason)}
	serviceBools := map[string]bool{}
	for _, slot := range inventory.slots {
		serviceBools[slot.service] = true
	}
	for _, service := range inventory.missingServices {
		serviceBools[service] = true
	}
	for service := range serviceBools {
		findings = append(findings, finding{probeId: contractFailureShareProbeId, class: "contract-failure-share", target: environment + "/" + service, clearHold: true})
	}
	return findings
}

// PAGE needs its complete five-minute hold, WARN its ten-minute hold. Missing
// older WARN history does not hide an independently qualified PAGE. Low volume
// stays silent with a clear hold; it is not evidence that an incident recovered.
func evaluateContractFailureShare(target string, points []contractFailurePoint, settings *ContractFailureShareSettings) []finding {
	warnCount := int(settings.WarnHold/settings.HoldStep) + 1
	pageCount := int(settings.PageHold/settings.HoldStep) + 1
	full, below, warn, page := len(points) == warnCount, len(points) == warnCount, len(points) == warnCount, len(points) >= pageCount
	for i, point := range points {
		known := point.known
		enough := known && point.eligibleRate >= settings.MinEligibleRate
		share := 0.0
		if enough {
			share = point.failureRate / point.eligibleRate
		}
		full = full && known
		below = below && enough && share < settings.WarnShare
		warn = warn && enough && share >= settings.WarnShare
		if i >= len(points)-pageCount {
			page = page && enough && share >= settings.PageShare
		}
	}
	findings := []finding{}
	if !full {
		findings = append(findings, contractFailureUnknown(target, "incomplete_job_history"))
	}
	if page || warn {
		tier := tierWarn
		if page {
			tier = tierPage
		}
		last := points[len(points)-1]
		findings = append(findings, finding{
			probeId: contractFailureShareProbeId, tier: tier, class: "contract-failure-share", target: target, sustain: 1,
			symptom:   "Returned contract calls sustained an elevated eligible failure share",
			mechanism: "The per-job rolling ratio is (canceled + panic)/(contract_reply + canceled + panic). Cancellation includes client departure and rollout drain; this observation selects no backend cause.",
			baseline:  fmt.Sprintf("At least %g eligible returned calls per second at every requested hold step; WARN at %g%% for %s and PAGE at %g%% for %s.", settings.MinEligibleRate, 100*settings.WarnShare, settings.WarnHold, 100*settings.PageShare, settings.PageHold),
			observed:  fmt.Sprintf("requested_eval_unix=%d requested_rate_window=%s eligible_rate=%.3f failure_share=%.6f excluded_error_rate=%.3f excluded_protocol_reject_rate=%.3f requested_hold_step=%s", last.requestedEvalUnix, settings.Window, last.eligibleRate, last.failureRate/last.eligibleRate, last.excludedErrorRate, last.excludedRejectRate, settings.HoldStep),
			evidence:  "One bounded historical Mimir query. Every desired slot has one source publisher, both ingresses and all five eager outcomes, fresh current/prior source samples, an old-enough process and no reported counter reset in each rate window.",
			context:   "Rates estimate returned-call accounting, not unique customers or delivery. Error and protocol_reject are outside this authored ratio. Unfinished calls, stopped callers, missing processes and the seasonal reply floor remain separate coverage boundaries. Sampled hold steps do not prove continuous scrapes between them.",
			action:    "Inspect rollout-guard and the per-service stage means before assigning a cause. Preserve existing PgBouncer, Redis and host repair owners; do not infer a shared cause from simultaneous ratios.",
			verify:    fmt.Sprintf("Every point of a complete %s sampled hold has at least %g eligible returned calls/s and a share below %g%% before this ratio incident can clear. Missing or low-volume history cannot clear it.", settings.WarnHold, settings.MinEligibleRate, 100*settings.WarnShare),
			playbook:  "SIGNALS.md §25.1",
		})
	} else if below {
		findings = append(findings, healthyFinding(contractFailureShareProbeId, tierWarn, "contract-failure-share", target))
	} else {
		findings = append(findings, finding{probeId: contractFailureShareProbeId, class: "contract-failure-share", target: target, clearHold: true})
	}
	return findings
}

// Only fixed diagnostics leave the source parser; raw labels and errors stay private.
func contractFailureUnknown(target, reason string) finding {
	return finding{probeId: contractFailureShareProbeId, tier: tierWarn, class: "contract-failure-share-unobservable", target: target, sustain: 2,
		symptom:   "Returned contract failure share lacks complete eligible source history",
		mechanism: "Inventory, source generation, eager outcome coverage or a bounded historical read could not establish the authored ratio for this scope.",
		baseline:  "Every desired slot and hold step has complete current and prior source proof.", observed: "reason=" + reason,
		evidence: "Fixed source qualification result; absent counters are not zero.", context: "This is observation uncertainty, not proof of healthy contracts or backend failure.",
		action: "Restore the declared source coverage before interpreting the ratio. Preserve existing product repair owners.",
		verify: "Complete source history at every requested hold step.", playbook: "SIGNALS.md §25.1"}
}
