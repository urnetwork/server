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
	host               string
	block              string
	instance           string
	rss                float64
	capacity           *float64
	retained           *float64
	packet             *float64
	large              *float64
	outstanding        *float64
	startTime          time.Time
	sampleTime         time.Time
	poolTime           time.Time
	retainedTime       time.Time
	omitRetainedSource bool
}

func TestProxyPoolSignalSyntheticMissingCollector(t *testing.T) {
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	payload := proxyPoolFixtureJSON(t, now,
		proxyPoolFixtureProcess{host: "host-a.example", block: "g1", instance: "proxy-a", rss: 5 << 30},
		proxyPoolFixtureProcess{host: "host-a.example", block: "g2", instance: "proxy-b", rss: 5 << 30},
	)
	alerts := runProxyPoolFixture(t, now, payload)
	alert := requireAlertClass(t, alerts, "proxy-message-pool-unobservable")
	if alert.Severity != SeverityWarn || alert.SignalNumber != "14.7a" || alert.SignalKey != "proxy-pool" {
		t.Fatalf("wrong missing-collector identity: %+v", alert)
	}
	for _, want := range []string{
		"2 of 2 newest fresh proxy identities",
		"missing_identities=2",
		"host-a.example/g1#proxy-a[capacity,retained,packet-retained,large-retained,outstanding]",
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
		proxyPoolFixtureProcess{host: "host-b.example", block: "g1", instance: "old-g1", rss: 5 << 30, startTime: now.Add(-2 * time.Hour)},
		complete("host-b.example", "g1", "new-g1", now.Add(-time.Minute)),
		complete("host-b.example", "g2", "old-g2", now.Add(-2*time.Hour)),
		proxyPoolFixtureProcess{host: "host-b.example", block: "g2", instance: "new-g2", rss: 5 << 30, startTime: now.Add(-time.Minute)},
	)
	alerts := runProxyPoolFixture(t, now, payload)
	alert := requireAlertClass(t, alerts, "proxy-message-pool-unobservable")
	for _, want := range []string{
		"1 of 2 newest fresh proxy identities",
		"current_proxy_identities=2",
		"missing_identities=1",
		"host-b.example/g2#new-g2[capacity,retained,packet-retained,large-retained,outstanding]",
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
		host: "host-a.example", block: "g1", instance: "legacy", rss: 5 << 30,
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
		host: "host-b.example", block: "g4", instance: "fixed", rss: 5 << 30,
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
			host: "host-b.example", block: "g1", instance: "stale", rss: 5 << 30,
			capacity: &capacity, retained: &retained, packet: &packet, large: &large, outstanding: &outstanding,
			poolTime: now.Add(-2 * time.Minute),
		},
		proxyPoolFixtureProcess{
			host: "host-b.example", block: "g2", instance: "invalid", rss: 5 << 30,
			capacity: &capacity, retained: &retained, packet: &packet, large: &large, outstanding: &outstanding,
		},
	)
	alerts := runProxyPoolFixture(t, now, payload)
	if missing := requireAlertClass(t, alerts, "proxy-message-pool-unobservable"); !strings.Contains(missing.Observed, "host-b.example/g1#stale") {
		t.Fatalf("stale process not classified as missing: %+v", missing)
	}
	if invalid := requireAlertClass(t, alerts, "proxy-message-pool-metrics-invalid"); !strings.Contains(invalid.Observed, "host-b.example/g2#invalid") {
		t.Fatalf("inconsistent process not classified: %+v", invalid)
	}
}

func TestProxyPoolSignalSyntheticMixedScrapesAreNotAccountingCorruption(t *testing.T) {
	now := time.Date(2026, 9, 10, 19, 32, 22, 0, time.UTC)
	capacity := float64(8 << 30)
	retained := float64(1515008)
	packet := float64(863744)
	large := float64(659456)
	outstanding := 9.0
	payload := proxyPoolFixtureJSON(t, now, proxyPoolFixtureProcess{
		host: "proxy-1.example", block: "g3", instance: "mixed-scrape", rss: 600 << 20,
		capacity: &capacity, retained: &retained, packet: &packet, large: &large, outstanding: &outstanding,
		retainedTime: now.Add(-15 * time.Second),
	})
	alerts := runProxyPoolFixture(t, now, payload)
	alert := requireAlertClass(t, alerts, "proxy-message-pool-snapshot-unobservable")
	for _, want := range []string{
		"same-scrape message-pool gauge set",
		"proxy-1.example/g3#mixed-scrape[source-time-skew]",
		"remote-write batch",
		"not an impossible GetMessagePoolAggregateStats result",
		"only a same-scrape",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("mixed-scrape alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	for _, candidate := range alerts {
		if candidate.Class == "proxy-message-pool-metrics-invalid" {
			t.Fatalf("mixed scrapes were misclassified as accounting corruption: %+v", candidate)
		}
	}
}

func TestProxyPoolSignalSyntheticMissingSourceTimeIsNotAccountingCorruption(t *testing.T) {
	now := time.Date(2026, 9, 10, 19, 33, 22, 0, time.UTC)
	capacity := float64(8 << 30)
	retained := float64(1515008)
	packet := float64(863744)
	large := float64(659456)
	outstanding := 9.0
	payload := proxyPoolFixtureJSON(t, now, proxyPoolFixtureProcess{
		host: "proxy-1.example", block: "g3", instance: "missing-source-time", rss: 600 << 20,
		capacity: &capacity, retained: &retained, packet: &packet, large: &large, outstanding: &outstanding,
		omitRetainedSource: true,
	})
	alerts := runProxyPoolFixture(t, now, payload)
	alert := requireAlertClass(t, alerts, "proxy-message-pool-snapshot-unobservable")
	if !strings.Contains(alert.Observed, "proxy-1.example/g3#missing-source-time[missing-source-times=retained]") {
		t.Fatalf("missing source time was not preserved as ambiguous: %+v", alert)
	}
	for _, candidate := range alerts {
		if candidate.Class == "proxy-message-pool-metrics-invalid" {
			t.Fatalf("missing source time was misclassified as accounting corruption: %+v", candidate)
		}
	}
}

func runProxyPoolFixture(t testing.TB, now time.Time, payload string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" || !strings.Contains(command, "message_pool_capacity_bytes") ||
			!strings.Contains(command, "%22synthetic%22") ||
			!strings.Contains(command, "label_replace%28timestamp%28urnetwork_message_pool") ||
			!strings.Contains(command, "monitor_metric") ||
			!strings.Contains(command, proxyPoolSourceSuffix) {
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
		addSourceTime := func(name string, sourceTime time.Time) {
			metric := map[string]string{"monitor_metric": name + proxyPoolSourceSuffix}
			for key, label := range labels {
				metric[key] = label
			}
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(now.Unix()), fmt.Sprintf("%.9f", float64(sourceTime.UnixNano())/1e9)},
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
			name       string
			value      *float64
			time       time.Time
			omitSource bool
		}{
			{"urnetwork_message_pool_capacity_bytes", process.capacity, poolTime, false},
			{"urnetwork_message_pool_retained_bytes", process.retained, process.retainedTime, process.omitRetainedSource},
			{"urnetwork_message_pool_packet_retained_bytes", process.packet, poolTime, false},
			{"urnetwork_message_pool_large_object_retained_bytes", process.large, poolTime, false},
			{"urnetwork_message_pool_outstanding", process.outstanding, poolTime, false},
		} {
			if metric.value != nil {
				metricTime := metric.time
				if metricTime.IsZero() {
					metricTime = poolTime
				}
				add(metric.name, *metric.value, metricTime)
				if !metric.omitSource {
					addSourceTime(metric.name, metricTime)
				}
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
