package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	providerCountRange     = "5m"
	providerCountFreshness = 90 * time.Second
	providerCountPageMin   = 20.0
	providerCountWarnMin   = 50.0
)

// SIGNALS.md §2.9a maps to signal_provider_count.go and
// signal_provider_count_test.go. It measures completed API responses, not a
// Redis score-cache surrogate, so a real empty provider list remains visible.
func NewProviderCountSignal() Signal {
	return &signalAdapter{number: "2.9a", key: "provider-count", name: "User-visible provider-count degradation", probe: providerCountProbe{}}
}

type providerCountProbe struct{}

func (providerCountProbe) id() string             { return "mimir/provider-count" }
func (providerCountProbe) tier() string           { return tierPage }
func (providerCountProbe) cadence() time.Duration { return time.Minute }

func providerCountQuery(environment string) string {
	return `sum by (ip_family,location_kind,caller_country,rank_mode,force_minimum,result_count) (increase(urnetwork_findproviders2_outcomes_total{env=` + strconv.Quote(environment) + `}[` + providerCountRange + `]))`
}

type providerCountKey struct {
	ipFamily      string
	locationKind  string
	callerCountry string
	rankMode      string
	forceMinimum  string
}

func (k providerCountKey) frame() string {
	return strings.Join([]string{k.ipFamily, k.locationKind, k.callerCountry, k.rankMode}, "/")
}

type providerCountCohort struct {
	key   providerCountKey
	bands map[string]float64
}

func (p providerCountProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("provider count: no services host is configured for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(providerCountQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(ctx, env.runner, metricHosts, nil, "curl -fsS --max-time 15 '"+queryURL+"'")
	if err != nil {
		return nil, fmt.Errorf("provider count: query Mimir through service gateways: %w", err)
	}
	cohorts, err := parseProviderCountCohorts(out, env.now())
	if err != nil {
		return nil, err
	}
	findings := make([]finding, 0, len(cohorts))
	observedDiscovery := false
	for _, cohort := range cohorts {
		if cohort.key.forceMinimum != "false" {
			continue
		}
		total := 0.0
		for _, value := range cohort.bands {
			total += value
		}
		// Direct specs append caller-chosen IDs without running discovery.
		// Neither their natural small size nor their absence proves capacity.
		if cohort.key.locationKind != "direct" && total > 0 {
			observedDiscovery = true
		}
		zero := cohort.bands["0"]
		small := zero + cohort.bands["1-2"]
		if total >= providerCountPageMin && zero/total >= .8 {
			findings = append(findings, providerCountFinding(cohort, total, zero, small, tierPage, metricHost.name, "effective-empty"))
			continue
		}
		if total >= providerCountWarnMin && small/total >= .5 {
			findings = append(findings, providerCountFinding(cohort, total, zero, small, tierWarn, metricHost.name, "small-list"))
		} else if cohort.key.locationKind != "direct" && total >= providerCountPageMin && total < providerCountWarnMin && small/total >= .5 {
			// Falling below the empty-list PAGE ratio does not establish
			// recovery when a traffic-bearing cohort remains mostly small.
			// The sample is below the ordinary WARN floor; retain only an
			// intent-unknown observation, never a lower-threshold scarcity rule.
			findings = append(findings, providerCountFinding(cohort, total, zero, small, tierWarn, metricHost.name, "small-list-unclassified"))
		}
	}
	if len(findings) == 0 && observedDiscovery {
		return []finding{healthyFinding("mimir/provider-count", tierPage, "provider-count-degraded", "api-fleet")}, nil
	}
	return findings, nil
}

func providerCountFinding(cohort providerCountCohort, total, zero, small float64, tier, metricHost, class string) finding {
	zeroPercent := 100 * zero / total
	smallPercent := 100 * small / total
	observed := fmt.Sprintf("completed_requests=%.0f zero=%.0f zero_percent=%.1f zero_or_1_2=%.0f zero_or_1_2_percent=%.1f ip_family=%s location_kind=%s caller_country=%s rank_mode=%s force_minimum=false range=%s metrics_gateway=%s", total, zero, zeroPercent, small, smallPercent, cohort.key.ipFamily, cohort.key.locationKind, cohort.key.callerCountry, cohort.key.rankMode, providerCountRange, metricHost)
	if cohort.key.locationKind == "direct" {
		return finding{
			probeId: "mimir/provider-count", tier: tierWarn, class: "provider-count-direct-unclassified",
			target: "api-fleet", frame: cohort.key.frame(), sustain: 1,
			symptom:   fmt.Sprintf("Direct-provider response intent is unclassified for %s: %.0f completed requests, zero=%.1f%%, zero-or-1-2=%.1f%%", cohort.key.frame(), total, zeroPercent, smallPercent),
			mechanism: "The direct request class has no location, group or best-available selector. FindProviders2 appends each explicit ClientId unless that final destination is excluded; it does not run the discovery score, health, reliability, network-only or IP-family filters. An empty specification or all-excluded explicit destinations can return zero, and one or two requested IDs can legitimately return one or two. This count does not prove supply depletion or a provider failure.",
			baseline:  "Direct responses are interpreted against requested and nonexcluded explicit IDs, not discovery-pool size. The existing cohort thresholds identify material response traffic needing request-intent evidence; they do not establish a scarcity baseline.",
			observed:  observed,
			evidence:  "The completed-response metric retains only bounded request labels and result-count bands. It has no specification count, exclusion count, expected explicit-result count, client, network, provider, location ID, address or request identifier.",
			context:   "Intent remains unknown, not healthy. The default quality label does not prove quality ranking ran, and caller_country=unknown is expected when the direct branch skips caller-country lookup. Do not attribute this cohort to probe admission, DoH, TLS, quality gates or general capacity. A returned explicit ID is not proof that the destination is available.",
			action:    "Verify the emitting API artifact and correlate naturally occurring direct requests with bounded counts of requested explicit IDs, nonexcluded final IDs and returned entries at the same request boundary. Keep any identifiers private. Distinguish empty or intentionally excluded requests from request-construction failures; do not clear score keys, relax gates or fabricate traffic.",
			verify:    "Establish request intent and the expected nonexcluded explicit-result count before judging direct responses. Require independent traffic-bearing discovery cohorts and successful routes for supply recovery; disappearance of this warning or a quiet direct cohort is not recovery.",
			playbook:  "SIGNALS.md §2.9a and §5.9",
		}
	}
	if class == "small-list-unclassified" {
		return finding{
			probeId: "mimir/provider-count", tier: tierWarn, class: "provider-count-small-list-unclassified",
			target: "api-fleet", frame: cohort.key.frame(), sustain: 1,
			symptom:   fmt.Sprintf("Small provider-list response intent is unclassified for %s: %.0f completed responses, zero=%.1f%%, zero-or-1-2=%.1f%%", cohort.key.frame(), total, zeroPercent, smallPercent),
			mechanism: "The cohort has completed traffic and at least half its responses contain zero, one or two entries, but its sample is below the ordinary small-list WARN floor. This records response shape only; request intent and the established baseline remain unknown. It does not establish supply scarcity, provider failure or recovery.",
			baseline:  "Retain a nonpaging intent-unknown observation for at least 20 but fewer than 50 completed ordinary discovery responses in five minutes with at least 50% zero-or-one-to-two entries. The existing effective-empty PAGE takes precedence; the ordinary provisional WARN still requires 50 completed responses.",
			observed:  observed + " qualification=unclassified sample_state=below_warn_min request_intent=unknown baseline_state=unobserved completed_requests_for_threshold=" + strconv.FormatFloat(total, 'g', -1, 64),
			evidence:  "The completed-response counter omits ForceCount, requested/effective Count, target location and caller exclusions. ForceMinimum=false does not exclude an intentionally capped request. Caller country is not target/provider country, and repeated calls are not a unique-caller denominator.",
			context:   "A zero share falling below 80% can remove the PAGE while all responses remain small. That threshold movement is not recovery. Direct explicit-destination requests retain their separate interpretation, and ForceMinimum diagnostics remain excluded. The same-label cohort can mix target restrictions, count caps and exclusions; positive returned entries do not prove successful routes.",
			action:    "Retain the exact response counts and inspect already-available bounded request-intent evidence before attributing a cause. Correlate independent cache completeness, eligibility, egress and route results. Do not relax gates, clear score keys or manufacture traffic to cross an alert threshold.",
			verify:    "Establish request intent and sufficient same-cohort baseline evidence separately from successful routes. Falling below an alert ratio or volume floor, disappearance of a class, and a quiet cohort are not recovery evidence.",
			playbook:  "SIGNALS.md §2.9a and §5.9",
		}
	}
	symptom := fmt.Sprintf("provider lists are materially degraded for %s: %.0f completed ordinary requests, zero=%.1f%%, zero-or-1-2=%.1f%%", cohort.key.frame(), total, zeroPercent, smallPercent)
	if class == "effective-empty" {
		symptom = fmt.Sprintf("provider lists are effectively empty for %s: %.0f completed ordinary requests, zero=%.1f%%", cohort.key.frame(), total, zeroPercent)
	}
	f := finding{
		probeId: "mimir/provider-count", tier: tier, class: "provider-count-" + class,
		target: "api-fleet", frame: cohort.key.frame(), sustain: 1,
		symptom:   symptom,
		mechanism: "This counts the final FindProviders2 response after network-only, IP-family, and explicit-destination exclusion filters. A nonempty Redis score cache can therefore remain healthy while the product-visible list is empty or materially small for this request cohort.",
		baseline:  "At least 20 ordinary discovery requests in five minutes with 80% zero providers is an effective-empty page; this absolute page does not require a learned baseline.",
		observed:  observed,
		evidence:  "Mimir aggregates only the API counter's fixed-vocabulary request class and result-count bands. The metric is incremented at the completed response boundary; no client, network, provider, location ID, address, request identifier, or provider list leaves the API.",
		context:   "Direct explicit-destination cohorts are reported separately as request-intent unknown, and ForceMinimum diagnostics are excluded. Caller exclusions, network-only access outside its network and restrictive location or group requests can still legitimately make a discovery cohort small; these dimensions are not fully distinguished by the bounded metric. A returned candidate can still fail later, so pair this with missing-origin, stale-destination, egress admission, and end-to-end connection success. Missing outcome telemetry is a cannot-observe condition, never a healthy zero.",
		action:    "First correlate the same cohort with §2.9 cache population, §2.15 reliability integrity, §2.19a egress admission, and §2.17/§2.18 lifecycle failures. Restore the owning selection or lifecycle boundary; do not clear score keys, relax gates, or fabricate requests to make the list count recover.",
		verify:    "Two consecutive complete five-minute windows show every materially used ordinary cohort below the alert band, §2.9 current cache documents remain complete, and successful provider routes recover without manual state changes.",
		playbook:  "SIGNALS.md §2.9a and §5.9",
	}
	if class == "small-list" {
		f.symptom = fmt.Sprintf("Provisional five-minute small provider lists for %s: %.0f completed ordinary requests, zero=%.1f%%, zero-or-1-2=%.1f%%", cohort.key.frame(), total, zeroPercent, smallPercent)
		f.mechanism += " This fixed five-minute observation has no established rolling baseline and does not prove a regression from normal request behavior."
		f.baseline = "The current reducer retains a provisional WARN at 50 completed requests with at least 50% zero or one-to-two responses in five minutes. The desired catalog rule requires 15 minutes and an established materially lower cohort baseline; that qualification is not implemented."
		f.observed += " qualification=provisional baseline_state=unobserved"
		f.action = "Preserve this bounded observation and establish the same cohort's 15-minute distribution and pre-incident baseline before diagnosing a regression. Correlate request restrictions and independent cache, reliability, egress and route controls; do not clear score keys, relax gates or fabricate traffic from this provisional count."
		f.verify = "Require the catalog's separate 15-minute and established-baseline authority before claiming a qualified regression or its recovery. Clearing the provisional five-minute WARN alone does not satisfy that contract; retain independent traffic-bearing discovery and successful-route checks."
	}
	return f
}

func parseProviderCountCohorts(raw string, now time.Time) ([]providerCountCohort, error) {
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, fmt.Errorf("provider count: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("provider count: Mimir status=%q result_type=%q error=%q", response.Status, response.Data.ResultType, response.Error)
	}
	if len(response.Data.Result) == 0 {
		return nil, fmt.Errorf("provider count: outcome metric is absent")
	}
	byKey := map[providerCountKey]map[string]float64{}
	for _, series := range response.Data.Result {
		key, band, err := providerCountSeriesKey(series.Metric)
		if err != nil {
			return nil, err
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("provider count: parse %s: %w", key.frame(), err)
		}
		age := now.Sub(observedAt)
		if age > providerCountFreshness || age < -30*time.Second {
			return nil, fmt.Errorf("provider count: stale outcome sample for %s age=%s", key.frame(), age.Round(time.Second))
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("provider count: invalid outcome value for %s", key.frame())
		}
		bands := byKey[key]
		if bands == nil {
			bands = map[string]float64{}
			byKey[key] = bands
		}
		if _, duplicate := bands[band]; duplicate {
			return nil, fmt.Errorf("provider count: duplicate result band %s for %s", band, key.frame())
		}
		bands[band] = value
	}
	keys := make([]providerCountKey, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].frame() < keys[j].frame() })
	cohorts := make([]providerCountCohort, 0, len(keys))
	for _, key := range keys {
		cohorts = append(cohorts, providerCountCohort{key: key, bands: byKey[key]})
	}
	return cohorts, nil
}

func providerCountSeriesKey(metric map[string]string) (providerCountKey, string, error) {
	value := func(name string) string { return metric[name] }
	key := providerCountKey{ipFamily: value("ip_family"), locationKind: value("location_kind"), callerCountry: value("caller_country"), rankMode: value("rank_mode"), forceMinimum: value("force_minimum")}
	valid := func(actual string, allowed ...string) bool { return slices.Contains(allowed, actual) }
	if !valid(key.ipFamily, "any", "v4", "v6", "dualstack", "unknown") || !valid(key.locationKind, "location", "group", "best-available", "mixed", "direct") || !valid(key.rankMode, "quality", "speed", "unknown") || !valid(key.forceMinimum, "true", "false") || !(key.callerCountry == "unknown" || (len(key.callerCountry) == 2 && key.callerCountry == strings.ToLower(key.callerCountry))) {
		return providerCountKey{}, "", fmt.Errorf("provider count: invalid bounded metric labels")
	}
	band := value("result_count")
	if !valid(band, "0", "1-2", "3-9", "10+") {
		return providerCountKey{}, "", fmt.Errorf("provider count: invalid result count band")
	}
	return key, band, nil
}
