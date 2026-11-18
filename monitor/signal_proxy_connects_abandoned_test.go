package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

// A synthetic answer of fifteen one-minute steps over crisp's and fireside's
// g1-g10: each host's abandoned rate and its reference, and the blocks whose
// rates it sums. Every series reads healthy (3/s against a reference of
// 2.5/s, one process per block) unless overridden by "part key"; an override
// of nil leaves the series out, and NaN leaves a step out.
func proxyConnectsAbandonedResponse(t testing.TB, overrides map[string][]float64) string {
	series := []stormSyntheticSeries{}
	add := func(part, key string, labels map[string]string, value float64) {
		values, overridden := overrides[part+" "+key]
		if !overridden {
			values = stormRepeat(value, 15)
		}
		if values != nil {
			series = append(series, stormSyntheticSeries{part: part, labels: labels, values: values})
		}
	}
	for _, host := range []string{"crisp", "fireside"} {
		labels := map[string]string{"host": host}
		add("abandoned", host, labels, 3)
		add("reference", host, labels, 2.5)
		for _, block := range stormMainServiceBlocks["proxy"] {
			add("slots", host+"/"+block, map[string]string{"host": host, "block": block}, 1)
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(15), time.Minute, series)
}

// The band needs both the rise and the floor, strictly, for fifteen
// minutes; at or under the floor a host reads within whatever its
// reference; above it a zero reference is a rise and a missing one is
// unknown; a sum over fewer blocks stands only beyond.
func TestProxyConnectsAbandonedBandsAndReference(t *testing.T) {
	probe := proxyConnectsAbandonedProbe{}
	crisp, fireside := "proxy-connects-abandoned crisp", "proxy-connects-abandoned fireside"
	visibility := "proxy-connects-abandoned-unobservable main/proxy-connects-abandoned"
	gap := stormRepeat(3, 15)
	gap[3] = math.NaN()
	missingFireside := map[string][]float64{"abandoned fireside": nil, "reference fireside": nil}
	for _, block := range stormMainServiceBlocks["proxy"] {
		missingFireside["slots fireside/"+block] = nil
	}
	rising := func(rate, reference float64) map[string][]float64 {
		return map[string][]float64{"abandoned crisp": stormRepeat(rate, 15), "reference crisp": stormRepeat(reference, 15)}
	}
	for _, test := range []struct {
		name      string
		overrides map[string][]float64
		want      map[string]string
	}{
		{name: "healthy", want: map[string]string{crisp: "healthy", fireside: "healthy", visibility: "healthy"}},
		{name: "above the floor at the rise", overrides: rising(150, 50), want: map[string]string{crisp: "healthy", visibility: "healthy"}},
		{name: "at the floor far above the reference", overrides: rising(50, 2), want: map[string]string{crisp: "healthy", visibility: "healthy"}},
		{name: "above the rise under the floor", overrides: rising(40, 2), want: map[string]string{crisp: "healthy", visibility: "healthy"}},
		{name: "above both", overrides: rising(200, 50), want: map[string]string{crisp: "warn", fireside: "healthy", visibility: "healthy"}},
		{name: "fourteen minutes", overrides: map[string][]float64{"abandoned crisp": append([]float64{100}, stormRepeat(300, 14)...), "reference crisp": stormRepeat(50, 15)}, want: map[string]string{crisp: "hold", visibility: "healthy"}},
		{name: "a zero reference under the floor", overrides: rising(10, 0), want: map[string]string{crisp: "healthy", visibility: "healthy"}},
		{name: "a missing reference under the floor", overrides: map[string][]float64{"abandoned crisp": stormRepeat(10, 15), "reference crisp": nil}, want: map[string]string{crisp: "healthy", visibility: "healthy"}},
		{name: "a zero reference above the floor is a rise from nothing", overrides: rising(80, 0), want: map[string]string{crisp: "warn", visibility: "healthy"}},
		{name: "a missing reference above the floor is unknown", overrides: map[string][]float64{"abandoned crisp": stormRepeat(80, 15), "reference crisp": nil}, want: map[string]string{crisp: "hold", visibility: "unobservable"}},
		{name: "a placed block missing within the band", overrides: map[string][]float64{"slots crisp/g4": nil}, want: map[string]string{crisp: "hold", fireside: "healthy", visibility: "unobservable"}},
		{name: "a placed block missing beyond the band", overrides: map[string][]float64{"abandoned crisp": stormRepeat(400, 15), "reference crisp": stormRepeat(50, 15), "slots crisp/g4": nil}, want: map[string]string{crisp: "warn", visibility: "unobservable"}},
		{name: "a missing host", overrides: missingFireside, want: map[string]string{crisp: "healthy", fireside: "hold", visibility: "unobservable"}},
		{name: "a gap", overrides: map[string][]float64{"abandoned crisp": gap}, want: map[string]string{crisp: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, proxyConnectsAbandonedResponse(t, test.overrides), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

// The alert carries the rate, its reference and the rise, and says that
// bots raise it too.
func TestProxyConnectsAbandonedAlerts(t *testing.T) {
	response := proxyConnectsAbandonedResponse(t, map[string][]float64{"abandoned fireside": stormRepeat(876.4, 15), "reference fireside": stormRepeat(114.1, 15)})
	alerts := runStormSynthetic(t, NewProxyConnectsAbandonedSignal(), stormSyntheticNow, response, nil)
	requireStormAlerts(t, "rising", alerts, map[string][]string{"proxy-connects-abandoned": {"fireside"}})
	alert := stormAlertsByTarget(alerts, "proxy-connects-abandoned")["fireside"]
	if alert.Severity != SeverityWarn || alert.SignalNumber != "26.14" || alert.unobservable {
		t.Fatalf("%+v", alert)
	}
	if !strings.Contains(alert.Observed, "host=fireside abandoned_per_second_now=876.4 reference_per_second=114.1 rise_now=7.68 rise_level=3 floor_per_second=50") {
		t.Fatalf("observed %q", alert.Observed)
	}
	if !strings.Contains(alert.Context, "Bots that open CONNECTs and drop them") || !strings.Contains(alert.Context, "mint refusals") {
		t.Fatalf("context %q", alert.Context)
	}

	response = proxyConnectsAbandonedResponse(t, map[string][]float64{"abandoned crisp": stormRepeat(80, 15), "reference crisp": stormRepeat(0, 15)})
	alert = stormAlertsByTarget(runStormSynthetic(t, NewProxyConnectsAbandonedSignal(), stormSyntheticNow, response, nil), "proxy-connects-abandoned")["crisp"]
	if !strings.Contains(alert.Observed, "reference_per_second=0.0 rise_now=+Inf") {
		t.Fatalf("observed %q", alert.Observed)
	}
}

// A healthy history for the shared §26 diagnostics.
func proxyConnectsAbandonedHealthyResponse(t testing.TB) string {
	return proxyConnectsAbandonedResponse(t, nil)
}

func TestProxyConnectsAbandonedQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultProxyConnectsAbandonedSettings()
	if settings.RiseRatio != 3 || settings.MinRate != 50 || settings.Hold != 15*time.Minute || settings.Window != 15*time.Minute ||
		settings.BaselineSpan != 24*time.Hour || settings.BaselineStep != 15*time.Minute || settings.BaselineOffset != 30*time.Minute || settings.Service != "proxy" {
		t.Fatalf("§26.14 policy changed: %+v", settings)
	}
	abandoned := `sum by (host) (rate(urnetwork_proxy_ingress_events_total{env="main",service="proxy",protocol="http",reason="connect_client_gone"}[15m]))`
	query := proxyConnectsAbandonedQuery("main", settings)
	for _, want := range []string{abandoned, `quantile_over_time(0.5, ` + abandoned + `[24h:15m] offset 30m)`} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost %s:\n%s", want, query)
		}
	}
}
