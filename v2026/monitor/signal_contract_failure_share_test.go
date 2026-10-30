// Synthetic producer histories exercise §25.1 without Mimir, SSH or fixtures.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A complete private process/outcome matrix with the producer's eager zeros.
func contractFailureFixture(now time.Time, share float64) []fleetSeries {
	settings := DefaultContractFailureShareSettings()
	at := fleetEvaluationTime(now, settings.EvaluationLag)
	series := []fleetSeries{}
	for _, slot := range contractStageFixtureSlots {
		add := func(part, ingress, outcome string, value func(time.Time) float64) {
			labels := map[string]string{"env": "synthetic", "job": slot.service, "service": slot.service, "host": slot.host, "block": slot.block, "instance": "aabb0011"}
			if ingress != "" {
				labels["ingress"], labels["outcome"] = ingress, outcome
			}
			points := map[int64]float64{}
			for tick := at.Add(-settings.WarnHold); !at.Before(tick); tick = tick.Add(settings.HoldStep) {
				points[tick.Unix()] = value(tick)
			}
			series = append(series, fleetSeries{part: part, labels: labels, points: points})
		}
		constant := func(v float64) func(time.Time) float64 { return func(time.Time) float64 { return v } }
		start := float64(at.Add(-time.Hour).Unix())
		add("start", "", "", constant(start))
		add("prior_start", "", "", constant(start))
		add("start_clock", "", "", func(tick time.Time) float64 { return float64(tick.Add(-15 * time.Second).Unix()) })
		add("prior_start_clock", "", "", func(tick time.Time) float64 { return float64(tick.Add(-settings.Window - 15*time.Second).Unix()) })
		for _, ingress := range []string{"http", "internal"} {
			for _, outcome := range []string{"contract_reply", "canceled", "panic", "error", "protocol_reject"} {
				rate := 0.0
				switch outcome {
				case "contract_reply":
					rate = 10 * (1 - share)
				case "canceled":
					rate = 10 * share
				case "error", "protocol_reject":
					rate = 10000
				}
				add("rate", ingress, outcome, constant(rate))
				add("clock", ingress, outcome, func(tick time.Time) float64 { return float64(tick.Add(-15 * time.Second).Unix()) })
				add("prior_clock", ingress, outcome, func(tick time.Time) float64 { return float64(tick.Add(-settings.Window - 15*time.Second).Unix()) })
				add("resets", ingress, outcome, constant(0))
				add("samples", ingress, outcome, constant(20))
			}
		}
	}
	return series
}

// Exact matrix JSON follows the public range-reader schema and requested grid.
func contractFailureFixtureJson(t testing.TB, series []fleetSeries) string {
	t.Helper()
	rows := []map[string]any{}
	for _, one := range series {
		labels := map[string]string{fleetPartLabel: one.part}
		for key, value := range one.labels {
			labels[key] = value
		}
		values := [][]any{}
		pointSeconds := make([]int64, 0, len(one.points))
		for at := range one.points {
			pointSeconds = append(pointSeconds, at)
		}
		sort.Slice(pointSeconds, func(i, j int) bool { return pointSeconds[i] < pointSeconds[j] })
		for _, at := range pointSeconds {
			values = append(values, []any{at, strconv.FormatFloat(one.points[at], 'g', -1, 64)})
		}
		rows = append(rows, map[string]any{"metric": labels, "values": values})
	}
	encoded, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": rows}})
	if err != nil {
		t.Fatal("synthetic matrix encoding failed")
	}
	return string(encoded)
}

// Uses the same desired service inventory as the public constructor control.
func contractFailureInventory() fleetSlotInventory {
	slots := append([]fleetSlot(nil), contractStageFixtureSlots...)
	for i := range slots {
		slots[i].allowed = true
	}
	return fleetSlotInventory{slots: slots}
}

// Selects one job's qualified points without reducing away source absence.
func contractFailureHistory(series []fleetSeries, service string, now time.Time) []contractFailurePoint {
	settings := DefaultContractFailureShareSettings()
	at := fleetEvaluationTime(now, settings.EvaluationLag)
	points := []contractFailurePoint{}
	for tick := at.Add(-settings.WarnHold); !at.Before(tick); tick = tick.Add(settings.HoldStep) {
		points = append(points, contractFailureSharePoints(fleetSamplesAt(series, tick), contractFailureInventory(), "synthetic", tick, settings)[service])
	}
	return points
}

// The sampled hold policy distinguishes measured incidents, absence and holds.
func contractFailureVerdict(findings []finding) (tier string, healthy, held, unknown bool) {
	for _, one := range findings {
		if one.class == "contract-failure-share-unobservable" {
			unknown = true
		}
		if one.class == "contract-failure-share" {
			healthy = healthy || one.healthy
			held = held || one.clearHold
			if !one.healthy && !one.clearHold {
				tier = one.tier
			}
		}
	}
	return
}

// Inclusive source-authored bands use only eligible outcomes; a large normal
// protocol-reject/error population cannot dilute or create this ratio.
func TestContractFailureShareBandsAndExcludedOutcomes(t *testing.T) {
	now := contractStageNow
	for _, test := range []struct {
		share   float64
		tier    string
		healthy bool
	}{
		{share: 0, healthy: true}, {share: .049, healthy: true},
		{share: .05, tier: tierWarn}, {share: .199, tier: tierWarn}, {share: .20, tier: tierPage}, {share: 1, tier: tierPage},
	} {
		points := contractFailureHistory(contractFailureFixture(now, test.share), "api", now)
		findings := evaluateContractFailureShare("synthetic/api", points, DefaultContractFailureShareSettings())
		tier, healthy, held, unknown := contractFailureVerdict(findings)
		if tier != test.tier || healthy != test.healthy || held || unknown {
			t.Fatal("inclusive share policy changed")
		}
		if points[0].excludedErrorRate != 20000 || points[0].excludedRejectRate != 20000 || points[0].eligibleRate != 20 {
			t.Fatal("excluded outcomes changed eligible denominator")
		}
	}
}

// An absent/reset/stale/foreign eager outcome or ambiguous source cannot certify
// the ratio, even when a complete healthy sibling survives.
func TestContractFailureShareSourceDisqualifiers(t *testing.T) {
	for _, variant := range []string{"missing_rate", "missing_error", "stale", "prior_stale", "reset", "too_few", "restart", "foreign_env", "extra_label", "duplicate", "multiple_processes", "missing_slot"} {
		series := contractFailureFixture(contractStageNow, .3)
		for i := range series {
			one := &series[i]
			if one.labels["job"] != "api" {
				continue
			}
			switch variant {
			case "missing_rate":
				if one.part == "rate" {
					one.points = nil
				}
			case "missing_error":
				if one.labels["outcome"] == "error" {
					one.points = nil
				}
			case "stale", "prior_stale":
				if one.part == "clock" && variant == "stale" || one.part == "prior_clock" && variant == "prior_stale" {
					for at := range one.points {
						one.points[at] = 1
					}
				}
			case "reset":
				if one.part == "resets" {
					for at := range one.points {
						one.points[at] = 1
					}
				}
			case "too_few":
				if one.part == "samples" {
					for at := range one.points {
						one.points[at] = 1
					}
				}
			case "restart":
				if one.part == "prior_start" {
					for at := range one.points {
						one.points[at]--
					}
				}
			case "foreign_env":
				one.labels["env"] = "foreign"
			case "extra_label":
				one.labels["unrecognized"] = "private-canary"
			case "missing_slot":
				one.points = nil
			}
		}
		if variant == "duplicate" {
			series = append(series, series[0])
		}
		if variant == "multiple_processes" {
			extra := series[0]
			extra.labels = map[string]string{}
			for key, value := range series[0].labels {
				extra.labels[key] = value
			}
			extra.labels["instance"] = "00112233"
			series = append(series, extra)
		}
		api := contractFailureHistory(series, "api", contractStageNow)
		proxy := contractFailureHistory(series, "proxy", contractStageNow)
		tier, healthy, held, unknown := contractFailureVerdict(evaluateContractFailureShare("synthetic/api", api, DefaultContractFailureShareSettings()))
		siblingTier, _, _, siblingUnknown := contractFailureVerdict(evaluateContractFailureShare("synthetic/proxy", proxy, DefaultContractFailureShareSettings()))
		if tier != "" || healthy || !held || !unknown || siblingTier != tierPage || siblingUnknown {
			t.Fatal("source loss changed service isolation or unknown hold")
		}
	}
}

// PAGE can survive missing older WARN history, while no incomplete or
// low-volume interval supplies recovery credit to a previously armed class.
func TestContractFailureShareHistoryAndLowVolumeHolds(t *testing.T) {
	settings := DefaultContractFailureShareSettings()
	for _, variant := range []string{"page_recent", "partial_page", "low_volume", "falling", "below_full"} {
		points := contractFailureHistory(contractFailureFixture(contractStageNow, .3), "api", contractStageNow)
		wantTier, wantHealthy, wantHeld, wantUnknown := "", false, true, false
		switch variant {
		case "page_recent":
			points[0].known = false
			wantTier, wantHeld, wantUnknown = tierPage, false, true
		case "partial_page":
			points[len(points)-1].known = false
			wantUnknown = true
		case "low_volume":
			for i := range points {
				points[i].eligibleRate = 19.999
				points[i].failureRate = 19
			}
		case "falling":
			points[len(points)-1].failureRate = 0
		case "below_full":
			for i := range points {
				points[i].failureRate = 0
			}
			wantHealthy, wantHeld = true, false
		}
		tier, healthy, held, unknown := contractFailureVerdict(evaluateContractFailureShare("synthetic/api", points, settings))
		if tier != wantTier || healthy != wantHealthy || held != wantHeld || unknown != wantUnknown {
			t.Fatal("sampled hold or traffic floor changed")
		}
	}
}

// Public source integration binds one bounded explicit history request and
// classifies the resulting pressure as an incident, not visibility.
func TestContractFailureSharePublicRunBindsHistoryAndClassification(t *testing.T) {
	calls := 0
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		calls++
		for _, fragment := range []string{"query_range", "start=" + strconv.FormatInt(fleetEvaluationTime(contractStageNow, 2*time.Minute).Add(-10*time.Minute).Unix(), 10), "end=" + strconv.FormatInt(fleetEvaluationTime(contractStageNow, 2*time.Minute).Unix(), 10), "step=60", "--max-filesize", "prior_clock", "prior_start", "resets(", "count_over_time("} {
			if !strings.Contains(command, fragment) {
				t.Fatal("historical source command lost a required bound or proof")
			}
		}
		if host.Name != "metrics-1" {
			t.Fatal("source left declared services gateway")
		}
		return contractFailureFixtureJson(t, contractFailureFixture(contractStageNow, .20)), nil
	}}
	settings := contractStageSettings(contractStageNow, source)
	if err := settings.Validate(); err != nil {
		t.Fatal("synthetic placement invalid")
	}
	alerts, err := NewContractFailureShareSignal().Run(context.Background(), settings)
	if err != nil || calls != 1 || len(alerts) != 4 {
		t.Fatal("public bounded source observation failed")
	}
	for _, alert := range alerts {
		if alert.Class != "contract-failure-share" || alert.unobservable || alert.Severity != SeverityPage {
			t.Fatal("public pressure classification changed")
		}
		if strings.Contains(alert.Markdown(), "aabb0011") || strings.Contains(alert.Markdown(), "api-a.example") {
			t.Fatal("source labels escaped fixed public projection")
		}
	}
}

// Returned transport/parser errors produce fixed visibility, never a partial
// ratio or source payload. Retiring the live owner prevents any publication.
func TestContractFailureSharePublicUnavailableAndCancellation(t *testing.T) {
	for _, variant := range []string{"transport", "partial_json", "cancel"} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			if variant == "cancel" {
				cancel()
				return "", context.Canceled
			}
			if variant == "transport" {
				return "private-canary", errors.New("private-canary")
			}
			return "private-canary", nil
		}}
		settings := contractStageSettings(contractStageNow, source)
		if err := settings.Validate(); err != nil {
			t.Fatal("synthetic placement invalid")
		}
		alerts, err := NewContractFailureShareSignal().Run(ctx, settings)
		cancel()
		if variant == "cancel" {
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 1 {
				t.Fatal("retired owner published")
			}
			continue
		}
		if err != nil || calls != 1 || len(alerts) != 1 || alerts[0].Class != "contract-failure-share-unobservable" || !alerts[0].unobservable || strings.Contains(alerts[0].Markdown(), "private-canary") {
			t.Fatal("unavailable source changed visibility/privacy contract")
		}
	}
}

// Invalid settings fail closed before contacting a source; an optional unplaced
// alt does not add a fabricated empty fleet requirement.
func TestContractFailureShareInvalidPolicyAndOptionalPlacement(t *testing.T) {
	for _, variant := range []string{"zero_step", "zero_band", "duplicate_service"} {
		settings := DefaultContractFailureShareSettings()
		switch variant {
		case "zero_step":
			settings.HoldStep = 0
		case "zero_band":
			settings.WarnShare = 0
		case "duplicate_service":
			settings.Services = append(settings.Services, "api")
		}
		if settings.valid() {
			t.Fatal("invalid share policy accepted")
		}
	}
	points := contractFailureHistory(contractFailureFixture(contractStageNow, 0), "api", contractStageNow)
	_, healthy, _, unknown := contractFailureVerdict(evaluateContractFailureShare("synthetic/api", points, DefaultContractFailureShareSettings()))
	if !healthy || unknown {
		t.Fatal("complete qualified low-share control not healthy")
	}
}

// Required absent or excluded inventory stays unknown. Optional unplaced alt
// creates no fabricated requirement, while an independent qualified job remains
// evaluable beside a missing required service.
func TestContractFailureShareInventoryCoverage(t *testing.T) {
	settings := DefaultContractFailureShareSettings()
	at := fleetEvaluationTime(contractStageNow, settings.EvaluationLag)
	for _, variant := range []string{"excluded_slot", "missing_service", "missing_whole_slot"} {
		samples := fleetSamplesAt(contractFailureFixture(contractStageNow, .3), at)
		inventory := contractFailureInventory()
		if variant == "excluded_slot" {
			inventory.slots[0].allowed = false
		}
		if variant == "missing_service" {
			inventory.slots = inventory.slots[1:]
			inventory.missingServices = []string{"api"}
		}
		if variant == "missing_whole_slot" {
			kept := []fleetSample{}
			for _, sample := range samples {
				if sample.labels["job"] != "api" {
					kept = append(kept, sample)
				}
			}
			samples = kept
		}
		points := contractFailureSharePoints(samples, inventory, "synthetic", at, settings)
		if points["api"].known || !points["proxy"].known {
			t.Fatal("required inventory gap replaced by a surviving sibling")
		}
		if _, present := points["alt"]; present {
			t.Fatal("optional unplaced alt became a fabricated zero")
		}
	}
}

// Custom settings reach both the scheduler and public effective-policy guidance.
// The default authored thresholds remain a separate invariant.
func TestContractFailureShareCustomPolicyCadenceAndGuidance(t *testing.T) {
	policy := DefaultContractFailureShareSettings()
	policy.Cadence = 37 * time.Second
	policy.MinEligibleRate = 10
	policy.WarnShare, policy.PageShare = .10, .30
	policy.WarnHold, policy.PageHold = 8*time.Minute, 3*time.Minute
	signal := NewContractFailureShareSignalWithSettings(policy)
	if signal.Cadence() != 37*time.Second || !policy.valid() {
		t.Fatal("effective scheduler policy lost")
	}
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		at := fleetEvaluationTime(contractStageNow, policy.EvaluationLag)
		series := contractFailureFixture(contractStageNow, .40)
		for i := range series {
			for second := range series[i].points {
				if second < at.Add(-policy.WarnHold).Unix() {
					delete(series[i].points, second)
				}
			}
		}
		return contractFailureFixtureJson(t, series), nil
	}}
	settings := contractStageSettings(contractStageNow, source)
	if err := settings.Validate(); err != nil {
		t.Fatal("synthetic placement invalid")
	}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 4 {
		t.Fatal("custom effective policy observation failed")
	}
	for _, alert := range alerts {
		if alert.Severity != SeverityPage || !strings.Contains(alert.Baseline, "10 eligible") || !strings.Contains(alert.Baseline, "10% for 8m0s") || !strings.Contains(alert.Baseline, "30% for 3m0s") || !strings.Contains(alert.Verify, "8m0s") || !strings.Contains(alert.Verify, "below 10%") {
			t.Fatal("public guidance misstated effective policy")
		}
	}
	policy.Cadence = 0
	if policy.valid() || signal.Cadence() != time.Minute {
		t.Fatal("invalid cadence reached scheduler or became valid")
	}
	defaults := DefaultContractFailureShareSettings()
	if defaults.Cadence != time.Minute || defaults.MinEligibleRate != 20 || defaults.WarnShare != .05 || defaults.PageShare != .20 || defaults.WarnHold != 10*time.Minute || defaults.PageHold != 5*time.Minute {
		t.Fatal("authored default policy changed")
	}
}

// The actual diagnosed four source rates total 20 after rounding. Every
// permutation must produce that same total without mutating the source slice.
// The historical naive fold is retained only to establish the failing order.
func TestContractFailureShareRateSumPermutations(t *testing.T) {
	rates := make([]float64, 4)
	// Use the same runtime operations as the producer fixture, rather than
	// higher-precision arithmetic on untyped constant expressions.
	share := .049
	for i := range rates {
		if i%2 == 0 {
			rates[i] = 10 * (1 - share)
		} else {
			rates[i] = 10 * share
		}
	}
	legacyBelow := false
	for a := 0; a < 4; a++ {
		for b := 0; b < 4; b++ {
			for c := 0; c < 4; c++ {
				for d := 0; d < 4; d++ {
					if a == b || a == c || a == d || b == c || b == d || c == d {
						continue
					}
					orderedFloats := []float64{rates[a], rates[b], rates[c], rates[d]}
					legacy := 0.0
					for _, rate := range orderedFloats {
						legacy += rate
					}
					legacyBelow = legacyBelow || legacy < 20
					if contractFailureRateSum(orderedFloats) != 20 {
						t.Fatal("equivalent source permutations changed eligible floor")
					}
					if orderedFloats[0] != rates[a] || orderedFloats[1] != rates[b] || orderedFloats[2] != rates[c] || orderedFloats[3] != rates[d] {
						t.Fatal("rate summation mutated input")
					}
				}
			}
		}
	}
	if !legacyBelow {
		t.Fatal("diagnosed naive accumulation order was not represented")
	}
	for _, rate := range []float64{math.Nextafter(20, 0), 20, math.Nextafter(20, math.Inf(1))} {
		if contractFailureRateSum([]float64{rate}) != rate {
			t.Fatal("summation broadened a genuine adjacent floor value")
		}
	}
}

// Actual qualified source rows retain strict lower/at/upper-ULP comparisons at
// the traffic floor and both share bands. No threshold tolerance is introduced.
func TestContractFailureShareQualifiedUlpBoundaries(t *testing.T) {
	policy := DefaultContractFailureShareSettings()
	for _, test := range []struct {
		eligible, share float64
		tier            string
		healthy, held   bool
	}{
		{eligible: math.Nextafter(20, 0), share: 0, held: true},
		{eligible: 20, share: 0, healthy: true},
		{eligible: math.Nextafter(20, math.Inf(1)), share: 0, healthy: true},
		{eligible: 32, share: math.Nextafter(.05, 0), healthy: true},
		{eligible: 32, share: .05, tier: tierWarn},
		{eligible: 32, share: math.Nextafter(.05, math.Inf(1)), tier: tierWarn},
		{eligible: 32, share: math.Nextafter(.20, 0), tier: tierWarn},
		{eligible: 32, share: .20, tier: tierPage},
		{eligible: 32, share: math.Nextafter(.20, math.Inf(1)), tier: tierPage},
	} {
		series := contractFailureFixture(contractStageNow, 0)
		for i := range series {
			one := &series[i]
			if one.labels["job"] != "api" || one.part != "rate" {
				continue
			}
			rate := 0.0
			if one.labels["ingress"] == "internal" {
				if one.labels["outcome"] == "contract_reply" {
					rate = test.eligible - test.eligible*test.share
				}
				if one.labels["outcome"] == "canceled" {
					rate = test.eligible * test.share
				}
			}
			for at := range one.points {
				one.points[at] = rate
			}
		}
		// Reverse input series on alternating controls; compensation must also
		// span all source categories/slots, not rely on JSON or map order.
		if test.healthy {
			for i, j := 0, len(series)-1; i < j; i, j = i+1, j-1 {
				series[i], series[j] = series[j], series[i]
			}
		}
		points := contractFailureHistory(series, "api", contractStageNow)
		for _, point := range points {
			if !point.known || point.eligibleRate != test.eligible {
				t.Fatal("qualified source changed exact eligible boundary")
			}
		}
		tier, healthy, held, unknown := contractFailureVerdict(evaluateContractFailureShare("synthetic/api", points, policy))
		if tier != test.tier || healthy != test.healthy || held != test.held || unknown {
			t.Fatal("strict qualified ULP policy changed")
		}
	}
}
