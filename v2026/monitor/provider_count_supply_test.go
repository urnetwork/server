// A country's empty answers page only when the country lost its admitted
// supply, or answered empty although well supplied; a country with no supply
// over the whole lookback only warns, per IP family. Every value below is
// synthetic.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// One taskworker's latest measurement.
type providerCountSupplyInstance struct {
	instance   string
	measuredAt time.Time
	countries  map[[2]string]float64
	fleet      map[string]float64
}

// The supply parts of the probe's query as Mimir returns them.
type providerCountSupplyState struct {
	instances     []providerCountSupplyInstance
	lookback      map[[2]string]float64
	coverage      time.Time
	fleetLookback map[string]float64
}

func providerCountSupplyFixture(t testing.TB, now time.Time, state providerCountSupplyState) string {
	t.Helper()
	result := []map[string]any{}
	row := func(labels map[string]string, value float64) {
		result = append(result, map[string]any{"metric": labels, "value": []any{float64(now.Unix()), fmt.Sprintf("%.3f", value)}})
	}
	for _, instance := range state.instances {
		row(map[string]string{"monitor_part": "supply_time", "job": "taskworker", "instance": instance.instance}, float64(instance.measuredAt.Unix()))
		for key, value := range instance.countries {
			row(map[string]string{"monitor_part": "supply", "job": "taskworker", "instance": instance.instance, "country": key[0], "ip_family": key[1]}, value)
		}
		for family, value := range instance.fleet {
			row(map[string]string{"monitor_part": "fleet", "job": "taskworker", "instance": instance.instance, "ip_family": family}, value)
		}
	}
	for key, value := range state.lookback {
		row(map[string]string{"monitor_part": "supply_lookback", "country": key[0], "ip_family": key[1]}, value)
	}
	if !state.coverage.IsZero() {
		row(map[string]string{"monitor_part": "supply_coverage"}, float64(state.coverage.Unix()))
	}
	for family, value := range state.fleetLookback {
		row(map[string]string{"monitor_part": "fleet_lookback", "ip_family": family}, value)
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// An ordinary location cohort whose zeros are supply-empty answers for one
// target, with the cohort's family and caller country.
func providerCountSupplyCohort(t testing.TB, now time.Time, family, caller, targetKind, targetCountry string, bands map[string]float64, zeros float64) string {
	t.Helper()
	outcomes := providerCountFixture(t, now, bands, "false")
	outcomes = strings.ReplaceAll(outcomes, `"caller_country":"us"`, `"caller_country":"`+caller+`"`)
	outcomes = strings.ReplaceAll(outcomes, `"ip_family":"any"`, `"ip_family":"`+family+`"`)
	explanation, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []map[string]any{{
		"metric": map[string]string{
			"monitor_part": "empty", "ip_family": family, "location_kind": "location", "caller_country": caller,
			"rank_mode": "quality", "force_minimum": "false", "target_kind": targetKind, "target_country": targetCountry, "reason": "cache_empty",
		},
		"value": []any{float64(now.Unix()), fmt.Sprintf("%.3f", zeros)},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	return providerCountJoinedControlFixtures(t, outcomes, string(explanation))
}

// One taskworker measured the supply four minutes ago, and the measurement
// history covers the whole day.
func providerCountSupplied(now time.Time, family string, country string, current, lookback float64, fleetNow, fleetLookback float64) providerCountSupplyState {
	return providerCountSupplyState{
		instances: []providerCountSupplyInstance{{
			instance: "taskworker-1", measuredAt: now.Add(-4 * time.Minute),
			countries: map[[2]string]float64{{country, family}: current, {"zz", family}: 1},
			fleet:     map[string]float64{family: fleetNow},
		}},
		lookback:      map[[2]string]float64{{country, family}: lookback, {"zz", family}: 1},
		coverage:      now.Add(-24*time.Hour - 3*time.Minute),
		fleetLookback: map[string]float64{family: fleetLookback},
	}
}

func providerCountSupplyAlerts(t *testing.T, now time.Time, payloads ...string) Alerts {
	t.Helper()
	alerts, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, providerCountJoinedControlFixtures(t, payloads...)))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func providerCountAlertClasses(alerts Alerts) []string {
	classes := []string{}
	for _, alert := range alerts {
		classes = append(classes, alert.Class)
	}
	return classes
}

var providerCountSupplyNow = time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)

func TestProviderCountCountrySupplyLossPages(t *testing.T) {
	now := providerCountSupplyNow
	alerts := providerCountSupplyAlerts(t, now,
		providerCountSupplyCohort(t, now, "any", "us", "country", "de", map[string]float64{"0": 20, "10+": 1}, 20),
		providerCountSupplyFixture(t, now, providerCountSupplied(now, "any", "de", 0, 40, 30000, 31000)))
	alert := requireAlertClass(t, alerts, "provider-count-effective-empty")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "country_supply=joined") || !strings.Contains(alert.Observed, "de/any:now=0:max_24h=40:lost") {
		t.Fatalf("supply loss: %s", alert.Observed)
	}
}

func TestProviderCountWellSuppliedCountryEmptyPages(t *testing.T) {
	now := providerCountSupplyNow
	alerts := providerCountSupplyAlerts(t, now,
		providerCountSupplyCohort(t, now, "any", "us", "country", "de", map[string]float64{"0": 20, "10+": 1}, 20),
		providerCountSupplyFixture(t, now, providerCountSupplied(now, "any", "de", 200, 220, 30000, 31000)))
	alert := requireAlertClass(t, alerts, "provider-count-effective-empty")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "de/any:now=200:max_24h=220:well_supplied") {
		t.Fatalf("well supplied: %s", alert.Observed)
	}
}

// Zero admitted supply over the whole lookback, or a supply below the floor,
// explains the zeros: the cohort that would have paged warns, carrying the
// counts, and never pages.
func TestProviderCountChronicZeroCountrySupplyOnlyWarns(t *testing.T) {
	now := providerCountSupplyNow
	for _, supply := range []struct{ current, lookback float64 }{{0, 0}, {3, 3}, {3, 40}} {
		alerts := providerCountSupplyAlerts(t, now,
			providerCountSupplyCohort(t, now, "any", "us", "country", "bj", map[string]float64{"0": 20, "10+": 1}, 20),
			providerCountSupplyFixture(t, now, providerCountSupplied(now, "any", "bj", supply.current, supply.lookback, 30000, 31000)))
		if classes := providerCountAlertClasses(alerts); len(classes) != 1 || classes[0] != "provider-count-country-unsupplied" {
			t.Fatalf("supply %+v: classes %v, want only the unsupplied WARN", supply, classes)
		}
		alert := alerts[0]
		want := fmt.Sprintf("bj/any:now=%.0f:max_24h=%.0f", supply.current, supply.lookback)
		if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, want) || !strings.Contains(alert.Observed, "zero_explained_by_country_supply=20") {
			t.Fatalf("supply %+v: %s", supply, alert.Observed)
		}
		for _, text := range []string{"IPv6 egress is offered only where providers have proven IPv6", "not a selection or cache failure", "SIGNALS.md §2.9a"} {
			if !strings.Contains(alert.Markdown(), text) {
				t.Fatalf("WARN lost %q", text)
			}
		}
	}
}

// The owner statement: an empty IPv6 answer for a country with no
// IPv6-capable provider over the lookback is expected.
func TestProviderCountV6CountryWithoutIpv6SupplyOnlyWarns(t *testing.T) {
	now := providerCountSupplyNow
	alerts := providerCountSupplyAlerts(t, now,
		providerCountSupplyCohort(t, now, "v6", "kr", "country", "kr", map[string]float64{"0": 20, "10+": 1}, 20),
		providerCountSupplyFixture(t, now, providerCountSupplied(now, "v6", "kr", 0, 0, 321, 330)))
	if classes := providerCountAlertClasses(alerts); len(classes) != 1 || classes[0] != "provider-count-country-unsupplied" {
		t.Fatalf("classes %v, want only the unsupplied WARN", classes)
	}
	if !strings.Contains(alerts[0].Observed, "kr/v6:now=0:max_24h=0") || alerts[0].Severity != SeverityWarn {
		t.Fatalf("v6 unsupplied: %s", alerts[0].Observed)
	}
}

// IPv6 supply lost in the country, or across the fleet, still pages.
func TestProviderCountV6SupplyLossPages(t *testing.T) {
	now := providerCountSupplyNow
	for name, state := range map[string]providerCountSupplyState{
		"country":    providerCountSupplied(now, "v6", "kr", 0, 7, 321, 330),
		"fleet-wide": providerCountSupplied(now, "v6", "kr", 0, 0, 100, 330),
	} {
		alerts := providerCountSupplyAlerts(t, now,
			providerCountSupplyCohort(t, now, "v6", "kr", "country", "kr", map[string]float64{"0": 20, "10+": 1}, 20),
			providerCountSupplyFixture(t, now, state))
		if alert := requireAlertClass(t, alerts, "provider-count-effective-empty"); alert.Severity != SeverityPage {
			t.Fatalf("%s: severity %s", name, alert.Severity)
		}
		if name == "fleet-wide" && !strings.Contains(requireAlertClass(t, alerts, "provider-count-effective-empty").Observed, "fleet_v6_dropped") {
			t.Fatalf("fleet-wide drop is not reported")
		}
	}
}

// A region pages when its country lost all supply; otherwise its empty answer
// stays the narrow target it was.
func TestProviderCountRegionPagesOnlyOnItsCountrysSupplyLoss(t *testing.T) {
	now := providerCountSupplyNow
	cohort := providerCountSupplyCohort(t, now, "any", "au", "region", "au", map[string]float64{"0": 20, "10+": 1}, 20)
	lost := providerCountSupplyAlerts(t, now, cohort, providerCountSupplyFixture(t, now, providerCountSupplied(now, "any", "au", 0, 50, 30000, 31000)))
	if alert := requireAlertClass(t, lost, "provider-count-effective-empty"); alert.Severity != SeverityPage {
		t.Fatalf("lost: severity %s", alert.Severity)
	}
	supplied := providerCountSupplyAlerts(t, now, cohort, providerCountSupplyFixture(t, now, providerCountSupplied(now, "any", "au", 150, 160, 30000, 31000)))
	if len(supplied) != 0 {
		t.Fatalf("a supplied country's empty region raised %v", providerCountAlertClasses(supplied))
	}
}

// The newest measurement is the current supply; an older taskworker's count
// only feeds the lookback.
func TestProviderCountCurrentSupplyIsTheNewestMeasurement(t *testing.T) {
	now := providerCountSupplyNow
	state := providerCountSupplied(now, "any", "de", 0, 50, 30000, 31000)
	state.instances = append(state.instances, providerCountSupplyInstance{
		instance: "taskworker-2", measuredAt: now.Add(-40 * time.Minute),
		countries: map[[2]string]float64{{"de", "any"}: 50}, fleet: map[string]float64{"any": 31000},
	})
	alerts := providerCountSupplyAlerts(t, now,
		providerCountSupplyCohort(t, now, "any", "us", "country", "de", map[string]float64{"0": 20, "10+": 1}, 20),
		providerCountSupplyFixture(t, now, state))
	if alert := requireAlertClass(t, alerts, "provider-count-effective-empty"); !strings.Contains(alert.Observed, "de/any:now=0:max_24h=50:lost") {
		t.Fatalf("newest measurement: %s", alert.Observed)
	}
}

// Missing, untrusted, stale or short supply history keeps today's verdict: a
// country's empty answer pages, a region's stays explained.
func TestProviderCountMissingSupplyKeepsTodaysBehavior(t *testing.T) {
	now := providerCountSupplyNow
	chronic := providerCountSupplied(now, "any", "bj", 0, 0, 30000, 31000)
	invalid := chronic
	invalid.lookback = map[[2]string]float64{{"synthetic-private.example", "any"}: 0}
	stale := providerCountSupplied(now, "any", "bj", 0, 0, 30000, 31000)
	stale.instances[0].measuredAt = now.Add(-2 * time.Hour)
	short := providerCountSupplied(now, "any", "bj", 0, 0, 30000, 31000)
	short.coverage = now.Add(-3 * time.Hour)
	unknownCountry := providerCountSupplied(now, "any", "de", 0, 0, 30000, 31000)
	for name, supply := range map[string]string{
		"absent":          "",
		"invalid":         providerCountSupplyFixture(t, now, invalid),
		"stale":           providerCountSupplyFixture(t, now, stale),
		"short-lookback":  providerCountSupplyFixture(t, now, short),
		"country-missing": providerCountSupplyFixture(t, now, unknownCountry),
	} {
		payloads := []string{providerCountSupplyCohort(t, now, "any", "us", "country", "bj", map[string]float64{"0": 20, "10+": 1}, 20)}
		if supply != "" {
			payloads = append(payloads, supply)
		}
		alerts := providerCountSupplyAlerts(t, now, payloads...)
		alert := requireAlertClass(t, alerts, "provider-count-effective-empty")
		if alert.Severity != SeverityPage || strings.Contains(alert.Markdown(), "synthetic-private.example") {
			t.Fatalf("%s: %+v", name, alert)
		}
		if !strings.Contains(alert.Observed, "zero_explained_by_country_supply=0") {
			t.Fatalf("%s: %s", name, alert.Observed)
		}
	}
	region := providerCountSupplyAlerts(t, now, providerCountSupplyCohort(t, now, "any", "au", "region", "au", map[string]float64{"0": 20, "10+": 1}, 20))
	if len(region) != 0 {
		t.Fatalf("absent supply changed a region's explained answer: %v", providerCountAlertClasses(region))
	}
	// a v6 judgment needs the fleet totals too
	noFleet := providerCountSupplied(now, "v6", "kr", 0, 0, 321, 330)
	noFleet.instances[0].fleet = nil
	noFleet.fleetLookback = nil
	alerts := providerCountSupplyAlerts(t, now,
		providerCountSupplyCohort(t, now, "v6", "kr", "country", "kr", map[string]float64{"0": 20, "10+": 1}, 20),
		providerCountSupplyFixture(t, now, noFleet))
	if alert := requireAlertClass(t, alerts, "provider-count-effective-empty"); alert.Severity != SeverityPage {
		t.Fatalf("v6 without fleet totals: severity %s", alert.Severity)
	}
}

func TestProviderCountQueryReadsCountrySupply(t *testing.T) {
	query := providerCountQuery("synthetic")
	for _, want := range []string{
		`sum by (ip_family,location_kind,caller_country,rank_mode,force_minimum,target_kind,target_country,reason) (increase(urnetwork_findproviders2_empty_outcomes_total{env="synthetic",capability!~"modern.*|post_quantum"}[5m]))`,
		`label_replace(urnetwork_client_score_admitted_providers{env="synthetic"},"monitor_part","supply","","")`,
		`label_replace(urnetwork_client_score_supply_measured_timestamp_seconds{env="synthetic"},"monitor_part","supply_time","","")`,
		`label_replace(max by (country,ip_family) (max_over_time(urnetwork_client_score_admitted_providers{env="synthetic"}[24h])),"monitor_part","supply_lookback","","")`,
		`label_replace(min(min_over_time(urnetwork_client_score_supply_measured_timestamp_seconds{env="synthetic"}[24h])),"monitor_part","supply_coverage","","")`,
		`label_replace(urnetwork_client_score_admitted_providers_fleet{env="synthetic"},"monitor_part","fleet","","")`,
		`label_replace(max by (ip_family) (max_over_time(urnetwork_client_score_admitted_providers_fleet{env="synthetic"}[7d])),"monitor_part","fleet_lookback","","")`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query lost %q", want)
		}
	}
}

func TestMonitorDocumentationProviderCountCountrySupply(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "### 2.9a ")
	if !found {
		t.Fatal("SIGNALS.md lost the provider-count section")
	}
	section, _, found = strings.Cut(section, "\n### 2.9b ")
	if !found {
		t.Fatal("SIGNALS.md lost the provider-count section boundary")
	}
	documentation := strings.Join(strings.Fields(section), " ")
	for _, required := range []string{
		"IPv6 egress is offered only where providers have proven IPv6. An empty IPv6 answer for a country with no IPv6-capable provider is expected.",
		"`urnetwork_client_score_admitted_providers`",
		"`provider-count-country-unsupplied`",
		"taskworker",
		"at least 10 admitted providers",
		"Missing or untrusted supply data keeps the verdict the probe gave without it",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("SIGNALS.md lost the country supply contract %q", required)
		}
	}
}
