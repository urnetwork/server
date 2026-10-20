// Exercises current-generation authority without adding RSS to the target query.
package monitor

import (
	"strings"
	"testing"
	"time"
)

// A current owner gauge without a trustworthy start cannot borrow an old target.
func TestProxyDeviceTargetGenerationPartialStartStaysUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	signal := proxyDeviceGenerationTestSignal()
	for _, boundary := range []string{"missing-start", "missing-source", "stale-source", "future-source", "zero-start", "future-start", "start-after-source"} {
		for _, reverse := range []bool{false, true} {
			old := proxyGenerationTestProcess{instance: "zz-old", block: "lane-a", start: float64(now.Add(-time.Hour).Unix())}
			current := proxyGenerationTestProcess{instance: "aa-new", block: "lane-a", start: float64(now.Add(-time.Minute).Unix())}
			switch boundary {
			case "missing-start":
				current.omit = map[string]bool{"process_start_time_seconds": true}
			case "missing-source":
				current.omit = map[string]bool{"process_start_time_seconds__source_timestamp": true}
			case "stale-source":
				current.sourceOffsets = map[string]time.Duration{"process_start_time_seconds": -2 * time.Minute}
			case "future-source":
				current.sourceOffsets = map[string]time.Duration{"process_start_time_seconds": 31 * time.Second}
			case "zero-start":
				current.start = 0
			case "future-start":
				current.start = float64(now.Add(time.Second).Unix())
			case "start-after-source":
				current.sourceOffsets = map[string]time.Duration{"process_start_time_seconds": -75 * time.Second}
			}
			alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, reverse, old, current)
			alert := requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
			if len(alerts) != 1 || alert.SignalKey != "proxy-device-target" || alert.SignalNumber != "14.7d" ||
				!strings.Contains(alert.Observed, "unknown_generation_slots=1") || !alert.unobservable || !isVisibilityAlertClass(alert.Class) {
				t.Fatalf("%s reverse=%v: a partial start revived the old target: %+v", boundary, reverse, alerts)
			}
			if alert.Identity() != "runtime/proxy-device-target|proxy-process-generation-unobservable|proxy-fleet|proxy-fleet" {
				t.Fatalf("%s: current target identity changed with evidence: %s", boundary, alert.Identity())
			}
			if strings.Contains(alert.Observed, "zz-old") || strings.Contains(alert.Markdown(), "synthetic-private-label") {
				t.Fatalf("%s: stale authority or unrelated labels escaped: %s", boundary, alert.Markdown())
			}
		}
	}
}

// Distinct nonces with one newest start cannot elect a target by lexical order.
func TestProxyDeviceTargetGenerationEqualStartsStayAmbiguous(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	signal := proxyDeviceGenerationTestSignal()
	for _, reverse := range []bool{false, true} {
		first := proxyGenerationTestProcess{instance: "aa-first", block: "lane-a", start: float64(now.Add(-time.Minute).Unix())}
		second := proxyGenerationTestProcess{instance: "zz-second", block: "lane-a", start: first.start, changes: signal.positive}
		alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, reverse, first, second)
		alert := requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
		if len(alerts) != 1 || !strings.Contains(alert.Observed, "equal-newest-starts") {
			t.Fatalf("reverse=%v: equal starts became a device-target fault: %+v", reverse, alerts)
		}
	}
}

// Known other-slot mismatches remain visible when one current owner is unknown.
func TestProxyDeviceTargetGenerationUnknownPreservesPositiveAndCoverage(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	signal := proxyDeviceGenerationTestSignal()
	old := proxyGenerationTestProcess{instance: "zz-old", block: "lane-a", start: float64(now.Add(-time.Hour).Unix())}
	current := proxyGenerationTestProcess{
		instance: "aa-new", block: "lane-a", start: float64(now.Add(-time.Minute).Unix()),
		omit: map[string]bool{"process_start_time_seconds": true},
	}
	positive := proxyGenerationTestProcess{instance: "independent", block: "lane-b", start: current.start, changes: signal.positive}
	alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a", "lane-b", "lane-c"}, false, old, current, positive)
	requireAlertClass(t, alerts, "proxy-process-generation-unobservable")
	fault := requireAlertClass(t, alerts, "proxy-device-target-mismatch")
	coverage := requireAlertClass(t, alerts, "proxy-device-target-unobservable")
	if len(alerts) != 3 || !strings.Contains(fault.Observed, "proxy-node.example/lane-b[") || strings.Contains(fault.Observed, "lane-a") ||
		!strings.Contains(coverage.Observed, "proxy-node.example/lane-c[no-current-process]") || strings.Contains(coverage.Observed, "lane-a") {
		t.Fatalf("generation uncertainty erased or duplicated a known positive/coverage gap: %+v", alerts)
	}
}

// A complete new target supersedes old faults and ties without an RSS dependency.
func TestProxyDeviceTargetGenerationValidReplacementWithoutRss(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	signal := proxyDeviceGenerationTestSignal()
	for _, reverse := range []bool{false, true} {
		old := proxyGenerationTestProcess{instance: "zz-old", block: "lane-a", start: float64(now.Add(-time.Hour).Unix()), changes: signal.positive}
		oldTie := old
		oldTie.instance = "yy-old-tie"
		current := proxyGenerationTestProcess{instance: "aa-new", block: "lane-a", start: float64(now.Add(-time.Minute).Unix())}
		alerts := runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, reverse, old, oldTie, current)
		if len(alerts) != 0 {
			t.Fatalf("reverse=%v: a complete no-RSS target inherited old authority: %+v", reverse, alerts)
		}
		old.changes = nil
		current.changes = signal.positive
		alerts = runProxyGenerationTestSignal(t, signal, now, []string{"lane-a"}, reverse, old, current)
		alert := requireAlertClass(t, alerts, "proxy-device-target-mismatch")
		if len(alerts) != 1 || !strings.Contains(alert.Observed, "devices=2 target=33554432") {
			t.Fatalf("reverse=%v: a known newest target mismatch was hidden: %+v", reverse, alerts)
		}
	}
}

// Device-target's fixed query has four instantaneous families and no RSS/rates.
func proxyDeviceGenerationTestSignal() proxyGenerationTestSignal {
	return proxyGenerationTestSignal{
		key: "proxy-device-target", number: "14.7d", constructor: NewProxyDeviceTargetSignal,
		sourceTimes: true, withoutRss: true,
		values: map[string]float64{
			"urnetwork_proxy_devices_live":                     2,
			"urnetwork_proxy_device_memory_target_bytes":       64 * 1024 * 1024,
			"urnetwork_proxy_device_memory_tracked_used_bytes": 1024,
		},
		positive:      map[string]float64{"urnetwork_proxy_device_memory_target_bytes": 32 * 1024 * 1024},
		positiveClass: "proxy-device-target-mismatch",
	}
}
