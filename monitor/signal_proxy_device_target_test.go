package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type proxyDeviceTargetFixture struct {
	devices, target, used float64
	legacyBudget          float64
	omit                  string
	stale                 bool
}

func TestProxyDeviceTargetSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	alerts := runProxyDeviceTargetFixture(t, now, proxyDeviceTargetFixture{devices: 2, target: 2 * proxyDeviceTargetBytes, used: proxyDeviceTargetBytes})
	if len(alerts) != 0 {
		t.Fatalf("two independent device targets alerted: %+v", alerts)
	}
}

func TestProxyDeviceTargetSignalSyntheticEmptyProcessIsHealthy(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	alerts := runProxyDeviceTargetFixture(t, now, proxyDeviceTargetFixture{})
	if len(alerts) != 0 {
		t.Fatalf("empty current process has no shared admission ceiling: %+v", alerts)
	}
}

func TestProxyDeviceTargetSignalSyntheticMissingPrivateTarget(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	alerts := runProxyDeviceTargetFixture(t, now, proxyDeviceTargetFixture{devices: 2, target: proxyDeviceTargetBytes, used: 1})
	alert := requireAlertClass(t, alerts, "proxy-device-target-mismatch")
	if alert.SignalNumber != "14.7d" || !strings.Contains(alert.Markdown(), "24 MiB") {
		t.Fatalf("wrong private-target alert: %+v", alert)
	}
}

func TestProxyDeviceTargetSignalSyntheticMissingMetric(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	alerts := runProxyDeviceTargetFixture(t, now, proxyDeviceTargetFixture{devices: 1, target: proxyDeviceTargetBytes, used: 1, omit: "urnetwork_proxy_device_memory_target_bytes"})
	requireAlertClass(t, alerts, "proxy-device-target-unobservable")
}

func TestProxyDeviceTargetSignalSyntheticStaleMetric(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	alerts := runProxyDeviceTargetFixture(t, now, proxyDeviceTargetFixture{devices: 1, target: proxyDeviceTargetBytes, used: 1, stale: true})
	requireAlertClass(t, alerts, "proxy-device-target-unobservable")
}

func TestProxyDeviceTargetSignalSyntheticOverTarget(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	alerts := runProxyDeviceTargetFixture(t, now, proxyDeviceTargetFixture{devices: 1, target: proxyDeviceTargetBytes, used: proxyDeviceTargetBytes + 1})
	alert := requireAlertClass(t, alerts, "proxy-device-target-pressure")
	if !strings.Contains(alert.Markdown(), "not total RSS") {
		t.Fatal("pressure alert confuses device target with physical memory")
	}
}

func TestProxyDeviceTargetSignalSyntheticLegacySharedAdmission(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	alerts := runProxyDeviceTargetFixture(t, now, proxyDeviceTargetFixture{
		devices: 2, target: 2 * proxyDeviceTargetBytes, used: proxyDeviceTargetBytes,
		legacyBudget: 24 * 1024 * 1024,
	})
	alert := requireAlertClass(t, alerts, "proxy-device-shared-admission")
	if !strings.Contains(alert.Markdown(), "shared device admission budget") {
		t.Fatalf("legacy aggregate gate was not explained: %+v", alert)
	}
}

func TestProxyDeviceTargetSignalUsesNewestGeneration(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 0, 0, 0, time.UTC)
	newProcess := func(instance string, started time.Time, target float64) *proxyDeviceTargetProcess {
		values := map[string]float64{
			"process_start_time_seconds":                       float64(started.Unix()),
			"urnetwork_proxy_devices_live":                     1,
			"urnetwork_proxy_device_memory_target_bytes":       target,
			"urnetwork_proxy_device_memory_tracked_used_bytes": 1,
		}
		sources := map[string]time.Time{}
		for _, name := range proxyDeviceTargetMetrics {
			sources[name] = now
		}
		return &proxyDeviceTargetProcess{host: "proxy.example.test", block: "lane-a", instance: instance, values: values, sources: sources}
	}
	expected := map[string]string{"proxy.example.test\x00lane-a": "proxy.example.test/lane-a"}
	old := newProcess("old", now.Add(-2*time.Hour), 0)
	old.values[proxyDeviceLegacyBudget] = proxyDeviceTargetBytes
	old.sources[proxyDeviceLegacyBudget] = now
	current := newProcess("current", now.Add(-time.Hour), proxyDeviceTargetBytes)
	findings := assessProxyDeviceTargets(map[string]*proxyDeviceTargetProcess{"old": old, "current": current}, expected, now)
	if len(findings) != 1 || findings[0].class != "proxy-device-target-mismatch" || !findings[0].healthy {
		// healthyFinding is a non-alert sentinel with the monitored class.
		t.Fatalf("healthy newest generation was not selected: %+v", findings)
	}
	delete(current.sources, "urnetwork_proxy_device_memory_target_bytes")
	findings = assessProxyDeviceTargets(map[string]*proxyDeviceTargetProcess{"old": old, "current": current}, expected, now)
	if len(findings) != 1 || findings[0].class != "proxy-device-target-unobservable" {
		t.Fatalf("old complete generation hid current missing metric: %+v", findings)
	}
}

func runProxyDeviceTargetFixture(t *testing.T, now time.Time, fixture proxyDeviceTargetFixture) Alerts {
	t.Helper()
	values := map[string]float64{
		"process_start_time_seconds":                       float64(now.Add(-time.Hour).Unix()),
		"urnetwork_proxy_devices_live":                     fixture.devices,
		"urnetwork_proxy_device_memory_target_bytes":       fixture.target,
		"urnetwork_proxy_device_memory_tracked_used_bytes": fixture.used,
	}
	result := []map[string]any{}
	for _, metric := range proxyDeviceTargetMetrics {
		if metric == fixture.omit {
			continue
		}
		sourceTime := now
		if fixture.stale && metric == "urnetwork_proxy_device_memory_target_bytes" {
			sourceTime = now.Add(-2 * time.Minute)
		}
		for _, sample := range []struct {
			name  string
			value float64
		}{{metric, values[metric]}, {metric + proxyDeviceTargetSource, float64(sourceTime.Unix())}} {
			result = append(result, map[string]any{
				"metric": map[string]string{
					"host": "proxy.example.test", "block": "lane-a", "instance": "generation-a",
					"monitor_metric": sample.name,
				},
				"value": []any{float64(now.Unix()), fmt.Sprint(sample.value)},
			})
		}
	}
	if fixture.legacyBudget > 0 {
		for _, sample := range []struct {
			name  string
			value float64
		}{{proxyDeviceLegacyBudget, fixture.legacyBudget}, {proxyDeviceLegacyBudget + proxyDeviceTargetSource, float64(now.Unix())}} {
			result = append(result, map[string]any{
				"metric": map[string]string{
					"host": "proxy.example.test", "block": "lane-a", "instance": "generation-a",
					"monitor_metric": sample.name,
				},
				"value": []any{float64(now.Unix()), fmt.Sprint(sample.value)},
			})
		}
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics.example.test" || !strings.Contains(command, proxyDeviceTargetMarker) ||
			!strings.Contains(command, "urnetwork_proxy_device_memory_target_bytes") {
			return "", fmt.Errorf("unexpected Mimir command on %s", host.Name)
		}
		return string(payload), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.LogServices = []string{"proxy"}
	settings.LogServiceBlocks = map[string][]string{"proxy": {"lane-a"}}
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "proxy.example.test", Proxy: &ProxyHostSettings{}},
		HostSettings{Name: "metrics.example.test", Roles: []string{"services"}},
	)
	alerts, err := NewProxyDeviceTargetSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}
