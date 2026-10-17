package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mimirIndexFixtureOptions struct {
	count             int
	metricsReady      bool
	processStart      time.Time
	gatewayLastSync   time.Time
	gatewaySyncCount  int
	tenantsDiscovered int
	tenantsSynced     int
	indexUpdated      time.Time
}

func mimirIndexFixture(options mimirIndexFixtureOptions) string {
	if options.count == 0 {
		return "mimir_count 0\n"
	}
	lines := []string{
		"instance_begin 14819",
	}
	if !options.metricsReady {
		lines = append(lines, "metrics_ready 0", "instance_end", "mimir_count 1")
		return strings.Join(lines, "\n") + "\n"
	}
	lines = append(lines,
		"metrics_ready 1",
		"version 3.1.1",
		fmt.Sprintf("process_start %d", options.processStart.Unix()),
		fmt.Sprintf("gateway_last_sync %d", options.gatewayLastSync.Unix()),
		fmt.Sprintf("gateway_sync_count %d", options.gatewaySyncCount),
		fmt.Sprintf("tenants_discovered %d", options.tenantsDiscovered),
		fmt.Sprintf("tenants_synced %d", options.tenantsSynced),
	)
	if !options.indexUpdated.IsZero() {
		lines = append(lines, fmt.Sprintf("index_update anonymous %d", options.indexUpdated.Unix()))
	}
	lines = append(lines, "instance_end", "mimir_count 1")
	return strings.Join(lines, "\n") + "\n"
}

func runMimirIndexSynthetic(t *testing.T, fixtures map[string]string) []Alert {
	t.Helper()
	var commands atomic.Int64
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		commands.Add(1)
		if !strings.Contains(command, mimirIndexMarker) || !strings.Contains(command, "/api/v1/status/buildinfo") {
			return "", fmt.Errorf("unexpected Mimir index command on %s: %s", host.Name, command)
		}
		fixture, ok := fixtures[host.Name]
		if !ok {
			return "", fmt.Errorf("unexpected Mimir host %s", host.Name)
		}
		return fixture, nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = make([]HostSettings, 0, len(fixtures)+1)
	for host := range fixtures {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: host, Roles: []string{"services"}})
	}
	// A non-services host must never receive the bundled-Mimir command.
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-only", Roles: []string{"pg-primary"}})

	alerts, err := NewMimirIndexSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if commands.Load() != int64(len(fixtures)) {
		t.Fatalf("Mimir index commands = %d, want %d services hosts", commands.Load(), len(fixtures))
	}
	return alerts
}

func TestMimirIndexSignalHealthySingleGenerationSkew(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	alerts := runMimirIndexSynthetic(t, map[string]string{
		"edge-0": mimirIndexFixture(mimirIndexFixtureOptions{
			count: 1, metricsReady: true,
			processStart: now.Add(-6 * time.Hour), gatewayLastSync: now.Add(-13 * time.Minute),
			gatewaySyncCount: 24, tenantsDiscovered: 1, tenantsSynced: 1,
			// The live control's exact -873s generation gap is below both
			// failure thresholds and must not become an incident.
			indexUpdated: now.Add(-873 * time.Second),
		}),
		"edge-1": mimirIndexFixture(mimirIndexFixtureOptions{
			count: 1, metricsReady: true,
			processStart: now.Add(-6 * time.Hour), gatewayLastSync: now.Add(-5 * time.Minute),
			gatewaySyncCount: 25, tenantsDiscovered: 1, tenantsSynced: 1,
		}),
	})
	if len(alerts) != 0 {
		t.Fatalf("healthy bounded Mimir skew alerted: %+v", alerts)
	}
}

func TestMimirIndexSignalSyntheticChildAndMetricsFailures(t *testing.T) {
	alerts := runMimirIndexSynthetic(t, map[string]string{
		"missing":    "mimir_count 0\n",
		"unreadable": mimirIndexFixture(mimirIndexFixtureOptions{count: 1}),
	})
	missing := requireAlertClass(t, alerts, "mimir-child-missing")
	unreadable := requireAlertClass(t, alerts, "mimir-index-unobservable")
	if missing.SignalNumber != "11.18" || missing.SignalKey != "mimir-index" {
		t.Fatalf("wrong Mimir signal identity: %+v", missing)
	}
	for _, want := range []string{"mimir_instances=0", "healthy predecessor", "SIGNALS.md §11.18"} {
		if !strings.Contains(missing.Markdown(), want) {
			t.Fatalf("missing-child markdown lacks %q: %s", want, missing.Markdown())
		}
	}
	if !strings.Contains(unreadable.Markdown(), "Build-info identified a live Mimir") {
		t.Fatalf("metrics failure lost its discriminator: %s", unreadable.Markdown())
	}
}

func TestMimirIndexSignalSyntheticGatewayFailures(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	alerts := runMimirIndexSynthetic(t, map[string]string{
		"stale": mimirIndexFixture(mimirIndexFixtureOptions{
			count: 1, metricsReady: true,
			processStart: now.Add(-6 * time.Hour), gatewayLastSync: now.Add(-31 * time.Minute),
			gatewaySyncCount: 22, tenantsDiscovered: 1, tenantsSynced: 0,
			indexUpdated: now.Add(-10 * time.Minute),
		}),
	})
	stale := requireAlertClass(t, alerts, "mimir-store-gateway-stale")
	tenants := requireAlertClass(t, alerts, "mimir-store-gateway-tenants")
	for _, want := range []string{"age=31m0s", "sync_count=22", "single-generation", "two successful periodic syncs"} {
		if !strings.Contains(stale.Markdown(), want) {
			t.Fatalf("gateway freshness alert lacks %q: %s", want, stale.Markdown())
		}
	}
	for _, want := range []string{"synced 0 of 1", "tenants_discovered=1 tenants_synced=0", "shared writer signal"} {
		if !strings.Contains(tenants.Markdown(), want) {
			t.Fatalf("gateway tenant alert lacks %q: %s", want, tenants.Markdown())
		}
	}
}

func TestMimirIndexSignalSyntheticBucketWriterFailure(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	alerts := runMimirIndexSynthetic(t, map[string]string{
		"edge-0": mimirIndexFixture(mimirIndexFixtureOptions{
			count: 1, metricsReady: true,
			processStart: now.Add(-6 * time.Hour), gatewayLastSync: now.Add(-5 * time.Minute),
			gatewaySyncCount: 25, tenantsDiscovered: 1, tenantsSynced: 1,
			indexUpdated: now.Add(-36 * time.Minute),
		}),
		"edge-1": mimirIndexFixture(mimirIndexFixtureOptions{
			count: 1, metricsReady: true,
			processStart: now.Add(-6 * time.Hour), gatewayLastSync: now.Add(-4 * time.Minute),
			gatewaySyncCount: 26, tenantsDiscovered: 1, tenantsSynced: 1,
		}),
	})
	stale := requireAlertClass(t, alerts, "mimir-bucket-index-stale")
	for _, want := range []string{"age=36m0s", "two expected updates plus buffer", "one-hour max-stale period", "timestamp advances on two cleanup cadences"} {
		if !strings.Contains(stale.Markdown(), want) {
			t.Fatalf("bucket-index alert lacks %q: %s", want, stale.Markdown())
		}
	}
}

func TestMimirIndexSignalSyntheticMissingWriterMetric(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	alerts := runMimirIndexSynthetic(t, map[string]string{
		"edge-0": mimirIndexFixture(mimirIndexFixtureOptions{
			count: 1, metricsReady: true,
			processStart: now.Add(-2 * time.Hour), gatewayLastSync: now.Add(-5 * time.Minute),
			gatewaySyncCount: 8, tenantsDiscovered: 1, tenantsSynced: 1,
		}),
	})
	missing := requireAlertClass(t, alerts, "mimir-bucket-index-stale")
	if !strings.Contains(missing.Markdown(), "index_updates=0") ||
		!strings.Contains(missing.Markdown(), "no compactor owns a successful bucket-index update metric") {
		t.Fatalf("missing writer metric lacks root discriminator: %s", missing.Markdown())
	}
}

func TestParseMimirIndexHostSampleRejectsFramingMismatch(t *testing.T) {
	_, err := parseMimirIndexHostSample("instance_begin 1\nmetrics_ready 1\ninstance_end\nmimir_count 2\n")
	if err == nil || !strings.Contains(err.Error(), "count=2 instances=1") {
		t.Fatalf("framing mismatch error = %v", err)
	}
}

func TestMimirIndexScriptParsesProductionMetricShapes(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("ss", `#!/bin/sh
printf '%s\n' 'LISTEN 0 4096 127.0.0.1:14819 0.0.0.0:*'
`)
	writeExecutable("curl", `#!/bin/sh
case "${*}" in
  *'/api/v1/status/buildinfo')
    printf '%s\n' '{"application":"Grafana Mimir"}'
    ;;
  *'/metrics')
    printf '%s\n' \
      'cortex_build_info{version="3.1.1"} 1' \
      'process_start_time_seconds 1788192000' \
      'cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds 1788216123' \
      'cortex_bucket_stores_blocks_sync_seconds_count 25' \
      'cortex_bucket_stores_tenants_discovered 1' \
      'cortex_bucket_stores_tenants_synced 1' \
      'cortex_bucket_index_last_successful_update_timestamp_seconds{user="anonymous"} 1788216123'
    ;;
  *) exit 2 ;;
esac
`)

	command := exec.Command("sh", "-c", mimirIndexScript)
	command.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Mimir script: %v\n%s", err, output)
	}
	sample, err := parseMimirIndexHostSample(string(output))
	if err != nil {
		t.Fatalf("parse Mimir script output: %v\n%s", err, output)
	}
	if sample.count != 1 || len(sample.instances) != 1 {
		t.Fatalf("Mimir script instances = %+v", sample)
	}
	instance := sample.instances[0]
	if !instance.metricsReady || instance.version != "3.1.1" ||
		instance.gatewayLastSync != 1788216123 || instance.gatewaySyncCount != 25 ||
		instance.tenantsDiscovered != 1 || instance.tenantsSynced != 1 ||
		instance.indexUpdates["anonymous"] != 1788216123 {
		t.Fatalf("Mimir script lost production-shaped metrics: %+v\n%s", instance, output)
	}
}

func TestMimirIndexSignalDoesNotResolveSharedWriterThroughObservationLoss(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		return "", fmt.Errorf("synthetic SSH loss on %s", host.Name)
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge-0", Roles: []string{"services"}}}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (mimirIndexProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].class != "cannot-observe" || findings[0].healthy {
		t.Fatalf("observation loss produced a shared-writer conclusion: %+v", findings)
	}
}
