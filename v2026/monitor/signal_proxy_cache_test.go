package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type proxyCacheFixtureProcess struct {
	host        string
	block       string
	instance    string
	sampleTime  time.Time
	startTime   time.Time
	rss         float64
	entries     *float64
	capacity    *float64
	hits        *float64
	misses      *float64
	expirations *float64
	evictions   *float64
}

func TestProxyCacheSignalSyntheticLegacyMapUnobservable(t *testing.T) {
	now := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	process := proxyCacheFixture("fireside", "g1", "legacy", now.Add(-time.Hour))
	process.entries = nil
	process.capacity = nil
	process.evictions = nil

	alerts := runProxyCacheFixture(t, now, proxyCacheFixtureJSON(t, now, process))
	alert := requireAlertClass(t, alerts, "proxy-lock-cache-unobservable")
	if alert.SignalNumber != "14.7c" || alert.SignalKey != "proxy-cache" || alert.Sustain != 1 {
		t.Fatalf("wrong proxy-cache visibility identity: %+v", alert)
	}
	for _, want := range []string{
		"fireside/g1#legacy[entries,capacity,evictions]",
		"30-second value TTL without deleting expired keys",
		"cannot prove the lifetime-retention path is closed",
		"current-main commit a11ae7b1",
		"additional active-client slots",
		"SIGNALS.md §14.7c",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("proxy-cache visibility alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestProxyCacheSignalSyntheticInvalidBound(t *testing.T) {
	now := time.Date(2026, 9, 1, 13, 1, 0, 0, time.UTC)
	process := proxyCacheFixture("fireside", "g3", "bad-bound", now.Add(-time.Hour))
	setProxyCacheFixtureValues(&process, 20_001, 20_000, 100, 40, 20, 1)

	alerts := runProxyCacheFixture(t, now, proxyCacheFixtureJSON(t, now, process))
	alert := requireAlertClass(t, alerts, "proxy-lock-cache-bound")
	for _, want := range []string{
		"1 of 1 newest fresh proxy identities violate",
		"entries=20001 capacity=20000",
		"maximum_capacity=16384",
		"deterministic unique-ID churn",
		"do not raise the capacity",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("proxy-cache bound alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestProxyCacheSignalSyntheticSustainedPressure(t *testing.T) {
	now := time.Date(2026, 9, 1, 13, 2, 0, 0, time.UTC)
	process := proxyCacheFixture("crisp", "g4", "pressure", now.Add(-time.Hour))
	setProxyCacheFixtureValues(&process, 15_000, 16_384, 50_000, 20_000, 10_000, 3_616)

	alerts := runProxyCacheFixture(t, now, proxyCacheFixtureJSON(t, now, process))
	alert := requireAlertClass(t, alerts, "proxy-lock-cache-pressure")
	if alert.Sustain != 5 {
		t.Fatalf("proxy-cache pressure sustain=%d, want 5", alert.Sustain)
	}
	for _, want := range []string{
		"at least 90%",
		"entries=15000 capacity=16384 occupancy=0.916",
		"evictions=3616",
		"Rate-limit or block abusive invalid-token sources operationally",
		"additional capable Proxy hardware",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("proxy-cache pressure alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestProxyCacheSignalSyntheticNewestGenerationHealthy(t *testing.T) {
	now := time.Date(2026, 9, 1, 13, 3, 0, 0, time.UTC)
	old := proxyCacheFixture("crisp", "g2", "old", now.Add(-2*time.Hour))
	setProxyCacheFixtureValues(&old, 40_000, 0, 100, 40, 20, 0)
	current := proxyCacheFixture("crisp", "g2", "current", now.Add(-time.Minute))
	setProxyCacheFixtureValues(&current, 52, 16_384, 2_000, 70, 60, 0)

	alerts := runProxyCacheFixture(t, now, proxyCacheFixtureJSON(t, now, old, current))
	if len(alerts) != 0 {
		t.Fatalf("newest bounded cache sample alerted: %+v", alerts)
	}
}

func proxyCacheFixture(host, block, instance string, start time.Time) proxyCacheFixtureProcess {
	zero := float64(0)
	return proxyCacheFixtureProcess{
		host: host, block: block, instance: instance, startTime: start,
		rss: 5 << 30, entries: &zero, capacity: &zero, hits: &zero,
		misses: &zero, expirations: &zero, evictions: &zero,
	}
}

func setProxyCacheFixtureValues(
	process *proxyCacheFixtureProcess,
	entries, capacity, hits, misses, expirations, evictions float64,
) {
	process.entries = &entries
	process.capacity = &capacity
	process.hits = &hits
	process.misses = &misses
	process.expirations = &expirations
	process.evictions = &evictions
}

func runProxyCacheFixture(t testing.TB, now time.Time, payload string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" ||
			!strings.Contains(command, "urnetwork_proxy_lock_cache_entries") ||
			!strings.Contains(command, "urnetwork_proxy_lock_cache_evictions_total") ||
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
	alerts, err := NewProxyCacheSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func proxyCacheFixtureJSON(
	t testing.TB,
	now time.Time,
	processes ...proxyCacheFixtureProcess,
) string {
	t.Helper()
	result := []map[string]any{}
	for _, process := range processes {
		labels := map[string]string{
			"host": process.host, "block": process.block, "instance": process.instance,
		}
		add := func(name string, value float64) {
			metric := map[string]string{"__name__": name}
			for key, label := range labels {
				metric[key] = label
			}
			sampleTime := process.sampleTime
			if sampleTime.IsZero() {
				sampleTime = now
			}
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(sampleTime.Unix()), fmt.Sprintf("%.0f", value)},
			})
		}
		add("process_resident_memory_bytes", process.rss)
		add("process_start_time_seconds", float64(process.startTime.Unix()))
		for _, metric := range []struct {
			name  string
			value *float64
		}{
			{"urnetwork_proxy_lock_cache_entries", process.entries},
			{"urnetwork_proxy_lock_cache_capacity", process.capacity},
			{"urnetwork_proxy_lock_cache_hits_total", process.hits},
			{"urnetwork_proxy_lock_cache_misses_total", process.misses},
			{"urnetwork_proxy_lock_cache_expirations_total", process.expirations},
			{"urnetwork_proxy_lock_cache_evictions_total", process.evictions},
		} {
			if metric.value != nil {
				add(metric.name, *metric.value)
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
