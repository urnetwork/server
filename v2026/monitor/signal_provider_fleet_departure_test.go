package monitor

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// §28.3 over synthetic series: 45 one-minute evaluations, the hold, of the
// providers by country against their peaks, the fleet totals and the builds
// that lost the most.

const providerFleetTestPoints = 45

type providerFleetTestCountry struct {
	now  []float64
	peak []float64
}

type providerFleetTestSeries struct {
	countries map[string]providerFleetTestCountry
	builds    map[string][]float64
	totals    map[string][]float64
}

// Main on 2026-11-11 at 10:30Z, before the fleet left.
func providerFleetTestHealthy() providerFleetTestSeries {
	steady := func(now float64, peak float64) providerFleetTestCountry {
		return providerFleetTestCountry{now: stormRepeat(now, providerFleetTestPoints), peak: stormRepeat(peak, providerFleetTestPoints)}
	}
	return providerFleetTestSeries{
		countries: map[string]providerFleetTestCountry{
			"VN": steady(11221, 11224),
			"US": steady(65133, 65924),
			"DE": steady(7026, 7100),
			"MO": steady(2, 3),
		},
		builds: map[string][]float64{},
		totals: map[string][]float64{
			"providers_now":  stormRepeat(113859, providerFleetTestPoints),
			"providers_peak": stormRepeat(114686, providerFleetTestPoints),
			"connect_now":    stormRepeat(152611, providerFleetTestPoints),
			"connect_peak":   stormRepeat(153528, providerFleetTestPoints),
			"gossip_now":     stormRepeat(15576, providerFleetTestPoints),
			"gossip_peak":    stormRepeat(15576, providerFleetTestPoints),
		},
	}
}

// The same an hour after the fleet left: Vietnam at 216 of 11,221, the
// fleet's build gone, connect down 8% and gossip five-fold.
func providerFleetTestDeparted() providerFleetTestSeries {
	series := providerFleetTestHealthy()
	series.countries["VN"] = providerFleetTestCountry{now: stormRepeat(216, providerFleetTestPoints), peak: stormRepeat(11221, providerFleetTestPoints)}
	series.builds["2026.10.22+1078873780-localfix2"] = stormRepeat(12470, providerFleetTestPoints)
	series.builds["2026.10.27+1083160450"] = stormRepeat(85, providerFleetTestPoints)
	series.totals["providers_now"] = stormRepeat(102307, providerFleetTestPoints)
	series.totals["connect_now"] = stormRepeat(139823, providerFleetTestPoints)
	series.totals["gossip_now"] = stormRepeat(3296, providerFleetTestPoints)
	return series
}

func providerFleetTestResponse(t *testing.T, series providerFleetTestSeries) string {
	t.Helper()
	synthetic := []stormSyntheticSeries{}
	for countryCode, country := range series.countries {
		if country.now != nil {
			synthetic = append(synthetic, stormSyntheticSeries{part: "country_now", labels: map[string]string{"country_code": countryCode}, values: country.now})
		}
		synthetic = append(synthetic, stormSyntheticSeries{part: "country_peak", labels: map[string]string{"country_code": countryCode}, values: country.peak})
	}
	for version, values := range series.builds {
		synthetic = append(synthetic, stormSyntheticSeries{part: "build_loss", labels: map[string]string{"version": version}, values: values})
	}
	for part, values := range series.totals {
		synthetic = append(synthetic, stormSyntheticSeries{part: part, labels: map[string]string{}, values: values})
	}
	return stormSyntheticResponse(t, stormSyntheticStart(providerFleetTestPoints), time.Minute, synthetic)
}

func TestProviderFleetDepartureJudgesCountries(t *testing.T) {
	probe := providerFleetDepartureProbe{}
	const (
		departure  = "provider-fleet-departure main/provider-fleet"
		visibility = "provider-fleet-departure-unobservable main/provider-fleet"
	)

	// the 2026-11-05 13:20Z blip: Spain at 117 of 2,071 for one refresh
	blip := providerFleetTestHealthy()
	blipNow := stormRepeat(2071, providerFleetTestPoints)
	for index := 5; index < providerFleetTestPoints; index++ {
		blipNow[index] = 117
	}
	blip.countries["ES"] = providerFleetTestCountry{now: blipNow, peak: stormRepeat(2071, providerFleetTestPoints)}

	// 2026-11-04 17:30Z: the US lost 19,817 providers, a third of them
	usFleet := providerFleetTestHealthy()
	usFleet.countries["US"] = providerFleetTestCountry{now: stormRepeat(46107, providerFleetTestPoints), peak: stormRepeat(65924, providerFleetTestPoints)}

	// a small country losing every provider is under the minimum
	small := providerFleetTestHealthy()
	small.countries["PH"] = providerFleetTestCountry{now: stormRepeat(0, providerFleetTestPoints), peak: stormRepeat(600, providerFleetTestPoints)}

	// a refresh where the gauge exported no country at all
	gappy := providerFleetTestDeparted()
	for countryCode, country := range gappy.countries {
		now := append([]float64{}, country.now...)
		now[20] = math.NaN()
		gappy.countries[countryCode] = providerFleetTestCountry{now: now, peak: country.peak}
	}

	for _, test := range []struct {
		name   string
		series providerFleetTestSeries
		want   map[string]string
	}{
		{name: "healthy", series: providerFleetTestHealthy(), want: map[string]string{departure: "healthy", visibility: "healthy"}},
		{name: "the fleet left", series: providerFleetTestDeparted(), want: map[string]string{departure: "warn", visibility: "healthy"}},
		{name: "one refresh's blip", series: blip, want: map[string]string{departure: "hold"}},
		{name: "a large country's fleet under half of it", series: usFleet, want: map[string]string{departure: "healthy"}},
		{name: "a small country emptied", series: small, want: map[string]string{departure: "healthy"}},
		{name: "a refresh exported nothing", series: gappy, want: map[string]string{departure: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, providerFleetTestResponse(t, test.series), nil)
		requireStormStates(t, test.name, states, test.want)
	}

	nothing := providerFleetTestSeries{totals: providerFleetTestHealthy().totals}
	states := stormFindingStates(t, probe, stormSyntheticNow, providerFleetTestResponse(t, nothing), nil)
	if len(states) != 1 || states[visibility] != "unobservable" {
		t.Fatalf("nothing exported by country: %v", states)
	}
}

// The alert names the country, the totals beside it and the build that left,
// and the query reads what §28.3 names.
func TestProviderFleetDepartureAlertShowsTheWholeDeparture(t *testing.T) {
	settings := DefaultProviderFleetDepartureSettings()
	if settings.PeakWindow != 3*time.Hour || settings.MinLoss != 1000 || settings.MinLossShare != 0.5 || settings.Hold != 45*time.Minute {
		t.Fatalf("§28.3 policy changed: %+v", settings)
	}
	query := providerFleetDepartureQuery("main", settings)
	for _, want := range []string{
		`(max by (country_code) (urnetwork_stats_online_providers_by_country{env="main"}) or 0 * max by (country_code) (max_over_time(urnetwork_stats_online_providers_by_country{env="main"}[3h]))) and on () count(urnetwork_stats_online_providers_by_country{env="main"})`,
		`topk(3, max by (version) (max_over_time(urnetwork_stats_device_versions{env="main",population="provider"}[3h])) - (`,
		`max_over_time(sum(max by (host, block) (urnetwork_connect_connected_clients{env="main"}))[3h:1m])`,
		`max_over_time(sum(max by (host) (process_open_fds{env="main",service="gossip"}))[3h:1m])`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost %s:\n%s", want, query)
		}
	}

	alerts := runStormSynthetic(t, NewProviderFleetDepartureSignal(), stormSyntheticNow, providerFleetTestResponse(t, providerFleetTestDeparted()), nil)
	found := false
	for _, alert := range alerts {
		if alert.Class != "provider-fleet-departure" {
			continue
		}
		found = true
		for _, want := range []string{
			"countries=VN:11221->216(-98%)",
			"online_providers=102307/114686",
			"connect_clients=139823/153528",
			"gossip_open_fds=3296/15576",
			"builds_lost=2026.10.22+1078873780-localfix2:-12470,2026.10.27+1083160450:-85",
		} {
			if !strings.Contains(alert.Observed, want) {
				t.Fatalf("departure alert lost %q: %s", want, alert.Observed)
			}
		}
		if alert.SignalNumber != "28.3" || alert.Severity != SeverityWarn || !strings.Contains(alert.Symptom, "VN") {
			t.Fatalf("departure alert = %+v", alert)
		}
	}
	if !found {
		t.Fatalf("no departure alert in %+v", alerts)
	}
}

func TestProviderFleetDepartureRejectsInvalidPolicyWithoutReading(t *testing.T) {
	signal := NewProviderFleetDepartureSignalWithSettings(&ProviderFleetDepartureSettings{})
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatalf("read its source under an invalid policy")
		return "", nil
	}}
	alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "provider-fleet-departure-unobservable" || !strings.Contains(alerts[0].Observed, "invalid_policy") {
		t.Fatalf("%v %+v", err, alerts)
	}
}
