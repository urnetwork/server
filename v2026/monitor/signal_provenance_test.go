package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type provenanceFixtureProcess struct {
	job              string
	host             string
	block            string
	instance         string
	version          string
	sourceRevision   string
	sourceModified   string
	imageDigest      string
	sampleTime       time.Time
	sourceSampleTime time.Time
	startTime        time.Time
	rss              float64
	includeStart     bool
	includeBuild     bool
	includeSource    bool
}

func TestProvenanceIdentityValidators(t *testing.T) {
	for _, test := range []struct {
		name string
		got  bool
		want bool
	}{
		{"sha1 revision", validGoSourceRevision(strings.Repeat("a", 40)), true},
		{"sha256 revision", validGoSourceRevision(strings.Repeat("b", 64)), true},
		{"intermediate revision length", validGoSourceRevision(strings.Repeat("c", 41)), false},
		{"abbreviated revision", validGoSourceRevision("078d6c11"), false},
		{"exact image digest", validOCIImageDigest("sha256:" + strings.Repeat("d", 64)), true},
		{"mutable image tag", validOCIImageDigest("2026.9.1+synthetic"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("validator = %t, want %t", test.got, test.want)
			}
		})
	}
}

func TestProvenanceSignalSyntheticCleanNewestFleet(t *testing.T) {
	now := time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)
	old := completeProvenanceFixture("proxy", "fireside", "g1", "old", now.Add(-2*time.Hour))
	old.sourceModified = "true"
	current := completeProvenanceFixture("proxy", "fireside", "g1", "current", now.Add(-time.Minute))
	worker := completeProvenanceFixture("taskworker", "edge-1", "g2", "worker", now.Add(-time.Hour))

	alerts := runProvenanceFixture(t, now, provenanceFixtureJSON(t, now, old, current, worker))
	if len(alerts) != 0 {
		t.Fatalf("clean newest provenance alerted: %+v", alerts)
	}
}

func TestProvenanceSignalSyntheticNewestMissingSource(t *testing.T) {
	now := time.Date(2026, 9, 1, 7, 1, 0, 0, time.UTC)
	old := completeProvenanceFixture("proxy", "fireside", "g1", "old", now.Add(-2*time.Hour))
	current := completeProvenanceFixture("proxy", "fireside", "g1", "current", now.Add(-time.Minute))
	current.includeSource = false

	alerts := runProvenanceFixture(t, now, provenanceFixtureJSON(t, now, old, current))
	alert := requireAlertClass(t, alerts, "service-provenance-unobservable")
	if alert.SignalNumber != "8.12" || alert.SignalKey != "provenance" || alert.SignalID != "deploy/provenance" {
		t.Fatalf("wrong provenance signal identity: %+v", alert)
	}
	if alert.Frame != "" {
		t.Fatalf("replicated metrics gateway leaked into stable alert identity: %+v", alert)
	}
	for _, want := range []string{
		"1 of 1 newest fresh service identities",
		"proxy/fireside/g1#current[source-info]",
		"WARP_VERSION",
		"Newest process start suppresses a draining generation",
		"server commit 236bf0ce",
		"config-only rollout cannot add",
		"SIGNALS.md §8.12",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("provenance visibility alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Observed, "#old") {
		t.Fatalf("provenance visibility retained the draining generation: %s", alert.Observed)
	}
}

func TestProvenanceSignalSyntheticAllowsModifiedAndRejectsMalformed(t *testing.T) {
	now := time.Date(2026, 9, 1, 7, 2, 0, 0, time.UTC)
	dirty := completeProvenanceFixture("api", "edge-0", "beta", "dirty", now.Add(-time.Hour))
	dirty.sourceModified = "true"
	noDigest := completeProvenanceFixture("connect", "edge-1", "g2", "no-digest", now.Add(-time.Hour))
	noDigest.imageDigest = ""
	badRevision := completeProvenanceFixture("taskworker", "edge-3", "g4", "bad-revision", now.Add(-time.Hour))
	badRevision.sourceRevision = "078d6c11"
	badModified := completeProvenanceFixture("proxy", "fireside", "g3", "bad-modified", now.Add(-time.Hour))
	badModified.sourceModified = "FALSE"

	alerts := runProvenanceFixture(
		t,
		now,
		provenanceFixtureJSON(t, now, dirty, noDigest, badRevision, badModified),
	)
	alert := requireAlertClass(t, alerts, "service-provenance-invalid")
	for _, want := range []string{
		"3 of 4 newest fresh service identities",
		"connect/edge-1/g2#no-digest[image-digest]",
		"taskworker/edge-3/g4#bad-revision[source-revision]",
		"proxy/fireside/g3#bad-modified[source-modified-label]",
		"modified base revision 078d6c11",
		"BuildKit context provenance alone is insufficient",
		"intentional local-checkout workflow",
		"modified=true itself is not malformed",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("invalid provenance alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	for _, candidate := range alerts {
		if candidate.Class == "service-provenance-unobservable" {
			t.Fatalf("present but invalid source family was classified as absent: %+v", alerts)
		}
	}
	if strings.Contains(alert.Observed, "api/edge-0/beta#dirty") {
		t.Fatalf("intentional modified identity was classified invalid: %s", alert.Observed)
	}
}

func TestProvenanceSignalSyntheticModifiedIdentityIsHealthy(t *testing.T) {
	now := time.Date(2026, 9, 1, 7, 2, 0, 0, time.UTC)
	modified := completeProvenanceFixture("api", "edge-0", "beta", "modified", now.Add(-time.Hour))
	modified.sourceModified = "true"

	alerts := runProvenanceFixture(t, now, provenanceFixtureJSON(t, now, modified))
	if len(alerts) != 0 {
		t.Fatalf("intentional modified service artifact alerted: %+v", alerts)
	}
}

func TestProvenanceSignalSyntheticDigestConflict(t *testing.T) {
	now := time.Date(2026, 9, 1, 7, 3, 0, 0, time.UTC)
	digest := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	api := completeProvenanceFixture("api", "edge-0", "g1", "api", now.Add(-time.Hour))
	api.imageDigest = digest
	connect := completeProvenanceFixture("connect", "edge-1", "g1", "connect", now.Add(-time.Hour))
	connect.imageDigest = digest
	connect.sourceRevision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	alerts := runProvenanceFixture(t, now, provenanceFixtureJSON(t, now, api, connect))
	alert := requireAlertClass(t, alerts, "service-provenance-conflict")
	for _, want := range []string{
		"1 immutable image digests",
		digest,
		"api/edge-0/g1#api",
		"connect/edge-1/g1#connect",
		"Stop promotion",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("provenance conflict alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	for _, candidate := range alerts {
		if candidate.Class == "service-provenance-invalid" {
			t.Fatalf("conflicting but individually valid provenance was classified as malformed: %+v", alerts)
		}
	}
}

func TestProvenanceSignalSyntheticStaleSourceIsMissing(t *testing.T) {
	now := time.Date(2026, 9, 1, 7, 4, 0, 0, time.UTC)
	process := completeProvenanceFixture("proxy", "crisp", "g4", "current", now.Add(-time.Hour))
	process.sourceSampleTime = now.Add(-2 * time.Minute)

	alerts := runProvenanceFixture(t, now, provenanceFixtureJSON(t, now, process))
	alert := requireAlertClass(t, alerts, "service-provenance-unobservable")
	if !strings.Contains(alert.Observed, "proxy/crisp/g4#current[source-info]") {
		t.Fatalf("stale source family was not excluded independently: %s", alert.Observed)
	}
}

func TestProvenanceSignalSyntheticFreshRSSWithoutStartIsVisible(t *testing.T) {
	now := time.Date(2026, 9, 1, 7, 5, 0, 0, time.UTC)
	process := completeProvenanceFixture("taskworker", "edge-4", "g3", "no-start", now.Add(-time.Hour))
	process.includeStart = false

	alerts := runProvenanceFixture(t, now, provenanceFixtureJSON(t, now, process))
	alert := requireAlertClass(t, alerts, "service-provenance-unobservable")
	if !strings.Contains(alert.Observed, "taskworker/edge-4/g3#no-start[process-start]") {
		t.Fatalf("fresh RSS identity without start was dropped: %s", alert.Observed)
	}
}

func completeProvenanceFixture(job, host, block, instance string, start time.Time) provenanceFixtureProcess {
	return provenanceFixtureProcess{
		job: job, host: host, block: block, instance: instance,
		version:        "2026.9.1+synthetic",
		sourceRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		sourceModified: "false",
		imageDigest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		startTime:      start,
		rss:            float64(1 << 30),
		includeStart:   true,
		includeBuild:   true,
		includeSource:  true,
	}
}

func runProvenanceFixture(t testing.TB, now time.Time, payload string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			"process_resident_memory_bytes",
			"process_start_time_seconds",
			"urnetwork_build_info",
			"urnetwork_source_info",
			"competitionworker",
			"timestamp%28label_replace",
			"monitor_metric",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("Mimir command on %s omitted %q: %s", host.Name, want, command)
			}
		}
		if host.Name != "metrics-1" || !strings.Contains(command, "%22synthetic%22") {
			return "", fmt.Errorf("unexpected Mimir command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := NewProvenanceSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func provenanceFixtureJSON(
	t testing.TB,
	now time.Time,
	processes ...provenanceFixtureProcess,
) string {
	t.Helper()
	result := []map[string]any{}
	for _, process := range processes {
		labels := map[string]string{
			"job": process.job, "host": process.host, "block": process.block, "instance": process.instance,
		}
		sampleTime := process.sampleTime
		if sampleTime.IsZero() {
			sampleTime = now
		}
		add := func(name string, value float64, observedAt time.Time, extra map[string]string) {
			metric := map[string]string{"__name__": name}
			for key, label := range labels {
				metric[key] = label
			}
			for key, label := range extra {
				metric[key] = label
			}
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(observedAt.Unix()), fmt.Sprintf("%.0f", value)},
			})
		}
		add("process_resident_memory_bytes", process.rss, sampleTime, nil)
		if process.includeStart {
			add("process_start_time_seconds", float64(process.startTime.Unix()), sampleTime, nil)
		}
		if process.includeBuild {
			add("urnetwork_build_info", 1, sampleTime, map[string]string{"version": process.version})
		}
		if process.includeSource {
			sourceTime := process.sourceSampleTime
			if sourceTime.IsZero() {
				sourceTime = sampleTime
			}
			add("urnetwork_source_info", 1, sourceTime, map[string]string{
				"revision": process.sourceRevision, "modified": process.sourceModified, "image_digest": process.imageDigest,
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
	return string(payload)
}
