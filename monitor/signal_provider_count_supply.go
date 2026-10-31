// The admitted provider supply of the countries a provider-count cohort
// answered empty from (SIGNALS.md §2.9a). The taskworker's score export
// decides admission and publishes, per country target and request IP family,
// how many providers it admitted (model network_client_score_supply.go). A
// country that lost all of it, or answers empty although well supplied,
// pages; one with little or none over the lookback warns. Missing or
// untrusted supply data keeps the verdict the probe gave without it.
package monitor

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

const (
	// the lookback within which a country's supply counts as lost when it
	// is zero now
	providerCountSupplyLookback = "24h"
	// the lookback the fleet's IPv6 supply is compared with
	providerCountFleetLookback = "7d"
	// the newest measurement older than this is stale: exports run a few
	// minutes apart, and the exporter stops publishing after an hour
	providerCountSupplyMaxAge = time.Hour
	// the measurement history must reach this far back before zero supply
	// over the whole lookback is established
	providerCountSupplyCoverage = 23 * time.Hour
	// A country with at least this many admitted providers in the family
	// that answers a caller empty from supply has a cache or selection
	// failure: only that caller country's network exclusions could empty its
	// payload, and on Main the networks excluding a caller country held a
	// handful of connected providers (45 networks excluded AU callers, with
	// 3 connected providers in all, on 2026-10-30).
	providerCountWellSupplied = 10.0
	// the fleet's IPv6 supply below this share of its lookback maximum is a
	// loss, whatever each country shows
	providerCountFleetDropRatio = 0.5
)

// The tags of the supply parts of the probe's query.
var providerCountSupplyParts = []string{"supply", "supply_time", "supply_lookback", "supply_coverage", "fleet", "fleet_lookback"}

// The verdicts of a country's supply in one family.
const (
	providerCountSupplyUnknown      = "unknown"
	providerCountSupplyWellSupplied = "well_supplied"
	providerCountSupplyLost         = "lost"
	providerCountSupplyUnsupplied   = "unsupplied"
	providerCountSupplyLow          = "low"
	providerCountSupplyFleetDropped = "fleet_v6_dropped"
)

type providerCountSupplyKey struct {
	country string
	family  string
}

// One response's supply parts, trusted whole or not at all.
type providerCountSupplyEvidence struct {
	// absent, joined, invalid or stale
	state string
	// the newest measurement
	current map[providerCountSupplyKey]float64
	// the maximum of every measurement within the lookback
	lookback map[providerCountSupplyKey]float64
	// the measurement history reaches back the whole lookback
	covered       bool
	fleetCurrent  map[string]float64
	fleetLookback map[string]float64
}

// A lowercase two-letter code, the location table's country vocabulary.
func providerCountCountryCode(code string) bool {
	return len(code) == 2 && 'a' <= code[0] && code[0] <= 'z' && 'a' <= code[1] && code[1] <= 'z'
}

func providerCountSupplyFamily(family string) bool {
	return family == "any" || family == "v4" || family == "v6"
}

// Reads the supply parts. Any row that fails the outcome rows' checks or the
// fixed vocabularies makes the whole supply untrusted; the newest measurement
// must be fresh. Process identities stay inside the parser.
func parseProviderCountSupply(result []struct {
	Metric map[string]string `json:"metric"`
	Value  []json.RawMessage `json:"value"`
}, rows map[string][]int, now time.Time) providerCountSupplyEvidence {
	present := false
	for _, part := range providerCountSupplyParts {
		present = present || 0 < len(rows[part])
	}
	if !present {
		return providerCountSupplyEvidence{state: "absent"}
	}
	invalid := providerCountSupplyEvidence{state: "invalid"}
	type processKey struct{ host, block, instance string }
	process := func(metric map[string]string) (processKey, bool) {
		key := processKey{host: metric["host"], block: metric["block"], instance: metric["instance"]}
		return key, key.instance != "" && len(key.instance) <= 512
	}
	value := func(index int) (float64, bool) {
		observedAt, value, err := mimirInstantValue(result[index].Value)
		age := now.Sub(observedAt)
		return value, err == nil && age <= providerCountFreshness && -30*time.Second <= age && !math.IsNaN(value) && !math.IsInf(value, 0) && 0 <= value
	}
	measuredAt := map[processKey]float64{}
	for _, index := range rows["supply_time"] {
		key, ok := process(result[index].Metric)
		measured, valid := value(index)
		if _, duplicate := measuredAt[key]; !ok || !valid || duplicate {
			return invalid
		}
		measuredAt[key] = measured
	}
	newest, newestAt := processKey{}, -1.0
	for key, measured := range measuredAt {
		if newestAt < measured || (newestAt == measured && fmt.Sprint(key) < fmt.Sprint(newest)) {
			newest, newestAt = key, measured
		}
	}
	if newestAt < 0 {
		return providerCountSupplyEvidence{state: "absent"}
	}
	if age := now.Sub(time.Unix(int64(newestAt), 0)); age < -5*time.Minute {
		return invalid
	} else if providerCountSupplyMaxAge < age {
		return providerCountSupplyEvidence{state: "stale"}
	}
	evidence := providerCountSupplyEvidence{
		state:         "joined",
		current:       map[providerCountSupplyKey]float64{},
		lookback:      map[providerCountSupplyKey]float64{},
		fleetCurrent:  map[string]float64{},
		fleetLookback: map[string]float64{},
	}
	processCounts := map[processKey]map[providerCountSupplyKey]bool{}
	for _, index := range rows["supply"] {
		metric := result[index].Metric
		key, ok := process(metric)
		supplyKey := providerCountSupplyKey{country: metric["country"], family: metric["ip_family"]}
		count, valid := value(index)
		if !ok || !valid || !providerCountCountryCode(supplyKey.country) || !providerCountSupplyFamily(supplyKey.family) || processCounts[key][supplyKey] {
			return invalid
		}
		if processCounts[key] == nil {
			processCounts[key] = map[providerCountSupplyKey]bool{}
		}
		processCounts[key][supplyKey] = true
		if key == newest {
			evidence.current[supplyKey] = count
		}
	}
	for _, index := range rows["supply_lookback"] {
		metric := result[index].Metric
		supplyKey := providerCountSupplyKey{country: metric["country"], family: metric["ip_family"]}
		count, valid := value(index)
		if _, duplicate := evidence.lookback[supplyKey]; !valid || duplicate || !providerCountCountryCode(supplyKey.country) || !providerCountSupplyFamily(supplyKey.family) {
			return invalid
		}
		evidence.lookback[supplyKey] = count
	}
	if coverage := rows["supply_coverage"]; 1 < len(coverage) {
		return invalid
	} else if len(coverage) == 1 {
		oldest, valid := value(coverage[0])
		if !valid {
			return invalid
		}
		evidence.covered = !now.Add(-providerCountSupplyCoverage).Before(time.Unix(int64(oldest), 0))
	}
	fleetProcesses := map[processKey]map[string]bool{}
	for _, index := range rows["fleet"] {
		metric := result[index].Metric
		key, ok := process(metric)
		family := metric["ip_family"]
		count, valid := value(index)
		if !ok || !valid || !providerCountSupplyFamily(family) || fleetProcesses[key][family] {
			return invalid
		}
		if fleetProcesses[key] == nil {
			fleetProcesses[key] = map[string]bool{}
		}
		fleetProcesses[key][family] = true
		if key == newest {
			evidence.fleetCurrent[family] = count
		}
	}
	for _, index := range rows["fleet_lookback"] {
		family := result[index].Metric["ip_family"]
		count, valid := value(index)
		if _, duplicate := evidence.fleetLookback[family]; !valid || duplicate || !providerCountSupplyFamily(family) {
			return invalid
		}
		evidence.fleetLookback[family] = count
	}
	return evidence
}

// The fleet's IPv6 supply is below half of its lookback maximum.
func (self providerCountSupplyEvidence) fleetV6Dropped() bool {
	current, ok := self.fleetCurrent["v6"]
	maximum, okMaximum := self.fleetLookback["v6"]
	return self.state == "joined" && ok && okMaximum && 0 < maximum && current < providerCountFleetDropRatio*maximum
}

// The supply verdict for one country and family, with its newest and lookback
// counts. IPv6 needs the fleet totals as well, to see a fleet-wide loss.
func (self providerCountSupplyEvidence) verdict(key providerCountSupplyKey) (string, float64, float64) {
	if self.state != "joined" || !providerCountCountryCode(key.country) || !providerCountSupplyFamily(key.family) {
		return providerCountSupplyUnknown, 0, 0
	}
	current, ok := self.current[key]
	lookback, okLookback := self.lookback[key]
	if !ok || !okLookback {
		return providerCountSupplyUnknown, 0, 0
	}
	if key.family == "v6" {
		_, okCurrent := self.fleetCurrent["v6"]
		_, okMaximum := self.fleetLookback["v6"]
		if !okCurrent || !okMaximum {
			return providerCountSupplyUnknown, current, lookback
		}
		if self.fleetV6Dropped() {
			return providerCountSupplyFleetDropped, current, lookback
		}
	}
	switch {
	case providerCountWellSupplied <= current:
		return providerCountSupplyWellSupplied, current, lookback
	case 0 < current:
		return providerCountSupplyLow, current, lookback
	case 0 < lookback:
		return providerCountSupplyLost, current, lookback
	case self.covered:
		return providerCountSupplyUnsupplied, current, lookback
	default:
		return providerCountSupplyUnknown, current, lookback
	}
}

// A traffic-bearing cohort whose empty answers come from countries with
// little or no admitted supply in its family: expected, never a page.
func providerCountCountryUnsuppliedFinding(cohort providerCountCohort, metricHost string) finding {
	counts := newProviderCountCounts(cohort, metricHost)
	_, _, unsupplied := cohort.explained()
	return finding{
		probeId: "mimir/provider-count", tier: tierWarn, class: "provider-count-country-unsupplied",
		target: "api-fleet", frame: cohort.key.frame(), sustain: 1,
		symptom:   fmt.Sprintf("Requested countries hold little or no admitted %s supply for %s: %.0f of %.0f completed ordinary requests answered empty from them", cohort.key.ipFamily, cohort.key.frame(), unsupplied, counts.total),
		mechanism: "These empty answers carry cache_empty for a country target whose admitted providers in the request's IP family, as the taskworker's score export counts them, are below the well-supplied floor of 10 and were not lost: zero over the whole 24-hour lookback, or a few now. The export wrote the country without an admitted provider for this caller, so FindProviders2 answered correctly; this is not a selection or cache failure, and these zeros are left out of the effective-empty PAGE that would otherwise fire.",
		baseline:  "The effective-empty PAGE's own threshold, at least 20 completed requests in five minutes with 80% zero, met only by counting these countries' answers. A country that lost all of its supply within the lookback, or answers empty with at least 10 admitted providers, still pages.",
		observed:  counts.observed,
		evidence:  "urnetwork_findproviders2_empty_outcomes_total names the target country of each supply-empty answer; urnetwork_client_score_admitted_providers is the taskworker's per-country, per-family count of admitted providers, its newest measurement and 24-hour maximum. No client, network, provider, location ID or address leaves either.",
		context:   "IPv6 egress is offered only where providers have proven IPv6. An empty IPv6 answer for a country with no IPv6-capable provider is expected. The same holds for any family a country has no admitted supply in. A fleet-wide IPv6 loss, below half of its 7-day maximum, pages instead, whatever each country shows.",
		action:    "Whether these countries should be offered at all, in this family, is a product decision; the §2.9b picker shows what users can select. Do not relax admission gates, clear score keys or widen targets to make the answers nonempty.",
		verify:    "The cohort's answers from countries without supply fall below the page threshold for two consecutive complete five-minute windows, or the countries' providers are admitted again.",
		playbook:  "SIGNALS.md §2.9a and §5.9",
	}
}
