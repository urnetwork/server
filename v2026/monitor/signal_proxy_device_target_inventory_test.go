// A placement join is authoritative before the empty-fleet shortcut.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Build a public signal input with independently supplied placement intent and
// joined inventory. Every permitted query returns one synthetic current slot.
func proxyDeviceTargetInventoryTestSettings(t *testing.T, expected int, joined bool, devices float64, target float64) (SignalSettings, *int) {
	t.Helper()
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	values := map[string]float64{
		"process_start_time_seconds":                       float64(now.Add(-time.Hour).Unix()),
		"urnetwork_proxy_devices_live":                     devices,
		"urnetwork_proxy_device_memory_target_bytes":       target,
		"urnetwork_proxy_device_memory_tracked_used_bytes": 0,
	}
	rows := []map[string]any{}
	for metric, value := range values {
		for _, sample := range []struct {
			name  string
			value float64
		}{
			{name: metric, value: value},
			{name: metric + "__source_timestamp", value: float64(now.Unix())},
		} {
			rows = append(rows, map[string]any{
				"metric": map[string]string{
					"host": "proxy.example.test", "block": "lane-a", "instance": "synthetic-generation",
					"monitor_metric": sample.name, "unrelated_label": "synthetic-private-inventory",
				},
				"value": []any{float64(now.Unix()), fmt.Sprint(sample.value)},
			})
		}
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	settings := syntheticSettings(&syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		calls++
		if host.Name != "metrics.example.test" || !strings.Contains(command, proxyDeviceTargetMarker) {
			return "", errors.New("unexpected synthetic inventory source contact")
		}
		return string(payload), nil
	}})
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{{Name: "metrics.example.test", Roles: []string{"services"}}}
	if joined {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "proxy.example.test", Proxy: &ProxyHostSettings{}})
	}
	settings.LogServices = []string{"proxy"}
	settings.LogServiceBlocks = map[string][]string{"proxy": {"lane-a"}}
	settings.ProxyPathExpectedHosts = expected
	return settings, &calls
}

// A complete services placement count must not collapse to a healthy empty
// result merely because every Proxy host failed the monitor-inventory join.
func TestProxyDeviceTargetInventoryZeroJoinRemainsUnknown(t *testing.T) {
	settings, calls := proxyDeviceTargetInventoryTestSettings(t, 1, false, 0, 0)
	alerts, err := NewProxyDeviceTargetSignal().Run(context.Background(), settings)
	if err != nil || *calls != 0 || len(alerts) != 1 {
		t.Fatal("positive expected placement with zero joined Proxy rows became quiet or contacted a source")
	}
	alert := requireAlertClass(t, alerts, "proxy-device-target-unobservable")
	if alert.Identity() != "runtime/proxy-device-target|proxy-device-target-unobservable|proxy-fleet|inventory" ||
		alert.SignalNumber != "14.7d" || alert.SignalKey != "proxy-device-target" || alert.Severity != SeverityWarn || alert.Sustain != 1 || !alert.unobservable {
		t.Fatal("zero-join visibility lost its owning identity or unavailable authority")
	}
	if alert.Observed != "hosts=0 blocks=1 expected_hosts=1" {
		t.Fatal("zero-join diagnostic lost the independent expected count")
	}
	encoded, encodeErr := json.Marshal(alerts)
	if encodeErr != nil || strings.Contains(string(encoded), "synthetic-private-inventory") || strings.Contains(alerts.Markdown(), "synthetic-private-inventory") {
		t.Fatal("inventory visibility rendered an unrelated source label")
	}
}

// No active placement is deliberately inapplicable, even without service
// blocks or a gateway. It must remain quiet and make no source call.
func TestProxyDeviceTargetInventoryTrueZeroPlacementsStayQuiet(t *testing.T) {
	settings, calls := proxyDeviceTargetInventoryTestSettings(t, 0, false, 0, 0)
	settings.LogServices, settings.LogServiceBlocks, settings.Hosts = nil, nil, nil
	alerts, err := NewProxyDeviceTargetSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || *calls != 0 {
		t.Fatal("true zero-placement applicability queried telemetry or alerted")
	}
}

// Nonzero partial joins and missing blocks retain their pre-query inventory
// finding. A missing gateway does not turn unjoined placements into health.
func TestProxyDeviceTargetInventoryAdjacentCoverageStaysUnknownWithoutQuery(t *testing.T) {
	for _, test := range []struct {
		name          string
		expected      int
		joined        bool
		missingBlocks bool
		missingSource bool
	}{
		{name: "partial join", expected: 2, joined: true},
		{name: "zero join no blocks", expected: 1, missingBlocks: true},
		{name: "joined no blocks", expected: 1, joined: true, missingBlocks: true},
		{name: "zero join no gateway", expected: 1, missingSource: true},
	} {
		settings, calls := proxyDeviceTargetInventoryTestSettings(t, test.expected, test.joined, 1, 32*1024*1024)
		if test.missingBlocks {
			settings.LogServiceBlocks = nil
		}
		if test.missingSource {
			settings.Hosts = nil
		}
		alerts, err := NewProxyDeviceTargetSignal().Run(context.Background(), settings)
		if err != nil || *calls != 0 || len(alerts) != 1 {
			t.Fatalf("%s: incomplete inventory became healthy or queried a source", test.name)
		}
		if alert := requireAlertClass(t, alerts, "proxy-device-target-unobservable"); alert.Frame != "inventory" || !alert.unobservable {
			t.Fatalf("%s: incomplete join lost its inventory authority", test.name)
		}
	}
}

// Current zero-device telemetry is measured health, unlike a zero placement
// join. A complete nonempty slot also preserves the ordinary private target.
func TestProxyDeviceTargetInventoryCompleteSlotsRemainHealthy(t *testing.T) {
	for _, devices := range []float64{0, 1} {
		settings, calls := proxyDeviceTargetInventoryTestSettings(t, 1, true, devices, devices*32*1024*1024)
		alerts, err := NewProxyDeviceTargetSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 || *calls != 1 {
			t.Fatal("complete current slot lost its measured empty/nonempty healthy control")
		}
	}
}

// The loader retains derived disabled Proxy settings separately from active
// hosts. An entirely known pause stays inapplicable; a disabled subset cannot
// account for an additional placement missing from both inventories.
func TestProxyDeviceTargetInventoryKnownDisabledScopeDoesNotMaskMissingPlacement(t *testing.T) {
	for _, expected := range []int{1, 2} {
		settings, calls := proxyDeviceTargetInventoryTestSettings(t, expected, false, 0, 0)
		settings.disabledHosts = []HostSettings{{Name: "paused.example.test", Proxy: &ProxyHostSettings{}}}
		alerts, err := NewProxyDeviceTargetSignal().Run(context.Background(), settings)
		if err != nil || *calls != 0 {
			t.Fatal("known disabled placement caused source contact or an execution failure")
		}
		if expected == 1 {
			if len(alerts) != 0 {
				t.Fatal("entirely known disabled inventory became a failed placement join")
			}
		} else if len(alerts) != 1 || requireAlertClass(t, alerts, "proxy-device-target-unobservable").Frame != "inventory" {
			t.Fatal("one known disabled placement hid an additional unjoined placement")
		}
	}
}

// Exclusions preserve derived topology. Whole-environment Mimir can remain
// readable through a permitted gateway; a paused gateway is denied before
// synthetic or production contact and reports the existing scope visibility.
func TestProxyDeviceTargetInventoryExclusionsKeepTopologyAndDenyContact(t *testing.T) {
	for _, excluded := range []string{"proxy.example.test", "metrics.example.test"} {
		settings, calls := proxyDeviceTargetInventoryTestSettings(t, 1, true, 1, 32*1024*1024)
		scoped, err := ExcludeHosts(settings, excluded)
		if err != nil {
			t.Fatal("synthetic exclusion failed")
		}
		alerts, err := NewProxyDeviceTargetSignal().Run(context.Background(), scoped)
		if err != nil || len(scoped.Hosts) != 2 || scoped.ProxyPathExpectedHosts != 1 || scoped.Hosts[1].Proxy == nil {
			t.Fatal("exclusion changed desired inventory or escaped its scope reduction")
		}
		if excluded == "proxy.example.test" {
			if len(alerts) != 0 || *calls != 1 {
				t.Fatal("permitted aggregate read was lost or excluded Proxy was contacted")
			}
		} else if len(alerts) != 1 || *calls != 0 || !requireAlertClass(t, alerts, "monitor-host-scope-partial").unobservable {
			t.Fatal("paused gateway was contacted or its scope became an inventory fault")
		}
	}
}

// Public inventory visibility must not clear an existing target mismatch.
// Only two later complete current-slot observations supply recovery credit.
func TestProxyDeviceTargetInventoryZeroJoinCannotClearTargetIncident(t *testing.T) {
	signal := NewProxyDeviceTargetSignal()
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{
		settings:              DefaultSlackAlertNotifierSettings(),
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	key := ""
	for run := 0; run < 5; run++ {
		joined, target := true, float64(32*1024*1024)
		if run == 0 {
			target = 0
		} else if run == 1 || run == 2 {
			joined = false
		}
		settings, calls := proxyDeviceTargetInventoryTestSettings(t, 1, joined, 1, target)
		alerts, err := signal.Run(context.Background(), settings)
		wantCalls := 0
		if joined {
			wantCalls = 1
		}
		if err != nil || *calls != wantCalls {
			t.Fatal("public inventory sequence lost its independent query boundary")
		}
		if run == 0 {
			key = slackIncidentKey(requireAlertClass(t, alerts, "proxy-device-target-mismatch"))
		}
		ready, held := gate.split(signal, alerts)
		notifier.stateLock.Lock()
		notifier.observeWithLock(settings.Now(), signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		incident := notifier.keyIncidents[key]
		present := incident != nil
		absent := -1
		if present {
			absent = incident.signalKeyAbsentRuns[signal.Key()]
		}
		notifier.stateLock.Unlock()
		if present != (run < 4) {
			t.Fatalf("run %d: zero join cleared the target incident or measured recovery failed", run)
		}
		if run == 2 && absent != 0 || run == 3 && absent != 1 {
			t.Fatal("unqualified inventory granted absence credit or qualified recovery lost it")
		}
	}
}
