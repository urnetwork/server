package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

// A synthetic answer of fifteen one-minute steps over crisp's and fireside's
// g1-g10: each host's highest ratio, each block's, and the host's GC cycles
// a minute and lowest limit. Every series reads as a healthy host (0.6 of
// the limit, 0.5 per block, 1 GC a minute, 6500 MiB) unless overridden by
// "part key"; an override of nil leaves the series out, and NaN leaves a
// step out.
func proxyHeapLimitResponse(t testing.TB, overrides map[string][]float64) string {
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
		add("heap_limit", host, labels, 0.6)
		add("gc_per_minute", host, labels, 1)
		add("limit_mib", host, labels, 6500)
		for _, block := range stormMainServiceBlocks["proxy"] {
			add("block_heap_limit", host+"/"+block, map[string]string{"host": host, "block": block}, 0.5)
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(15), time.Minute, series)
}

// The band is strict and needs fifteen minutes; the limit is soft, so a
// ratio above 1 is beyond; a maximum over fewer blocks stands only beyond
// the level; the GC and limit context never makes a host unknown.
func TestProxyHeapLimitBands(t *testing.T) {
	probe := proxyHeapLimitProbe{}
	crisp, fireside := "proxy-heap-limit crisp", "proxy-heap-limit fireside"
	visibility := "proxy-heap-limit-unobservable main/proxy-heap-limit"
	gap := stormRepeat(0.6, 15)
	gap[7] = math.NaN()
	missingFireside := map[string][]float64{"heap_limit fireside": nil, "gc_per_minute fireside": nil, "limit_mib fireside": nil}
	for _, block := range stormMainServiceBlocks["proxy"] {
		missingFireside["block_heap_limit fireside/"+block] = nil
	}
	for _, test := range []struct {
		name      string
		overrides map[string][]float64
		want      map[string]string
	}{
		{name: "healthy", want: map[string]string{crisp: "healthy", fireside: "healthy", visibility: "healthy"}},
		{name: "at the level", overrides: map[string][]float64{"heap_limit crisp": stormRepeat(0.95, 15)}, want: map[string]string{crisp: "healthy", visibility: "healthy"}},
		{name: "above the level", overrides: map[string][]float64{"heap_limit crisp": stormRepeat(0.951, 15)}, want: map[string]string{crisp: "warn", fireside: "healthy", visibility: "healthy"}},
		{name: "past the soft limit", overrides: map[string][]float64{"heap_limit fireside": stormRepeat(1.04, 15)}, want: map[string]string{crisp: "healthy", fireside: "warn"}},
		{name: "fourteen minutes", overrides: map[string][]float64{"heap_limit crisp": append([]float64{0.94}, stormRepeat(0.99, 14)...)}, want: map[string]string{crisp: "hold", visibility: "healthy"}},
		{name: "a process without a limit reads near zero", overrides: map[string][]float64{"heap_limit crisp": stormRepeat(4e-10, 15), "limit_mib crisp": stormRepeat(8.796093022208e12, 15)}, want: map[string]string{crisp: "healthy", visibility: "healthy"}},
		{name: "a placed block missing within the level", overrides: map[string][]float64{"block_heap_limit crisp/g4": nil}, want: map[string]string{crisp: "hold", fireside: "healthy", visibility: "unobservable"}},
		{name: "a placed block missing beyond the level", overrides: map[string][]float64{"heap_limit crisp": stormRepeat(0.97, 15), "block_heap_limit crisp/g4": nil}, want: map[string]string{crisp: "warn", visibility: "unobservable"}},
		{name: "a missing host", overrides: missingFireside, want: map[string]string{crisp: "healthy", fireside: "hold", visibility: "unobservable"}},
		{name: "a gap", overrides: map[string][]float64{"heap_limit crisp": gap}, want: map[string]string{crisp: "hold", visibility: "unobservable"}},
		{name: "context alone missing", overrides: map[string][]float64{"heap_limit crisp": stormRepeat(0.97, 15), "gc_per_minute crisp": nil, "limit_mib crisp": nil}, want: map[string]string{crisp: "warn", visibility: "healthy"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, proxyHeapLimitResponse(t, test.overrides), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

// The alert carries the host's ratio, its GC cycles a minute and its limit,
// and names the blocks beyond the level; missing context reads unknown.
func TestProxyHeapLimitAlerts(t *testing.T) {
	response := proxyHeapLimitResponse(t, map[string][]float64{
		"heap_limit fireside":          stormRepeat(0.97, 15),
		"gc_per_minute fireside":       stormRepeat(12.5, 15),
		"block_heap_limit fireside/g2": stormRepeat(0.97, 15),
		"block_heap_limit fireside/g5": stormRepeat(0.96, 15),
		"block_heap_limit fireside/g7": stormRepeat(0.95, 15),
	})
	alerts := runStormSynthetic(t, NewProxyHeapLimitSignal(), stormSyntheticNow, response, nil)
	requireStormAlerts(t, "at the limit", alerts, map[string][]string{"proxy-heap-limit": {"fireside"}})
	alert := stormAlertsByTarget(alerts, "proxy-heap-limit")["fireside"]
	if alert.Severity != SeverityWarn || alert.SignalNumber != "26.13" || alert.unobservable {
		t.Fatalf("%+v", alert)
	}
	for _, want := range []string{"host=fireside", "heap_and_stacks_over_limit_now=0.970", "level=0.95", "gc_cycles_per_minute_now=12.5", "limit_mib_now=6500", "blocks_over_level_now=g2:0.970,g5:0.960 "} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("observed %q lacks %q", alert.Observed, want)
		}
	}

	response = proxyHeapLimitResponse(t, map[string][]float64{"heap_limit crisp": stormRepeat(0.98, 15), "gc_per_minute crisp": nil, "limit_mib crisp": nil})
	alert = stormAlertsByTarget(runStormSynthetic(t, NewProxyHeapLimitSignal(), stormSyntheticNow, response, nil), "proxy-heap-limit")["crisp"]
	if !strings.Contains(alert.Observed, "gc_cycles_per_minute_now=unknown limit_mib_now=unknown") {
		t.Fatalf("observed %q", alert.Observed)
	}
}

// A healthy history for the shared §26 diagnostics.
func proxyHeapLimitHealthyResponse(t testing.TB) string {
	return proxyHeapLimitResponse(t, nil)
}

func TestProxyHeapLimitQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultProxyHeapLimitSettings()
	if settings.Level != 0.95 || settings.Hold != 15*time.Minute || settings.GcWindow != 5*time.Minute || settings.Service != "proxy" || settings.MaxListed != 8 {
		t.Fatalf("§26.13 policy changed: %+v", settings)
	}
	query := proxyHeapLimitQuery("main", settings)
	for _, want := range []string{
		`max by (host) ((go_memstats_heap_inuse_bytes{env="main",service="proxy"} + go_memstats_stack_inuse_bytes{env="main",service="proxy"}) / go_gc_gomemlimit_bytes{env="main",service="proxy"})`,
		`max by (host) (60 * rate(go_gc_duration_seconds_count{env="main",service="proxy"}[5m]))`,
		`min by (host) (go_gc_gomemlimit_bytes{env="main",service="proxy"}) / 1048576`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost %s:\n%s", want, query)
		}
	}
}
