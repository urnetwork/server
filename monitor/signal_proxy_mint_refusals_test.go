package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

// A synthetic answer of ten one-minute steps over crisp's and fireside's
// g1-g10: each host's sum over its mature blocks, each block's rate and each
// block's newest process's uptime. Every series reads quiet (1 refusal/s per
// host, 0.1/s per block) and old (two hours) unless overridden by "part key";
// an override of nil leaves the series out, and NaN leaves a step out.
func proxyMintRefusalsResponse(t testing.TB, overrides map[string][]float64) string {
	series := []stormSyntheticSeries{}
	add := func(part, key string, labels map[string]string, value float64) {
		values, overridden := overrides[part+" "+key]
		if !overridden {
			values = stormRepeat(value, 10)
		}
		if values != nil {
			series = append(series, stormSyntheticSeries{part: part, labels: labels, values: values})
		}
	}
	for _, host := range []string{"crisp", "fireside"} {
		add("refusals", host, map[string]string{"host": host}, 1)
		for _, block := range stormMainServiceBlocks["proxy"] {
			labels := map[string]string{"host": host, "block": block}
			add("block_refusals", host+"/"+block, labels, 0.1)
			add("uptime", host+"/"+block, labels, 7200)
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(10), time.Minute, series)
}

// Overrides every block of a host.
func proxyMintRefusalsHostBlocks(host, part string, values []float64) map[string][]float64 {
	overrides := map[string][]float64{}
	for _, block := range stormMainServiceBlocks["proxy"] {
		overrides[part+" "+host+"/"+block] = values
	}
	return overrides
}

func proxyMintRefusalsMerge(maps ...map[string][]float64) map[string][]float64 {
	merged := map[string][]float64{}
	for _, one := range maps {
		for key, values := range one {
			merged[key] = values
		}
	}
	return merged
}

// The band is strict and needs ten judged minutes; a young block is left out
// of its host's sum; a host with no block old enough is left out of the
// band; a sum over fewer blocks stands only beyond the level.
func TestProxyMintRefusalsBandsAndUptimeRule(t *testing.T) {
	probe := proxyMintRefusalsProbe{}
	crisp, fireside := "proxy-mint-refusals crisp", "proxy-mint-refusals fireside"
	visibility := "proxy-mint-refusals-unobservable main/proxy-mint-refusals"
	young := []float64{1500, 1560, 1620, 1680, 1740, 1800, 1860, 1920, 1980, 2040}
	nan := stormRepeat(math.NaN(), 10)
	for _, test := range []struct {
		name      string
		overrides map[string][]float64
		want      map[string]string
	}{
		{name: "healthy", want: map[string]string{crisp: "healthy", fireside: "healthy", visibility: "healthy"}},
		{name: "at the level", overrides: map[string][]float64{"refusals crisp": stormRepeat(5, 10)}, want: map[string]string{crisp: "healthy", visibility: "healthy"}},
		{name: "above the level", overrides: map[string][]float64{"refusals crisp": stormRepeat(5.1, 10)}, want: map[string]string{crisp: "warn", fireside: "healthy", visibility: "healthy"}},
		{name: "nine minutes", overrides: map[string][]float64{"refusals crisp": append([]float64{4.9}, stormRepeat(80, 9)...)}, want: map[string]string{crisp: "hold", visibility: "healthy"}},
		{name: "a young block's burst is not its host's", overrides: map[string][]float64{"uptime crisp/g4": stormRepeat(600, 10), "block_refusals crisp/g4": stormRepeat(300, 10)}, want: map[string]string{crisp: "healthy", visibility: "healthy"}},
		{name: "a block that matures needs its rate from then on", overrides: map[string][]float64{"uptime crisp/g4": young, "block_refusals crisp/g4": append(stormRepeat(300, 6), stormRepeat(math.NaN(), 4)...)}, want: map[string]string{crisp: "hold", visibility: "unobservable"}},
		{name: "a host with no block old enough is left out", overrides: proxyMintRefusalsMerge(
			map[string][]float64{"refusals crisp": nil},
			proxyMintRefusalsHostBlocks("crisp", "uptime", stormRepeat(600, 10)),
			proxyMintRefusalsHostBlocks("crisp", "block_refusals", stormRepeat(80, 10)),
		), want: map[string]string{crisp: "hold", fireside: "healthy", visibility: "healthy"}},
		{name: "a restart through the hold neither fires nor clears", overrides: proxyMintRefusalsMerge(
			map[string][]float64{"refusals crisp": append(stormRepeat(math.NaN(), 6), stormRepeat(80, 4)...)},
			proxyMintRefusalsHostBlocks("crisp", "uptime", young),
		), want: map[string]string{crisp: "hold", visibility: "healthy"}},
		{name: "a placed block without a process", overrides: map[string][]float64{"uptime crisp/g4": nil, "block_refusals crisp/g4": nil}, want: map[string]string{crisp: "hold", fireside: "healthy", visibility: "unobservable"}},
		{name: "a sum without a placed block beyond the level stands", overrides: map[string][]float64{"refusals crisp": stormRepeat(20, 10), "uptime crisp/g4": nil, "block_refusals crisp/g4": nil}, want: map[string]string{crisp: "warn", visibility: "unobservable"}},
		{name: "an old block without a rate", overrides: map[string][]float64{"block_refusals crisp/g7": nil}, want: map[string]string{crisp: "hold", visibility: "unobservable"}},
		{name: "a missing host", overrides: proxyMintRefusalsMerge(
			map[string][]float64{"refusals fireside": nil},
			proxyMintRefusalsHostBlocks("fireside", "uptime", nil),
			proxyMintRefusalsHostBlocks("fireside", "block_refusals", nil),
		), want: map[string]string{crisp: "healthy", fireside: "hold", visibility: "unobservable"}},
		{name: "a gap in the sum", overrides: map[string][]float64{"refusals crisp": append(stormRepeat(1, 9), math.NaN())}, want: map[string]string{crisp: "hold", visibility: "unobservable"}},
		{name: "every series missing", overrides: proxyMintRefusalsMerge(
			map[string][]float64{"refusals crisp": nan, "refusals fireside": nan},
			proxyMintRefusalsHostBlocks("crisp", "uptime", nil),
			proxyMintRefusalsHostBlocks("fireside", "uptime", nil),
		), want: map[string]string{crisp: "hold", fireside: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, proxyMintRefusalsResponse(t, test.overrides), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

// The alert names the host, its refusals now and its busiest blocks; a
// missing block is listed by name in the visibility alert.
func TestProxyMintRefusalsAlerts(t *testing.T) {
	response := proxyMintRefusalsResponse(t, map[string][]float64{
		"refusals crisp":          stormRepeat(42.5, 10),
		"block_refusals crisp/g3": stormRepeat(20, 10),
		"block_refusals crisp/g8": stormRepeat(12.4, 10),
	})
	alerts := runStormSynthetic(t, NewProxyMintRefusalsSignal(), stormSyntheticNow, response, nil)
	requireStormAlerts(t, "refusing", alerts, map[string][]string{"proxy-mint-refusals": {"crisp"}})
	alert := stormAlertsByTarget(alerts, "proxy-mint-refusals")["crisp"]
	if alert.Severity != SeverityWarn || alert.SignalNumber != "26.12" || alert.unobservable {
		t.Fatalf("%+v", alert)
	}
	for _, want := range []string{"host=crisp", "refusals_per_second_now=42.5", "mature_blocks_now=10/10", "blocks_now=g3:20.0,g8:12.4,g1:0.1,"} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("observed %q lacks %q", alert.Observed, want)
		}
	}

	missing := proxyMintRefusalsResponse(t, map[string][]float64{"uptime fireside/g9": nil, "block_refusals fireside/g9": nil})
	alerts = runStormSynthetic(t, NewProxyMintRefusalsSignal(), stormSyntheticNow, missing, nil)
	requireStormAlerts(t, "missing", alerts, map[string][]string{"proxy-mint-refusals-unobservable": {"main/proxy-mint-refusals"}})
	if visibility := alerts[0]; !visibility.unobservable || !strings.Contains(visibility.Observed, "visibility_reason=evaluation_missing gaps=fireside/g9[missing]") {
		t.Fatalf("%+v", visibility)
	}
}

// A healthy history for the shared §26 diagnostics.
func proxyMintRefusalsHealthyResponse(t testing.TB) string {
	return proxyMintRefusalsResponse(t, nil)
}

func TestProxyMintRefusalsQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultProxyMintRefusalsSettings()
	if settings.WarnRefusals != 5 || settings.Hold != 10*time.Minute || settings.MinUptime != 30*time.Minute || settings.Window != 5*time.Minute || settings.Service != "proxy" || settings.MaxListed != 8 {
		t.Fatalf("§26.12 policy changed: %+v", settings)
	}
	want := `sum by (host) (sum by (host, block) (rate(urnetwork_hosted_mint_admission_refusals_total{env="main",service="proxy"}[5m])) and on (host, block) (time() - max by (host, block) (process_start_time_seconds{env="main",service="proxy"}) > 1800))`
	if query := proxyMintRefusalsQuery("main", settings); !strings.Contains(query, want) {
		t.Fatalf("query lost the catalog expression:\n%s", query)
	}
}
