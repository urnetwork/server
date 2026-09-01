package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type proxyRuntimeFixtureProcess struct {
	host           string
	block          string
	instance       string
	version        string
	sourceRevision string
	sourceModified string
	imageDigest    string
	sampleTime     time.Time
	startTime      time.Time
	rss            float64
	heap           *float64
	objects        *float64
	goroutines     *float64
	peers          *float64
	devices        *float64
	deviceTracked  *float64
	poolRetained   *float64
	nextGC         *float64
	gogc           *float64
	stack          *float64
	lastGC         *float64
}

func TestProxyRuntimeSignalSyntheticHighLiveSet(t *testing.T) {
	now := time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC)
	process := completeProxyRuntimeFixture("fireside", "g1", "current", now.Add(-time.Hour))
	setProxyRuntimeFixtureValues(
		&process,
		float64(5<<30),
		float64(4<<30),
		30_000_000,
		28_200,
		12_800,
		25,
		40<<20,
		5<<20,
	)

	alerts := runProxyRuntimeFixture(t, now, proxyRuntimeFixtureJSON(t, now, process))
	alert := requireAlertClass(t, alerts, "proxy-runtime-live-set")
	if alert.SignalNumber != "14.7b" || alert.SignalKey != "proxy-runtime" || alert.Sustain != 2 {
		t.Fatalf("wrong runtime live-set identity: %+v", alert)
	}
	for _, want := range []string{
		"1 of 1 newest fresh proxy identities",
		`config_version="synthetic-proxy"`,
		`source_revision="dc40916a2c6c6e576d77f29aef8634fa45be5a8f"`,
		"source_modified=false",
		`image_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`,
		"heap_bytes=4294967296",
		"heap_objects=30000000",
		"wg_peers=12800",
		"peer_allowance_bytes=629145600",
		"next_gc_bytes=8589934592",
		"gogc_percent=100",
		"exactly two durable goroutines",
		"endpoint-seeded server-initiated handshaking peers",
		"shared NetworkSpace",
		"additional hard client slots",
		"SIGNALS.md §14.7b",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("runtime live-set alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestProxyRuntimeSignalSyntheticConservativeOwnerAccountingHealthy(t *testing.T) {
	now := time.Date(2026, 9, 1, 6, 1, 0, 0, time.UTC)
	process := completeProxyRuntimeFixture("crisp", "g4", "accounted", now.Add(-time.Hour))
	setProxyRuntimeFixtureValues(
		&process,
		float64(5<<30),
		float64(4<<30),
		30_000_000,
		82_000,
		40_000,
		40,
		1<<30,
		2<<30,
	)

	alerts := runProxyRuntimeFixture(t, now, proxyRuntimeFixtureJSON(t, now, process))
	if len(alerts) != 0 {
		t.Fatalf("owner-accounted runtime sample alerted: %+v", alerts)
	}
}

func TestProxyRuntimeSignalSyntheticNewestGenerationMissingMetric(t *testing.T) {
	now := time.Date(2026, 9, 1, 6, 2, 0, 0, time.UTC)
	old := completeProxyRuntimeFixture("crisp", "g2", "old", now.Add(-2*time.Hour))
	setProxyRuntimeFixtureValues(
		&old,
		float64(5<<30),
		float64(4<<30),
		30_000_000,
		28_000,
		12_500,
		24,
		32<<20,
		4<<20,
	)
	current := completeProxyRuntimeFixture("crisp", "g2", "new", now.Add(-time.Minute))
	setProxyRuntimeFixtureValues(
		&current,
		float64(2<<30),
		float64(1<<30),
		8_000_000,
		26_000,
		12_500,
		0,
		0,
		0,
	)
	current.goroutines = nil

	alerts := runProxyRuntimeFixture(t, now, proxyRuntimeFixtureJSON(t, now, old, current))
	alert := requireAlertClass(t, alerts, "proxy-runtime-unobservable")
	for _, want := range []string{
		"1 of 1 newest fresh proxy identities",
		"crisp/g2#new[goroutines]",
		"newest process start suppresses a draining generation",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("runtime visibility alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Observed, "#old") {
		t.Fatalf("runtime visibility alert retained old generation: %s", alert.Observed)
	}
}

func TestProxyRuntimeSignalSyntheticMissingSourceDoesNotHideHighLiveSet(t *testing.T) {
	now := time.Date(2026, 9, 1, 6, 3, 0, 0, time.UTC)
	process := completeProxyRuntimeFixture("fireside", "g5", "legacy", now.Add(-time.Hour))
	setProxyRuntimeFixtureValues(
		&process,
		float64(5<<30),
		float64(4<<30),
		30_000_000,
		28_000,
		12_800,
		0,
		0,
		0,
	)
	process.sourceRevision = ""
	process.sourceModified = ""
	process.imageDigest = ""

	alerts := runProxyRuntimeFixture(t, now, proxyRuntimeFixtureJSON(t, now, process))
	liveSet := requireAlertClass(t, alerts, "proxy-runtime-live-set")
	for _, want := range []string{
		`source_revision="unknown"`,
		"source_modified=unknown",
		"WARP_VERSION is only the config-generation boundary",
	} {
		if !strings.Contains(liveSet.Markdown(), want) {
			t.Fatalf("runtime live-set alert missing %q:\n%s", want, liveSet.Markdown())
		}
	}
	for _, candidate := range alerts {
		if candidate.Class == "proxy-runtime-unobservable" {
			t.Fatalf("optional source provenance was classified as missing memory attribution: %+v", alerts)
		}
	}
}

func completeProxyRuntimeFixture(host, block, instance string, start time.Time) proxyRuntimeFixtureProcess {
	zero := float64(0)
	gogc := float64(100)
	stack := float64(80 << 20)
	lastGC := float64(start.Add(30 * time.Minute).Unix())
	return proxyRuntimeFixtureProcess{
		host: host, block: block, instance: instance, version: "synthetic-proxy",
		sourceRevision: "dc40916a2c6c6e576d77f29aef8634fa45be5a8f",
		sourceModified: "false",
		imageDigest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		startTime:      start,
		heap:           &zero, objects: &zero, goroutines: &zero, peers: &zero,
		devices: &zero, deviceTracked: &zero, poolRetained: &zero,
		nextGC: &zero, gogc: &gogc, stack: &stack, lastGC: &lastGC,
	}
}

func setProxyRuntimeFixtureValues(
	process *proxyRuntimeFixtureProcess,
	rss, heap, objects, goroutines, peers, devices, deviceTracked, poolRetained float64,
) {
	process.rss = rss
	process.heap = &heap
	process.objects = &objects
	process.goroutines = &goroutines
	process.peers = &peers
	process.devices = &devices
	process.deviceTracked = &deviceTracked
	process.poolRetained = &poolRetained
	nextGC := 2 * heap
	process.nextGC = &nextGC
}

func runProxyRuntimeFixture(t testing.TB, now time.Time, payload string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" ||
			!strings.Contains(command, "go_memstats_heap_alloc_bytes") ||
			!strings.Contains(command, "urnetwork_proxy_devices_live") ||
			!strings.Contains(command, "urnetwork_source_info") ||
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
	alerts, err := NewProxyRuntimeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func proxyRuntimeFixtureJSON(
	t testing.TB,
	now time.Time,
	processes ...proxyRuntimeFixtureProcess,
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
		buildMetric := map[string]string{"__name__": "urnetwork_build_info", "version": process.version}
		for key, label := range labels {
			buildMetric[key] = label
		}
		sampleTime := process.sampleTime
		if sampleTime.IsZero() {
			sampleTime = now
		}
		result = append(result, map[string]any{
			"metric": buildMetric,
			"value":  []any{float64(sampleTime.Unix()), "1"},
		})
		if process.sourceRevision != "" {
			sourceMetric := map[string]string{
				"__name__":     "urnetwork_source_info",
				"revision":     process.sourceRevision,
				"modified":     process.sourceModified,
				"image_digest": process.imageDigest,
			}
			for key, label := range labels {
				sourceMetric[key] = label
			}
			result = append(result, map[string]any{
				"metric": sourceMetric,
				"value":  []any{float64(sampleTime.Unix()), "1"},
			})
		}
		for _, metric := range []struct {
			name  string
			value *float64
		}{
			{"go_memstats_heap_alloc_bytes", process.heap},
			{"go_memstats_heap_objects", process.objects},
			{"go_goroutines", process.goroutines},
			{"urnetwork_proxy_wg_peers", process.peers},
			{"urnetwork_proxy_devices_live", process.devices},
			{"urnetwork_proxy_device_memory_tracked_used_bytes", process.deviceTracked},
			{"urnetwork_message_pool_retained_bytes", process.poolRetained},
			{"go_memstats_next_gc_bytes", process.nextGC},
			{"go_gc_gogc_percent", process.gogc},
			{"go_memstats_stack_inuse_bytes", process.stack},
			{"go_memstats_last_gc_time_seconds", process.lastGC},
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
