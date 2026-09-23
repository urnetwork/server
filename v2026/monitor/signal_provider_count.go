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
	for _, cohort := range cohorts {
		if cohort.key.forceMinimum != "false" {
			continue
		}
		total := 0.0
		for _, value := range cohort.bands {
			total += value
		}
		zero := cohort.bands["0"]
		small := zero + cohort.bands["1-2"]
		if total >= providerCountPageMin && zero/total >= .8 {
			findings = append(findings, providerCountFinding(cohort, total, zero, small, tierPage, metricHost.name, "effective-empty"))
			continue
		}
		if total >= providerCountWarnMin && small/total >= .5 {
			findings = append(findings, providerCountFinding(cohort, total, zero, small, tierWarn, metricHost.name, "small-list"))
		}
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("mimir/provider-count", tierPage, "provider-count-degraded", "api-fleet")}, nil
	}
	return findings, nil
}

func providerCountFinding(cohort providerCountCohort, total, zero, small float64, tier, metricHost, class string) finding {
	zeroPercent := 100 * zero / total
	smallPercent := 100 * small / total
	symptom := fmt.Sprintf("provider lists are materially degraded for %s: %.0f completed ordinary requests, zero=%.1f%%, zero-or-1-2=%.1f%%", cohort.key.frame(), total, zeroPercent, smallPercent)
	if class == "effective-empty" {
		symptom = fmt.Sprintf("provider lists are effectively empty for %s: %.0f completed ordinary requests, zero=%.1f%%", cohort.key.frame(), total, zeroPercent)
	}
	return finding{
		probeId: "mimir/provider-count", tier: tier, class: "provider-count-" + class,
		target: "api-fleet", frame: cohort.key.frame(), sustain: 1,
		symptom:   symptom,
		mechanism: "This counts the final FindProviders2 response after network-only, IP-family, and explicit-destination exclusion filters. A nonempty Redis score cache can therefore remain healthy while the product-visible list is empty or materially small for this request cohort.",
		baseline:  "At least 20 ordinary completed requests with 80% zero providers is an effective-empty page. At least 50 ordinary completed requests with 50% in the zero or one-to-two bands is a material-degradation warning; isolated restrictive requests do not meet either cohort threshold.",
		observed:  fmt.Sprintf("completed_requests=%.0f zero=%.0f zero_percent=%.1f zero_or_1_2=%.0f zero_or_1_2_percent=%.1f ip_family=%s location_kind=%s caller_country=%s rank_mode=%s force_minimum=false range=%s metrics_gateway=%s", total, zero, zeroPercent, small, smallPercent, cohort.key.ipFamily, cohort.key.locationKind, cohort.key.callerCountry, cohort.key.rankMode, providerCountRange, metricHost),
		evidence:  "Mimir aggregates only the API counter's fixed-vocabulary request class and result-count bands. The metric is incremented at the completed response boundary; no client, network, provider, location ID, address, request identifier, or provider list leaves the API.",
		context:   "A restricted explicit destination, network-only access outside its network, or ForceMinimum diagnostic call can legitimately be small; those are separated from this ordinary response cohort. A returned candidate can still fail later, so pair this with missing-origin, stale-destination, egress admission, and end-to-end connection success. Missing outcome telemetry is a cannot-observe condition, never a healthy zero.",
		action:    "First correlate the same cohort with §2.9 cache population, §2.15 reliability integrity, §2.19a egress admission, and §2.17/§2.18 lifecycle failures. Restore the owning selection or lifecycle boundary; do not clear score keys, relax gates, or fabricate requests to make the list count recover.",
		verify:    "Two consecutive complete five-minute windows show every materially used ordinary cohort below the alert band, §2.9 current cache documents remain complete, and successful provider routes recover without manual state changes.",
		playbook:  "SIGNALS.md §2.9a and §5.9",
	}
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
