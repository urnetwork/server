// Exercises generation authority at all four public Proxy signal boundaries.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Supplies complete synthetic owner telemetry and one independent real fault.
type proxyGenerationTestSignal struct {
	key           string
	number        string
	constructor   func() Signal
	values        map[string]float64
	positive      map[string]float64
	positiveClass string
	sourceTimes   bool
	withoutRss    bool
}

// Models distinct process nonces; omissions and clock defects stay local.
type proxyGenerationTestProcess struct {
	instance       string
	block          string
	start          float64
	omit           map[string]bool
	changes        map[string]float64
	duplicateStart bool
	onlyRates      bool
	sampleOffsets  map[string]time.Duration
	sourceOffsets  map[string]time.Duration
}

// A complete old pool must not certify a newer missing-RSS replacement.
func TestProxyPoolGenerationOldHealthyNewPartial(t *testing.T) {
	checkProxyGenerationOldHealthyNewPartial(t, proxyGenerationTestSignals()[0])
}

// A complete old cache must not certify a newer missing-RSS replacement.
func TestProxyCacheGenerationOldHealthyNewPartial(t *testing.T) {
	checkProxyGenerationOldHealthyNewPartial(t, proxyGenerationTestSignals()[1])
}

// A complete old runtime must not certify a newer missing-RSS replacement.
func TestProxyRuntimeGenerationOldHealthyNewPartial(t *testing.T) {
	checkProxyGenerationOldHealthyNewPartial(t, proxyGenerationTestSignals()[2])
}

// A complete old transport must not certify a newer missing-RSS replacement.
func TestProxyTransportGenerationOldHealthyNewPartial(t *testing.T) {
	checkProxyGenerationOldHealthyNewPartial(t, proxyGenerationTestSignals()[3])
}

// Keeps each family's red assertion independently reachable before repair.
func checkProxyGenerationOldHealthyNewPartial(t *testing.T, signal proxyGenerationTestSignal) {
	t.Helper()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, reverse := range []bool{false, true} {
		old := proxyGenerationTestProcess{instance: "zz-old", block: "lane-a", start: float64(now.Add(-time.Hour).Unix())}
		current := proxyGenerationTestProcess{
			instance: "aa-new", block: "lane-a", start: float64(now.Add(-time.Minute).Unix()),
			omit: map[string]bool{"process_resident_memory_bytes": true},
		}
		alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, reverse, old, current)
		alert := requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
		if len(alerts) != 1 || alert.SignalKey != signal.key || alert.SignalNumber != signal.number || alert.Sustain != 1 {
			t.Fatalf("%s reverse=%v: wrong current-generation alert: %+v", signal.key, reverse, alerts)
		}
		for _, want := range []string{"aa-new[missing-or-invalid-rss]", "selected_proxy_identities=0", "unknown_generation_slots=1", "not a proven missing collector", "does not justify a product rollout or restart"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("%s reverse=%v: generation coverage omitted %q: %s", signal.key, reverse, want, alert.Markdown())
			}
		}
		if strings.Contains(alert.Observed, "zz-old") || strings.Contains(alert.Markdown(), "synthetic-private-label") {
			t.Fatalf("%s: old identity or unrelated label escaped: %s", signal.key, alert.Markdown())
		}
		if !alert.unobservable || !isVisibilityAlertClass(alert.Class) {
			t.Fatalf("%s: generation unknown may clear previous findings", signal.key)
		}
		if alert.Identity() != signal.constructor().ID()+"|proxy-process-generation-unobservable|proxy-fleet|metrics.example" {
			t.Fatalf("%s: current-generation alert identity changed with evidence: %s", signal.key, alert.Identity())
		}
	}
}

// Complete other slots retain affirmative faults beside every ownership gap.
func TestProxyGenerationUnknownPreservesIndependentPositive(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, signal := range proxyGenerationTestSignals() {
		for _, missing := range []string{"process_resident_memory_bytes", "process_start_time_seconds"} {
			old := proxyGenerationTestProcess{instance: "zz-old", block: "lane-a", start: float64(now.Add(-time.Hour).Unix())}
			current := proxyGenerationTestProcess{
				instance: "aa-new", block: "lane-a", start: float64(now.Add(-time.Minute).Unix()),
				omit: map[string]bool{missing: true},
			}
			positive := proxyGenerationTestProcess{
				instance: "independent", block: "lane-b", start: float64(now.Add(-time.Minute).Unix()), changes: signal.positive,
			}
			alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a", "lane-b"}, false, old, current, positive)
			requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
			fault := requireAlertClass(t, alerts, signal.positiveClass)
			if len(alerts) != 2 || !strings.Contains(fault.Observed, "lane-b#independent") || strings.Contains(fault.Observed, "lane-a") {
				t.Fatalf("%s missing=%s: unknown contaminated a complete independent fault: %+v", signal.key, missing, alerts)
			}
		}
	}
}

// Random process nonce ordering cannot break equal newest starts, in either order.
func TestProxyGenerationEqualNewestStartsAreAmbiguous(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, signal := range proxyGenerationTestSignals() {
		for _, reverse := range []bool{false, true} {
			first := proxyGenerationTestProcess{instance: "aa-first", block: "lane-a", start: float64(now.Add(-time.Minute).Unix())}
			second := proxyGenerationTestProcess{instance: "zz-second", block: "lane-a", start: first.start, changes: signal.positive}
			alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, reverse, first, second)
			alert := requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
			if len(alerts) != 1 || !strings.Contains(alert.Observed, "equal-newest-starts") {
				t.Fatalf("%s reverse=%v: ambiguous generation became an owner diagnosis: %+v", signal.key, reverse, alerts)
			}
		}
	}
}

// Repeated start cells cannot let response order choose generation ownership.
func TestProxyGenerationDuplicateStartIsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, signal := range proxyGenerationTestSignals() {
		for _, reverse := range []bool{false, true} {
			process := proxyGenerationTestProcess{instance: "duplicated", block: "lane-a", start: float64(now.Add(-time.Minute).Unix()), duplicateStart: true}
			alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, reverse, process)
			alert := requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
			if len(alerts) != 1 || !strings.Contains(alert.Observed, "duplicate-process-start") {
				t.Fatalf("%s reverse=%v: duplicate start became a healthy generation: %+v", signal.key, reverse, alerts)
			}
		}
	}
}

// Missing, zero, or future starts and zero RSS never revive a complete old process.
func TestProxyGenerationInvalidIdentityClocksStayUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, signal := range proxyGenerationTestSignals() {
		for _, boundary := range []string{"missing-start", "zero-start", "future-start", "zero-rss", "missing-instance"} {
			old := proxyGenerationTestProcess{instance: "zz-old", block: "lane-a", start: float64(now.Add(-time.Hour).Unix())}
			current := proxyGenerationTestProcess{instance: "aa-new", block: "lane-a", start: float64(now.Add(-time.Minute).Unix())}
			want := "invalid-process-start"
			switch boundary {
			case "missing-start":
				current.omit = map[string]bool{"process_start_time_seconds": true}
				want = "missing-process-start"
			case "zero-start":
				current.start = 0
			case "future-start":
				current.start = float64(now.Add(time.Second).Unix())
			case "zero-rss":
				current.changes = map[string]float64{"process_resident_memory_bytes": 0}
				want = "missing-or-invalid-rss"
			case "missing-instance":
				current.instance = ""
				want = "missing-process-label"
			}
			alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, false, old, current)
			alert := requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
			if len(alerts) != 1 || !strings.Contains(alert.Observed, want) {
				t.Fatalf("%s %s: invalid generation certified health: %+v", signal.key, boundary, alerts)
			}
		}
	}
}

// A stale or future identity cell cannot hide its otherwise current cohort.
func TestProxyGenerationRejectedIdentityClockDoesNotReviveOld(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, signal := range proxyGenerationTestSignals() {
		for _, name := range []string{"process_start_time_seconds", "process_resident_memory_bytes"} {
			for _, offset := range []time.Duration{-2 * time.Minute, 31 * time.Second} {
				old := proxyGenerationTestProcess{instance: "zz-old", block: "lane-a", start: float64(now.Add(-time.Hour).Unix())}
				current := proxyGenerationTestProcess{
					instance: "aa-new", block: "lane-a", start: float64(now.Add(-time.Minute).Unix()),
					sampleOffsets: map[string]time.Duration{name: offset},
				}
				alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, false, old, current)
				alert := requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
				if len(alerts) != 1 || !strings.Contains(alert.Observed, "unknown_generation_slots=1") {
					t.Fatalf("%s %s offset=%s: rejected identity clock revived an old process: %+v", signal.key, name, offset, alerts)
				}
			}
		}
	}
}

// New complete generations start their own counters and do not inherit old faults.
func TestProxyGenerationNewValidResetSupersedesDrainingProcesses(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, signal := range proxyGenerationTestSignals() {
		for _, reverse := range []bool{false, true} {
			old := proxyGenerationTestProcess{instance: "zz-old", block: "lane-a", start: float64(now.Add(-time.Hour).Unix()), changes: signal.positive}
			oldTie := old
			oldTie.instance = "yy-old-tie"
			oldTie.omit = map[string]bool{"process_resident_memory_bytes": true}
			current := proxyGenerationTestProcess{instance: "aa-new", block: "lane-a", start: float64(now.Add(-time.Minute).Unix())}
			alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, reverse, old, oldTie, current)
			if len(alerts) != 0 {
				t.Fatalf("%s reverse=%v: a valid replacement inherited retired pressure or old start ties: %+v", signal.key, reverse, alerts)
			}
		}
	}
}

// A current fault must still fire when its draining predecessor was healthy.
func TestProxyGenerationNewValidPositiveSupersedesHealthyOld(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, signal := range proxyGenerationTestSignals() {
		old := proxyGenerationTestProcess{instance: "zz-old", block: "lane-a", start: float64(now.Add(-time.Hour).Unix())}
		current := proxyGenerationTestProcess{instance: "aa-new", block: "lane-a", start: float64(now.Add(-time.Minute).Unix()), changes: signal.positive}
		alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, false, old, current)
		alert := requireAlertClass(t, alerts, signal.positiveClass)
		if len(alerts) != 1 || !strings.Contains(alert.Observed, "#aa-new") || strings.Contains(alert.Observed, "#zz-old") {
			t.Fatalf("%s: newest complete fault was hidden or conflated: %+v", signal.key, alerts)
		}
	}
}

// Transport range results may survive after all instantaneous families retire.
func TestProxyGenerationTransportPriorRangeDoesNotPoisonCurrent(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	signal := proxyGenerationTestSignals()[3]
	retired := proxyGenerationTestProcess{instance: "retired", block: "lane-a", onlyRates: true}
	current := proxyGenerationTestProcess{instance: "current", block: "lane-a", start: float64(now.Add(-time.Minute).Unix())}
	alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, false, retired, current)
	if len(alerts) != 0 {
		t.Fatalf("prior range contaminated current transport generation: %+v", alerts)
	}
	alerts = runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, false, retired)
	alert := requireAlertClass(t, alerts, "proxy-transport-unobservable")
	if len(alerts) != 1 || !strings.Contains(alert.Observed, "absent_identities=1") {
		t.Fatalf("prior range certified missing instantaneous coverage: %+v", alerts)
	}
}

// Generation uncertainty does not erase the independently expected slot census.
func TestProxyGenerationTransportMissingExpectedSlotRemainsVisible(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	signal := proxyGenerationTestSignals()[3]
	current := proxyGenerationTestProcess{
		instance: "partial", block: "lane-a", start: float64(now.Add(-time.Minute).Unix()),
		omit: map[string]bool{"process_resident_memory_bytes": true},
	}
	alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a", "lane-b"}, false, current)
	requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
	alert := requireAlertClass(t, alerts, "proxy-transport-unobservable")
	if len(alerts) != 2 || !strings.Contains(alert.Observed, "absent_identities=1 absent=proxy-node.example/lane-b") {
		t.Fatalf("generation gap replaced or duplicated the expected-slot census: %+v", alerts)
	}
}

// The public adapters retain their independently filtered metric cohorts.
func runProxyGenerationTestSignal(t testing.TB, signal proxyGenerationTestSignal, now time.Time, blocks []string, reverse bool, processes ...proxyGenerationTestProcess) Alerts {
	t.Helper()
	rows := []map[string]any{}
	for _, process := range processes {
		values := map[string]float64{}
		for name, value := range signal.values {
			values[name] = value
		}
		if !signal.withoutRss {
			values["process_resident_memory_bytes"] = 1024 * 1024 * 1024
		}
		values["process_start_time_seconds"] = process.start
		for name, value := range process.changes {
			values[name] = value
		}
		if process.onlyRates {
			values = map[string]float64{proxyTransportCpuRateMetric: 1, proxyTransportPreemptionRateMetric: 1}
		}
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		add := func(name string, value float64) {
			if process.omit[name] {
				return
			}
			labels := map[string]string{
				"env": "synthetic", "job": "proxy", "service": "proxy",
				"host": "proxy-node.example", "block": process.block, "instance": process.instance,
				"monitor_metric": name, "unrelated_label": "synthetic-private-label",
			}
			if name == "urnetwork_build_info" {
				labels["version"] = "synthetic-config"
			}
			sampleTime := now.Add(process.sampleOffsets[strings.TrimSuffix(name, "__source_timestamp")])
			rows = append(rows, map[string]any{
				"metric": labels,
				"value":  []any{float64(sampleTime.Unix()), strconv.FormatFloat(value, 'f', -1, 64)},
			})
		}
		for _, name := range names {
			if process.omit[name] {
				continue
			}
			add(name, values[name])
			if name == "process_start_time_seconds" && process.duplicateStart {
				add(name, values[name]-1)
			}
			if signal.sourceTimes && !process.onlyRates {
				add(name+"__source_timestamp", float64(now.Add(process.sourceOffsets[name]).Unix()))
			}
		}
	}
	if reverse {
		slices.Reverse(rows)
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		decoded, err := url.QueryUnescape(command)
		if err != nil || host.Name != "metrics.example" || !strings.Contains(decoded, `env="synthetic",job="proxy"`) ||
			!strings.Contains(decoded, "process_start_time_seconds") || !signal.withoutRss && !strings.Contains(decoded, "process_resident_memory_bytes") ||
			!strings.Contains(decoded, "timestamp(") || !strings.Contains(decoded, "monitor_metric") {
			return "", fmt.Errorf("%s: unexpected synthetic metric query", signal.key)
		}
		if signal.withoutRss && (strings.Contains(decoded, "process_resident_memory_bytes") || strings.Contains(decoded, "rate(")) {
			return "", fmt.Errorf("%s: target query acquired an unrelated RSS/rate dependency", signal.key)
		}
		return string(payload), nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.LogServices = []string{"proxy"}
	settings.LogServiceBlocks = map[string][]string{"proxy": append([]string(nil), blocks...)}
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "proxy-node.example", Proxy: &ProxyHostSettings{}},
		HostSettings{Name: "metrics.example", Roles: []string{"services"}},
	)
	alerts, err := signal.constructor().Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("%s: public signal execution failed: %v", signal.key, err)
	}
	return alerts
}

// Values are synthetic per-process gauges; counters start at zero after replacement.
func proxyGenerationTestSignals() []proxyGenerationTestSignal {
	return []proxyGenerationTestSignal{
		{
			key: "proxy-pool", number: "14.7a", constructor: NewProxyPoolSignal, sourceTimes: true,
			values: map[string]float64{
				"urnetwork_message_pool_capacity_bytes": 8 * 1024 * 1024 * 1024,
				"urnetwork_message_pool_retained_bytes": 0, "urnetwork_message_pool_packet_retained_bytes": 0,
				"urnetwork_message_pool_large_object_retained_bytes": 0, "urnetwork_message_pool_outstanding": 0,
			},
			positive:      map[string]float64{"urnetwork_message_pool_capacity_bytes": 24 * 1024 * 1024 * 1024},
			positiveClass: "proxy-message-pool-capacity",
		},
		{
			key: "proxy-cache", number: "14.7c", constructor: NewProxyCacheSignal,
			values: map[string]float64{
				"urnetwork_proxy_lock_cache_capacity": 16_384, "urnetwork_proxy_lock_cache_entries": 0,
				"urnetwork_proxy_lock_cache_hits_total": 0, "urnetwork_proxy_lock_cache_misses_total": 0,
				"urnetwork_proxy_lock_cache_expirations_total": 0, "urnetwork_proxy_lock_cache_evictions_total": 0,
			},
			positive:      map[string]float64{"urnetwork_proxy_lock_cache_capacity": 100, "urnetwork_proxy_lock_cache_entries": 101},
			positiveClass: "proxy-lock-cache-bound",
		},
		{
			key: "proxy-runtime", number: "14.7b", constructor: NewProxyRuntimeSignal,
			values: map[string]float64{
				"urnetwork_build_info": 1, "go_memstats_heap_alloc_bytes": 1024 * 1024,
				"go_memstats_heap_objects": 100, "go_goroutines": 10, "urnetwork_proxy_wg_peers": 0,
				"urnetwork_proxy_devices_live": 0, "urnetwork_proxy_device_memory_tracked_used_bytes": 0,
				"urnetwork_message_pool_retained_bytes": 0, "go_memstats_next_gc_bytes": 2 * 1024 * 1024,
				"go_gc_gogc_percent": 100, "go_memstats_stack_inuse_bytes": 1024, "go_memstats_last_gc_time_seconds": 0,
				"urnetwork_proxy_lifecycle_join_enabled": 1,
			},
			positive:      map[string]float64{"go_memstats_heap_alloc_bytes": 4 * 1024 * 1024 * 1024, "go_memstats_heap_objects": 30_000_000},
			positiveClass: "proxy-runtime-live-set",
		},
		{
			key: "proxy-transport", number: "14.6", constructor: NewProxyTransportSignal, sourceTimes: true,
			values: map[string]float64{
				"process_cpu_seconds_total": 0, "urnetwork_proxy_devices_live": 2,
				"urnetwork_proxy_device_memory_target_bytes":      48 * 1024 * 1024,
				"urnetwork_proxy_platform_transport_budget_bytes": 12 * 1024 * 1024,
				"urnetwork_proxy_platform_transport_used_bytes":   2 * 1024 * 1024,
				"urnetwork_proxy_platform_transports_max":         32, "urnetwork_proxy_platform_transports_used": 4,
				"urnetwork_proxy_platform_transports_pending_h1": 0, "urnetwork_proxy_platform_transports_pending_h1_bytes": 0,
				"urnetwork_proxy_platform_transport_slot_full_pending_h1_devices":                     0,
				"urnetwork_proxy_platform_transport_h3_preemptions_total":                             0,
				"urnetwork_proxy_platform_transport_slot_full_pending_h1_h3_preemptions_total":        0,
				"urnetwork_proxy_platform_transport_active_handoff_transports":                        0,
				"urnetwork_proxy_platform_transport_active_handoff_bytes":                             0,
				"urnetwork_proxy_platform_transport_slot_full_pending_h1_handoff_devices":             0,
				"urnetwork_proxy_platform_transport_slot_full_pending_h1_handoff_unsatisfied_devices": 0,
				"urnetwork_proxy_platform_transport_slot_full_pending_h1_demand_unsatisfied_devices":  0,
				"urnetwork_proxy_platform_transport_slot_full_pending_h1_window_unknown_devices":      0,
			},
			positive:      map[string]float64{"urnetwork_proxy_platform_transports_pending_h1": 1, "urnetwork_proxy_platform_transports_pending_h1_bytes": 4096},
			positiveClass: "proxy-transport-admission-pending",
		},
	}
}
