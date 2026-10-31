// The admitted supply the score export measures per country target and
// request IP family, and the target country an empty answer names, without
// any client, network, provider or location identity. Every value below is
// synthetic.
package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

func supplyTestScore(families uint8, online bool, quality bool) *ClientScore {
	return &ClientScore{
		ClientId:       server.NewId(),
		IpFamilies:     families,
		Online:         online,
		PassesMinimums: map[string]bool{RankModeQuality: quality},
	}
}

// Admitted is what the export's non-forced union pool holds: online, or
// native in either mode. Each family counts the facets a request of that
// outcome family draws: any is dualstack and IPv4-only, v4 is IPv4-only, v6 is
// dualstack and IPv6-only. Departed country targets count zero; region, city
// and unknown targets are not countries.
func TestClientScoreSupplyCountsAdmittedProvidersPerCountryAndFamily(t *testing.T) {
	de, kr, departed, region, unknown := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	scores := func(list ...*ClientScore) map[server.Id]*ClientScore {
		m := map[server.Id]*ClientScore{}
		for _, score := range list {
			m[score.ClientId] = score
		}
		return m
	}
	dualstack := ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
	locationClientScores := map[server.Id]map[server.Id]*ClientScore{
		de: scores(
			supplyTestScore(ClientScoreIpFamilyV4, true, false),
			supplyTestScore(ClientScoreIpFamilyV4, false, true),
			supplyTestScore(dualstack, true, true),
			supplyTestScore(ClientScoreIpFamilyV6, true, false),
			// not admitted: neither online nor native
			supplyTestScore(dualstack, false, false),
		),
		kr:     scores(supplyTestScore(ClientScoreIpFamilyV4, false, false)),
		region: scores(supplyTestScore(dualstack, true, true)),
	}
	countryCodes := map[server.Id]string{de: "de", kr: "kr", departed: "zz", unknown: "not-a-country"}
	supply := newClientScoreSupply(time.Unix(1_800_000_000, 0), locationClientScores, []clientScoreTarget{{id: departed}, {id: region}}, countryCodes)
	want := map[clientScoreSupplyKey]int{
		{"de", "any"}: 3, {"de", "v4"}: 2, {"de", "v6"}: 2,
		{"kr", "any"}: 0, {"kr", "v4"}: 0, {"kr", "v6"}: 0,
		{"zz", "any"}: 0, {"zz", "v4"}: 0, {"zz", "v6"}: 0,
	}
	if len(supply.countries) != len(want) {
		t.Fatalf("countries=%v, want %v", supply.countries, want)
	}
	for key, count := range want {
		if got, ok := supply.countries[key]; !ok || got != count {
			t.Fatalf("%v=%d (%t), want %d", key, got, ok, count)
		}
	}
	for family, count := range map[string]int{"any": 3, "v4": 2, "v6": 2} {
		if supply.fleet[family] != count {
			t.Fatalf("fleet %s=%d, want %d", family, supply.fleet[family], count)
		}
	}
}

// One series per country and family, the fleet families and the measurement
// time, whatever the export holds: about 250 countries at most, a code the
// directory cannot vouch for left out, and nothing at all once the measurement
// is older than an hour or before the first export.
func TestClientScoreSupplyCollectorCardinalityIsBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	collector := &clientScoreSupplyCollector{now: func() time.Time { return now }}
	if count := testutil.CollectAndCount(collector); count != 0 {
		t.Fatalf("a process that never exported published %d series", count)
	}
	countries := map[clientScoreSupplyKey]int{}
	for i := 0; i < 26*26; i++ {
		code := fmt.Sprintf("%c%c", 'a'+i/26, 'a'+i%26)
		for _, family := range clientScoreSupplyFamilies {
			countries[clientScoreSupplyKey{code, family}] = i
		}
	}
	countries[clientScoreSupplyKey{"not-a-country", "any"}] = 1
	countries[clientScoreSupplyKey{"DE", "any"}] = 1
	countries[clientScoreSupplyKey{"de", "dualstack"}] = 1
	collector.publish(&clientScoreSupply{measuredAt: now, countries: countries, fleet: map[string]int{"any": 9, "v4": 8, "v6": 7, "dualstack": 1}})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	gathered, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	series := 0
	for _, family := range gathered {
		for _, metric := range family.Metric {
			series++
			labels := map[string]string{}
			for _, pair := range metric.Label {
				labels[pair.GetName()] = pair.GetValue()
			}
			switch family.GetName() {
			case "urnetwork_client_score_admitted_providers":
				country := labels["country"]
				if len(labels) != 2 || len(country) != 2 || country != strings.ToLower(country) || !supplyTestFamily(labels["ip_family"]) {
					t.Fatalf("admitted series labels %v", labels)
				}
			case "urnetwork_client_score_admitted_providers_fleet":
				if len(labels) != 1 || !supplyTestFamily(labels["ip_family"]) {
					t.Fatalf("fleet series labels %v", labels)
				}
			case "urnetwork_client_score_supply_measured_timestamp_seconds":
				if len(labels) != 0 || metric.GetGauge().GetValue() != float64(now.Unix()) {
					t.Fatalf("measured series %v", metric)
				}
			default:
				t.Fatalf("unexpected family %s", family.GetName())
			}
			if metric.GetGauge() == nil {
				t.Fatalf("%s is not a gauge", family.GetName())
			}
		}
	}
	if want := 26*26*3 + 3 + 1; series != want {
		t.Fatalf("series=%d, want %d", series, want)
	}
	now = now.Add(clientScoreSupplyMaxAge + time.Second)
	if count := testutil.CollectAndCount(collector); count != 0 {
		t.Fatalf("a measurement past its age still published %d series", count)
	}
}

func supplyTestFamily(family string) bool {
	return family == "any" || family == "v4" || family == "v6"
}

// A supply-empty answer names its requested country, a region's country for a
// region; every other answer leaves target_country empty, so the counter's
// added cardinality is the countries actually answered empty from supply.
func TestRecordFindProviders2EmptyOutcomeNamesTheSupplyEmptyTargetCountry(t *testing.T) {
	locationID := server.NewId()
	args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationID}}}
	for _, test := range []struct {
		targetKind, targetCountry, reason, want string
	}{
		{"country", "kr", "cache_empty", "kr"},
		{"region", "au", "cache_empty", "au"},
		{"city", "au", "cache_empty", ""},
		{"country", "kr", "cache_missing", ""},
		{"country", "kr", "filtered_destinations", ""},
		{"country", "", "cache_empty", ""},
	} {
		counter, err := findProviders2EmptyOutcomesTotal.GetMetricWithLabelValues("any", "location", "us", RankModeQuality, "false", "none", test.targetKind, test.want, test.reason)
		if err != nil {
			t.Fatal(err)
		}
		before := testutil.ToFloat64(counter)
		recordFindProviders2EmptyOutcome(args, "US", test.targetKind, test.targetCountry, test.reason)
		if got := testutil.ToFloat64(counter); got != before+1 {
			t.Fatalf("%+v: counter=%v, want %v", test, got, before+1)
		}
	}
}

// The requested country resolves through the directory, or the country list
// for a legacy entry; specs naming two countries, a group or the best
// available exits have no single target country.
func TestFindProviders2TargetCountryIsSingleAndResolved(t *testing.T) {
	kr, seoul, au, legacy := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	group := server.NewId()
	directory := map[server.Id]*locationDirectoryEntry{
		kr:    {CountryCode: "KR", LocationType: LocationTypeCountry},
		seoul: {CountryCode: "kr", LocationType: LocationTypeCity},
		au:    {CountryCode: "au", LocationType: LocationTypeCountry},
	}
	countries := map[string]server.Id{"jp": legacy}
	for _, test := range []struct {
		specs []*ProviderSpec
		want  string
	}{
		{[]*ProviderSpec{{LocationId: &kr}}, "kr"},
		{[]*ProviderSpec{{LocationId: &kr}, {LocationId: &seoul}}, "kr"},
		{[]*ProviderSpec{{LocationId: &legacy}}, "jp"},
		{[]*ProviderSpec{{LocationId: &kr}, {LocationId: &au}}, ""},
		{[]*ProviderSpec{{LocationId: &kr}, {LocationGroupId: &group}}, ""},
		{[]*ProviderSpec{{LocationId: &kr}, {BestAvailable: true}}, ""},
		{[]*ProviderSpec{{LocationGroupId: &group}}, ""},
		{nil, ""},
	} {
		if got := findProviders2TargetCountry(&FindProviders2Args{Specs: test.specs}, countries, directory); got != test.want {
			t.Fatalf("specs %d: target country %q, want %q", len(test.specs), got, test.want)
		}
	}
}
