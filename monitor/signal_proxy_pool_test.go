package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type proxyPoolFixtureProcess struct {
	host        string
	block       string
	instance    string
	rss         float64
	capacity    *float64
	retained    *float64
	packet      *float64
	large       *float64
	outstanding *float64
	startTime   time.Time
	sampleTime  time.Time
	poolTime    time.Time
}

func TestProxyPoolSignalSyntheticMissingCollector(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	payload := proxyPoolFixtureJSON(t, now,
		proxyPoolFixtureProcess{host: "fireside", block: "g1", instance: "proxy-a", rss: 5 << 30},
		proxyPoolFixtureProcess{host: "fireside", block: "g2", instance: "proxy-b", rss: 5 << 30},
	)
	alerts := runProxyPoolFixture(t, now, payload)
	alert := requireAlertClass(t, alerts, "proxy-message-pool-unobservable")
	if alert.Severity != SeverityWarn || alert.SignalNumber != "14.7a" || alert.SignalKey != "proxy-pool" {
		t.Fatalf("wrong missing-collector identity: %+v", alert)
	}
	for _, want := range []string{
		"2 of 2 newest fresh proxy identities",
		"missing_identities=2",
		"fireside/g1#proxy-a[capacity,retained,packet-retained,large-retained,outstanding]",
		"actual scrape timestamp",
		"not a live process-overlap measurement",
		"controller, which proxy does not import",
		"Do not infer a pool leak",
		"two-argument ResizeMessagePools",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing-collector alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestProxyPoolSignalSyntheticRolloutSelectsNewestFreshGeneration(t *testing.T) {
	now := time.Date(2026, 9, 1, 2, 34, 50, 0, time.UTC)
	capacity := float64((8 << 30) - (8 << 10))
	retained := float64(4 << 30)
	packet := float64(1 << 30)
	large := retained - packet
	outstanding := 48.0
	complete := func(host string, block string, instance string, start time.Time) proxyPoolFixtureProcess {
		return proxyPoolFixtureProcess{
			host: host, block: block, instance: instance, rss: 5 << 30, startTime: start,
			capacity: &capacity, retained: &retained, packet: &packet, large: &large, outstanding: &outstanding,
		}
	}
	payload := proxyPoolFixtureJSON(t, now,
		proxyPoolFixtureProcess{host: "crisp", block: "g1", instance: "old-g1", rss: 5 << 30, startTime: now.Add(-2 * time.Hour)},
		complete("crisp", "g1", "new-g1", now.Add(-time.Minute)),
		complete("crisp", "g2", "old-g2", now.Add(-2*time.Hour)),
		proxyPoolFixtureProcess{host: "crisp", block: "g2", instance: "new-g2", rss: 5 << 30, startTime: now.Add(-time.Minute)},
	)
	alerts := runProxyPoolFixture(t, now, payload)
	alert := requireAlertClass(t, alerts, "proxy-message-pool-unobservable")
	for _, want := range []string{
		"1 of 2 newest fresh proxy identities",
		"current_proxy_identities=2",
		"missing_identities=1",
		"crisp/g2#new-g2[capacity,retained,packet-retained,large-retained,outstanding]",
		"newest start time suppresses draining generations",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("rollout collector alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	for _, omitted := range []string{"old-g1", "old-g2", "new-g1"} {
		if strings.Contains(alert.Observed, omitted) {
			t.Fatalf("rollout collector alert retained non-missing generation %q: %s", omitted, alert.Observed)
		}
	}
}

func TestProxyPoolSignalSyntheticLegacyTwentyFourGiBCap(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 1, 0, 0, time.UTC)
	capacity := float64(24 << 30)
	retained := float64(5 << 30)
	packet := float64(2 << 30)
	large := retained - packet
	outstanding := 125.0
	payload := proxyPoolFixtureJSON(t, now, proxyPoolFixtureProcess{
		host: "fireside", block: "g1", instance: "legacy", rss: 5 << 30,
		capacity: &capacity, retained: &retained, packet: &packet, large: &large, outstanding: &outstanding,
	})
	alerts := runProxyPoolFixture(t, now, payload)
	alert := requireAlertClass(t, alerts, "proxy-message-pool-capacity")
	for _, want := range []string{
		"24 GiB process-wide",
		"capacity_bytes=25769803776",
		"limit_bytes=8589934592",
		"one third of 8 GiB",
		"does not by itself prove",
		"additional hardware",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("legacy-cap alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestProxyPoolSignalSyntheticFixedCapHealthy(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 2, 0, 0, time.UTC)
	capacity := float64((8 << 30) - (8 << 10))
	retained := float64(4 << 30)
	packet := float64(1 << 30)
	large := retained - packet
	outstanding := 48.0
	payload := proxyPoolFixtureJSON(t, now, proxyPoolFixtureProcess{
		host: "crisp", block: "g4", instance: "fixed", rss: 5 << 30,
		capacity: &capacity, retained: &retained, packet: &packet, large: &large, outstanding: &outstanding,
	})
	alerts := runProxyPoolFixture(t, now, payload)
	if len(alerts) != 0 {
		t.Fatalf("fixed proxy pool alerted: %+v", alerts)
	}
}

func TestProxyPoolSignalSyntheticStaleAndInconsistentMetrics(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 3, 0, 0, time.UTC)
	capacity := float64(8 << 30)
	retained := float64(5 << 30)
	packet := float64(1 << 30)
	large := float64(2 << 30)
	outstanding := 4.0
	payload := proxyPoolFixtureJSON(t, now,
		proxyPoolFixtureProcess{
			host: "crisp", block: "g1", instance: "stale", rss: 5 << 30,
			capacity: &capacity, retained: &retained, packet: &packet, large: &large, outstanding: &outstanding,
			poolTime: now.Add(-2 * time.Minute),
		},
		proxyPoolFixtureProcess{
			host: "crisp", block: "g2", instance: "invalid", rss: 5 << 30,
			capacity: &capacity, retained: &retained, packet: &packet, large: &large, outstanding: &outstanding,
		},
	)
	alerts := runProxyPoolFixture(t, now, payload)
	if missing := requireAlertClass(t, alerts, "proxy-message-pool-unobservable"); !strings.Contains(missing.Observed, "crisp/g1#stale") {
		t.Fatalf("stale process not classified as missing: %+v", missing)
	}
	if invalid := requireAlertClass(t, alerts, "proxy-message-pool-metrics-invalid"); !strings.Contains(invalid.Observed, "crisp/g2#invalid") {
		t.Fatalf("inconsistent process not classified: %+v", invalid)
	}
}

func runProxyPoolFixture(t testing.TB, now time.Time, payload string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" || !strings.Contains(command, "message_pool_capacity_bytes") ||
			!strings.Contains(command, "%22synthetic%22") ||
			!strings.Contains(command, "timestamp%28label_replace") ||
			!strings.Contains(command, "monitor_metric") {
			return "", fmt.Errorf("unexpected Mimir command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := NewProxyPoolSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func proxyPoolFixtureJSON(t testing.TB, now time.Time, processes ...proxyPoolFixtureProcess) string {
	t.Helper()
	result := []map[string]any{}
	for _, process := range processes {
		labels := map[string]string{
			"host": process.host, "block": process.block, "instance": process.instance,
		}
		add := func(name string, value float64, observedAt time.Time) {
			metric := map[string]string{"__name__": name}
			for key, label := range labels {
				metric[key] = label
			}
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(observedAt.Unix()), fmt.Sprintf("%.0f", value)},
			})
		}
		sampleTime := process.sampleTime
		if sampleTime.IsZero() {
			sampleTime = now
		}
		startTime := process.startTime
		if startTime.IsZero() {
			startTime = now.Add(-time.Hour)
		}
		add("process_resident_memory_bytes", process.rss, sampleTime)
		add("process_start_time_seconds", float64(startTime.Unix()), sampleTime)
		poolTime := process.poolTime
		if poolTime.IsZero() {
			poolTime = sampleTime
		}
		for _, metric := range []struct {
			name  string
			value *float64
		}{
			{"urnetwork_message_pool_capacity_bytes", process.capacity},
			{"urnetwork_message_pool_retained_bytes", process.retained},
			{"urnetwork_message_pool_packet_retained_bytes", process.packet},
			{"urnetwork_message_pool_large_object_retained_bytes", process.large},
			{"urnetwork_message_pool_outstanding", process.outstanding},
		} {
			if metric.value != nil {
				add(metric.name, *metric.value, poolTime)
			}
		}
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
