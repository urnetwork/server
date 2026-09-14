package monitor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func logShipperFixture(overrides map[string]string) string {
	keys := []string{
		"observation_schema", "active_state", "sub_state", "result", "restarts",
		"nofile_hard", "nofile_soft", "fluent_bit_version", "restart_reason",
		"redis_exporter_state", "redis_latency_histogram_policy",
	}
	values := map[string]string{
		"observation_schema": "3", "active_state": "active", "sub_state": "running",
		"result": "success", "restarts": "0", "nofile_hard": "65536", "nofile_soft": "65536",
		"fluent_bit_version": "4.2.1", "restart_reason": logShipperRestartNone,
		"redis_exporter_state": "active", "redis_latency_histogram_policy": "excluded",
	}
	for key, value := range overrides {
		values[key] = value
	}
	if values["restarts"] != "0" {
		if _, explicitlySet := overrides["restart_reason"]; !explicitlySet {
			values["restart_reason"] = logShipperRestartOther
		}
	}
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := values[key]; ok {
			lines = append(lines, key+"="+value)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestLogShipperSignalSyntheticProblemClassesAndHostScope(t *testing.T) {
	observations := map[string]string{
		"healthy": logShipperFixture(nil),
		"down":    logShipperFixture(map[string]string{"active_state": "failed", "sub_state": "failed", "result": "exit-code"}),
		"low-fd":  logShipperFixture(map[string]string{"nofile_soft": "1024"}),
		"churn":   logShipperFixture(map[string]string{"restarts": "5"}),
		"decoder": logShipperFixture(map[string]string{
			"restarts": "1", "restart_reason": logShipperRestartHistogramDecoder,
		}),
		"pg": logShipperFixture(nil), "redis": logShipperFixture(nil),
		"backup": logShipperFixture(nil), "subtensor": logShipperFixture(nil),
	}
	seen := map[string]int{}
	var seenMu sync.Mutex
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if !strings.Contains(command, logShipperMarker) {
			t.Fatalf("log shipper marker absent")
		}
		seenMu.Lock()
		seen[host.Name]++
		seenMu.Unlock()
		return observations[host.Name], nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "healthy", Roles: []string{"services"}},
		{Name: "down", Roles: []string{"services"}},
		{Name: "low-fd", Roles: []string{"services"}},
		{Name: "churn", Roles: []string{"services"}},
		{Name: "decoder", Roles: []string{"redis-cluster"}},
		{Name: "pg", Roles: []string{"pg-primary"}},
		{Name: "redis", Roles: []string{"redis-cluster", "minio"}},
		{Name: "backup", Roles: []string{"backup"}},
		{Name: "subtensor", Roles: []string{"subtensor"}},
		{Name: "vpn", Roles: []string{"vpn-server"}},
	}
	alerts, err := NewLogShipperSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 4 {
		t.Fatalf("alerts=%d, want 4: %+v", len(alerts), alerts)
	}
	byTarget := map[string]Alert{}
	for _, alert := range alerts {
		byTarget[alert.Target] = alert
	}
	if alert := byTarget["down"]; alert.Class != "log-shipper-down" || alert.Severity != SeverityPage {
		t.Fatalf("down alert=%+v", alert)
	}
	if alert := byTarget["low-fd"]; alert.Class != "log-shipper-fd-budget" || !strings.Contains(alert.Markdown(), "1024") {
		t.Fatalf("fd alert=%+v", alert)
	}
	if alert := byTarget["churn"]; alert.Class != "log-shipper-churn" || !strings.Contains(alert.Markdown(), "restarts=5") {
		t.Fatalf("churn alert=%+v", alert)
	}
	if alert := byTarget["decoder"]; alert.Class != "log-shipper-prometheus-histogram-decoder-crash" ||
		!strings.Contains(alert.Markdown(), "prometheus-histogram-decoder-crash") {
		t.Fatalf("decoder alert=%+v", alert)
	}
	for _, target := range []string{"healthy", "down", "low-fd", "churn", "decoder", "pg", "redis", "backup", "subtensor"} {
		if seen[target] != 1 {
			t.Errorf("host %s observations=%d, want 1", target, seen[target])
		}
	}
	if seen["vpn"] != 0 {
		t.Errorf("vpn-only host was probed")
	}
}

func TestLogShipperSignalSyntheticMalformedIsVisibility(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return logShipperFixture(map[string]string{"restarts": "secret\nunknown=value"}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge", Roles: []string{"services"}}}
	alerts, err := NewLogShipperSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if strings.Contains(alert.Markdown(), "secret") {
		t.Fatalf("raw malformed output leaked: %s", alert.Markdown())
	}
}

func TestLogShipperDetectsRedisLatencyHistogramRuntimePolicy(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{
			name: "optional histogram enabled",
			overrides: map[string]string{
				"redis_latency_histogram_policy": "enabled",
			},
			want: "redis_latency_histogram_policy=enabled",
		},
		{
			name: "exporter inactive",
			overrides: map[string]string{
				"redis_exporter_state": "inactive",
			},
			want: "redis_exporter_state=inactive",
		},
		{
			name: "unit arguments unavailable",
			overrides: map[string]string{
				"redis_exporter_state": "unobservable", "redis_latency_histogram_policy": "unobservable",
			},
			want: "redis_latency_histogram_policy=unobservable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				return logShipperFixture(test.overrides), nil
			}}
			settings := syntheticSettings(source)
			settings.Hosts = []HostSettings{{Name: "cache.invalid", Roles: []string{"redis-cluster"}}}
			alerts, err := NewLogShipperSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, "redis-latency-histogram-policy-drift")
			if !strings.Contains(alert.Markdown(), test.want) ||
				!strings.Contains(alert.Markdown(), "redis_commands_processed_total") {
				t.Fatalf("runtime-policy alert incomplete:\n%s", alert.Markdown())
			}
		})
	}

	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return logShipperFixture(map[string]string{
			"redis_exporter_state": "not-applicable", "redis_latency_histogram_policy": "not-applicable",
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "service.invalid", Roles: []string{"services"}}}
	alerts, err := NewLogShipperSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		if alert.Class == "redis-latency-histogram-policy-drift" {
			t.Fatalf("non-Redis host armed Redis exporter policy: %+v", alert)
		}
	}
}

func TestLogShipperCommandReadsBothFDLimitsAndBoundedCrashEvidence(t *testing.T) {
	for _, want := range []string{
		"systemctl show fluent-bit.service", "LimitNOFILE", "LimitNOFILESoft", "NRestarts",
		"systemctl show redis-exporter.service", "--exclude-latency-histogram-metrics",
		"journalctl -b -n 400", "COREDUMP_COMM=fluent-bit", "COREDUMP_SIGNAL=11",
		"ExecMainStartTimestamp", "-u fluent-bit.service", "--since", "--until",
		"add_metric_histogram", "finish_duplicate_histogram_summary_sum_count",
		"parse_histogram_summary_name", "dpkg-query",
	} {
		if !strings.Contains(logShipperCommand, want) {
			t.Errorf("command lacks %q", want)
		}
	}
	for _, forbidden := range []string{"sudo", "docker", "coredumpctl"} {
		if strings.Contains(logShipperCommand, forbidden) {
			t.Errorf("command contains unrelated boundary %q", forbidden)
		}
	}
	window := `--since "$restart_since" --until "$restart_until"`
	if count := strings.Count(logShipperCommand, window); count != 2 {
		t.Errorf("generation window uses=%d, want one per journal selector", count)
	}
}

func TestLogShipperCommandUsesOnlyGenerationBoundedDecoderStack(t *testing.T) {
	binDir := t.TempDir()
	writeCommand := func(name string, body string) {
		t.Helper()
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCommand("systemctl", `printf '%s\n' \
'ActiveState=active' \
'SubState=running' \
'Result=success' \
'NRestarts=1' \
'LimitNOFILE=65536' \
'LimitNOFILESoft=65536' \
'ExecMainStartTimestamp=Mon 2024-01-01 00:00:00 UTC'
`)
	writeCommand("dpkg-query", "printf '%s' '4.2.1-fixture'\n")
	writeCommand("date", "printf '%s\\n' '1704067200'\n")
	writeCommand("journalctl", `case "$*" in
*"--since @1704066900"*"--until @1704067201"*"COREDUMP_COMM=fluent-bit COREDUMP_SIGNAL=11"*) exit 0 ;;
*"-u fluent-bit.service"*"--since @1704066900"*"--until @1704067201"*) printf '%s\n' \
'add_metric_histogram' \
'finish_duplicate_histogram_summary_sum_count' \
'parse_histogram_summary_name' \
'sensitive-fixture-detail' ;;
*) exit 91 ;;
esac
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := func() string {
		t.Helper()
		command := exec.Command("sh", "-c", logShipperCommand)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("log shipper fixture: %v: %s", err, output)
		}
		return string(output)
	}

	exact := run()
	sample, err := parseLogShipperSample(exact)
	if err != nil {
		t.Fatal(err)
	}
	if sample.restartReason != logShipperRestartHistogramDecoder {
		t.Fatalf("restart reason=%q, want exact decoder crash", sample.restartReason)
	}
	if strings.Contains(exact, "sensitive-fixture-detail") {
		t.Fatalf("raw core detail escaped reducer: %q", exact)
	}

	writeCommand("journalctl", `case "$*" in
*"--since @1704066900"*"--until @1704067201"*"COREDUMP_COMM=fluent-bit COREDUMP_SIGNAL=11"*) exit 0 ;;
*"-u fluent-bit.service"*"--since @1704066900"*"--until @1704067201"*) printf '%s\n' 'different synthetic crash frame' ;;
*) printf '%s\n' 'add_metric_histogram' 'finish_duplicate_histogram_summary_sum_count' 'parse_histogram_summary_name' ;;
esac
`)
	other, err := parseLogShipperSample(run())
	if err != nil {
		t.Fatal(err)
	}
	if other.restartReason != logShipperRestartOther {
		t.Fatalf("restart reason=%q, want other-or-unobservable", other.restartReason)
	}

	writeCommand("date", "exit 1\n")
	writeCommand("journalctl", `printf '%s\n' \
'add_metric_histogram' \
'finish_duplicate_histogram_summary_sum_count' \
'parse_histogram_summary_name'
`)
	unobservable, err := parseLogShipperSample(run())
	if err != nil {
		t.Fatal(err)
	}
	if unobservable.restartReason != logShipperRestartOther {
		t.Fatalf("unavailable generation timestamp reason=%q, want other-or-unobservable", unobservable.restartReason)
	}
}

func TestLogShipperDecoderCrashDoesNotAttributeNonRedisScrapeSource(t *testing.T) {
	sample, err := parseLogShipperSample(logShipperFixture(map[string]string{
		"restarts": "1", "restart_reason": logShipperRestartHistogramDecoder,
	}))
	if err != nil {
		t.Fatal(err)
	}
	finding := findingByClass(
		t,
		evaluateLogShipper("service-fixture", sample, false),
		"log-shipper-prometheus-histogram-decoder-crash",
	)
	rendered := finding.mechanism + finding.context + finding.action + finding.verify
	if strings.Contains(rendered, "Redis exporter") || strings.Contains(rendered, "Redis's optional") {
		t.Fatalf("generic decoder crash attributed an unproven Redis source: %s", rendered)
	}
	if !strings.Contains(rendered, "does not name the offending scrape source") {
		t.Fatalf("generic decoder crash lacks source uncertainty: %s", rendered)
	}
}

func TestLogShipperSampleRejectsUnboundedOrInconsistentCrashMetadata(t *testing.T) {
	for name, observation := range map[string]string{
		"unknown reason": logShipperFixture(map[string]string{
			"restarts": "1", "restart_reason": "raw-crash-text",
		}),
		"reason without restart": logShipperFixture(map[string]string{
			"restart_reason": logShipperRestartHistogramDecoder,
		}),
		"restart without reason": logShipperFixture(map[string]string{
			"restarts": "1", "restart_reason": logShipperRestartNone,
		}),
		"unsafe version": logShipperFixture(map[string]string{
			"fluent_bit_version": "4.2.1 raw detail",
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLogShipperSample(observation); err == nil {
				t.Fatal("malformed crash metadata was accepted")
			}
		})
	}
}
