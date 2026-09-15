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
		"journal_reader_state", "journal_reader_errors_10m", "journal_reader_ebadmsg_errors_10m",
		"redis_exporter_state", "redis_latency_histogram_policy",
	}
	values := map[string]string{
		"observation_schema": "4", "active_state": "active", "sub_state": "running",
		"result": "success", "restarts": "0", "nofile_hard": "65536", "nofile_soft": "65536",
		"fluent_bit_version": "4.2.1", "restart_reason": logShipperRestartNone,
		"journal_reader_state": "healthy", "journal_reader_errors_10m": "0", "journal_reader_ebadmsg_errors_10m": "0",
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
		"journal-loss": logShipperFixture(map[string]string{
			"journal_reader_state": "ebadmsg", "journal_reader_errors_10m": "3", "journal_reader_ebadmsg_errors_10m": "3",
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
		{Name: "journal-loss", Roles: []string{"services"}},
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
	if len(alerts) != 5 {
		t.Fatalf("alerts=%d, want 5: %+v", len(alerts), alerts)
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
	if alert := byTarget["journal-loss"]; alert.Class != "log-shipper-journal-read-loss" ||
		alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), "journal_reader_ebadmsg_errors_10m=3") ||
		!strings.Contains(alert.Markdown(), "seeking the head can also replay") {
		t.Fatalf("journal reader alert=%+v", alert)
	}
	for _, target := range []string{"healthy", "down", "low-fd", "churn", "decoder", "journal-loss", "pg", "redis", "backup", "subtensor"} {
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
				"redis_exporter_state": "inactive", "redis_latency_histogram_policy": "unobservable",
			},
			want: "redis_exporter_state=inactive",
		},
	}
	for _, test := range tests {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			return logShipperFixture(test.overrides), nil
		}}
		settings := syntheticSettings(source)
		settings.Hosts = []HostSettings{{Name: "cache.example.test", Roles: []string{"redis-cluster"}}}
		alerts, err := NewLogShipperSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "redis-latency-histogram-policy-drift")
		if !strings.Contains(alert.Markdown(), test.want) ||
			!strings.Contains(alert.Markdown(), "redis_commands_processed_total") {
			t.Fatalf("%s: runtime-policy alert incomplete:\n%s", test.name, alert.Markdown())
		}
	}

	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return logShipperFixture(map[string]string{
			"redis_exporter_state": "not-applicable", "redis_latency_histogram_policy": "not-applicable",
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "service.example.test", Roles: []string{"services"}}}
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

func TestLogShipperUnknownRedisPolicyCannotAttributeOrResolveDrift(t *testing.T) {
	for _, overrides := range []map[string]string{
		{"redis_exporter_state": "unobservable", "redis_latency_histogram_policy": "unobservable"},
		{"redis_exporter_state": "active", "redis_latency_histogram_policy": "unobservable"},
	} {
		sample, err := parseLogShipperSample(logShipperFixture(overrides))
		if err != nil {
			t.Fatal(err)
		}
		findings := evaluateLogShipper("cache.example.test", sample, true)
		visibility := findingByClass(t, findings, "cannot-observe")
		if visibility.healthy || visibility.target != "cache.example.test/redis-latency-histogram-policy" {
			t.Fatalf("unknown policy lost visibility boundary: %+v", visibility)
		}
		for _, finding := range findings {
			if finding.class == "redis-latency-histogram-policy-drift" {
				t.Fatalf("unknown policy attributed or resolved drift: %+v", finding)
			}
		}
	}
}

func TestLogShipperRedisPolicyReducerUsesExactNulArgvAndGoBoolSemantics(t *testing.T) {
	for _, test := range []struct {
		name                  string
		args                  []string
		environment           string
		environmentObservable bool
		want                  string
	}{
		{name: "bare true", args: []string{"--exclude-latency-histogram-metrics"}, want: "excluded"},
		{name: "explicit true", args: []string{"--exclude-latency-histogram-metrics=true"}, want: "excluded"},
		{name: "single dash true", args: []string{"-exclude-latency-histogram-metrics=1"}, want: "excluded"},
		{name: "explicit false", args: []string{"--exclude-latency-histogram-metrics=false"}, want: "enabled"},
		{name: "repeat last false", args: []string{"--exclude-latency-histogram-metrics", "--exclude-latency-histogram-metrics=0"}, want: "enabled"},
		{name: "repeat last true", args: []string{"--exclude-latency-histogram-metrics=false", "--exclude-latency-histogram-metrics=True"}, want: "excluded"},
		{name: "invalid bool", args: []string{"--exclude-latency-histogram-metrics=maybe"}, want: "unobservable"},
		{name: "flag suffix", args: []string{"--exclude-latency-histogram-metrics-suffix"}, want: "unobservable"},
		{name: "embedded flag in value", args: []string{"--redis.addr=redis://fixture.example.test:6379/--exclude-latency-histogram-metrics"}, environmentObservable: true, want: "enabled"},
		{name: "embedded newline in value", args: []string{"--redis.addr=fixture\n--exclude-latency-histogram-metrics"}, environmentObservable: true, want: "enabled"},
		{name: "separate string value", args: []string{"--redis.addr", "--exclude-latency-histogram-metrics"}, environmentObservable: true, want: "enabled"},
		{name: "after end marker", args: []string{"--", "--exclude-latency-histogram-metrics"}, environmentObservable: true, want: "enabled"},
		{name: "after positional", args: []string{"fixture", "--exclude-latency-histogram-metrics"}, environmentObservable: true, want: "enabled"},
		{name: "ambiguous other bare flag", args: []string{"--other", "--exclude-latency-histogram-metrics"}, want: "unobservable"},
		{name: "environment missing", want: "unobservable"},
		{name: "default false", environmentObservable: true, want: "enabled"},
		{name: "environment true", environment: "REDIS_EXPORTER_EXCLUDE_LATENCY_HISTOGRAM_METRICS=true\x00", environmentObservable: true, want: "excluded"},
		{name: "environment false", environment: "REDIS_EXPORTER_EXCLUDE_LATENCY_HISTOGRAM_METRICS=false\x00", environmentObservable: true, want: "enabled"},
		{name: "environment invalid defaults false", environment: "REDIS_EXPORTER_EXCLUDE_LATENCY_HISTOGRAM_METRICS=maybe\x00", environmentObservable: true, want: "enabled"},
		{name: "argv overrides environment", args: []string{"--exclude-latency-histogram-metrics=false"}, environment: "REDIS_EXPORTER_EXCLUDE_LATENCY_HISTOGRAM_METRICS=true\x00", environmentObservable: true, want: "enabled"},
		{name: "unrelated private environment", args: []string{"--exclude-latency-histogram-metrics"}, environment: "FIXTURE_SECRET=synthetic-private-value\x00", environmentObservable: true, want: "excluded"},
	} {
		dir := t.TempDir()
		argvPath := filepath.Join(dir, "cmdline")
		environmentPath := filepath.Join(dir, "environ")
		args := append([]string{"redis_exporter"}, test.args...)
		if err := os.WriteFile(argvPath, []byte(strings.Join(args, "\x00")+"\x00"), 0o600); err != nil {
			t.Fatal(err)
		}
		if test.environmentObservable {
			if err := os.WriteFile(environmentPath, []byte(test.environment), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		command := exec.Command("sh", "-c", logShipperRedisPolicyReducer+`redis_histogram_policy "$1" "$2"`, "fixture", argvPath, environmentPath)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: reducer exit: %v", test.name, err)
		}
		if got := strings.TrimSpace(string(output)); got != test.want {
			t.Errorf("%s: policy=%q, want %q", test.name, got, test.want)
		}
	}
}

func TestLogShipperCommandObservesLiveArgvNotReloadedExecStart(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	procDir := filepath.Join(dir, "proc")
	processDir := filepath.Join(procDir, "123")
	for _, path := range []string{binDir, processDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeCommand := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const systemctlFixture = `case "$*" in
*"show fluent-bit.service"*) printf '%s\n' ActiveState=active SubState=running Result=success NRestarts=0 LimitNOFILE=65536 LimitNOFILESoft=65536 ExecMainStartTimestamp=fixture ;;
*"show redis-exporter.service"*"--value"*) printf '%s\n' 123 ;;
*"show redis-exporter.service"*) printf '%s\n' LoadState=loaded ActiveState=active SubState=running MainPID=123 'ExecStart=--exclude-latency-histogram-metrics=true synthetic-private-value' ;;
*) exit 1 ;;
esac
`
	writeCommand(systemctlFixture)
	if err := os.WriteFile(filepath.Join(processDir, "stat"), []byte("123 (fixture) S "+strings.Repeat("0 ", 18)+"7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processDir, "comm"), []byte("redis_exporter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeArgv := func(flag string) {
		t.Helper()
		argv := "redis_exporter\x00--redis.addr=redis://user:synthetic-private-value@cache.example.test:6379\x00" + flag + "\x00"
		if err := os.WriteFile(filepath.Join(processDir, "cmdline"), []byte(argv), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	commandText := strings.Replace(logShipperCommand, "redis_process_root=/proc", "redis_process_root="+shellSingleQuote(procDir), 1)
	run := func() logShipperSample {
		t.Helper()
		output, err := exec.Command("sh", "-c", commandText).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture command: %v", err)
		}
		if strings.Contains(string(output), "synthetic-private-value") || strings.Contains(string(output), "cache.example.test") {
			t.Fatal("raw live arguments escaped the host reducer")
		}
		sample, err := parseLogShipperSample(string(output))
		if err != nil {
			t.Fatal(err)
		}
		return sample
	}
	writeArgv("--exclude-latency-histogram-metrics=false")
	if sample := run(); sample.redisExporterState != "active" || sample.redisLatencyHistogramPolicy != "enabled" {
		t.Fatalf("desired unit definition hid the live false policy: %+v", sample)
	}
	writeArgv("--exclude-latency-histogram-metrics")
	if sample := run(); sample.redisLatencyHistogramPolicy != "excluded" {
		t.Fatalf("explicit live exclusion requires no readable environment: %+v", sample)
	}
	writeCommand(strings.Replace(systemctlFixture, "printf '%s\\n' 123", "printf '%s\\n' 124", 1))
	if sample := run(); sample.redisExporterState != "unobservable" || sample.redisLatencyHistogramPolicy != "unobservable" {
		t.Fatalf("process generation changed without observation failure: %+v", sample)
	}
}

func TestLogShipperCanceledObservationReturnsContextWithoutAlerts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		cancel()
		return "", ctx.Err()
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "shipper.example.test", Roles: []string{"redis-cluster"}}}
	alerts, err := NewLogShipperSignal().Run(ctx, settings)
	if err != context.Canceled || len(alerts) != 0 {
		t.Fatalf("cancellation emitted fabricated alerts: error=%v alerts=%d", err, len(alerts))
	}
}

func TestLogShipperCommandReadsBothFDLimitsAndBoundedCrashEvidence(t *testing.T) {
	for _, want := range []string{
		"systemctl show fluent-bit.service", "LimitNOFILE", "LimitNOFILESoft", "NRestarts",
		"systemctl show redis-exporter.service", "exclude-latency-histogram-metrics", "MainPID", "redis_histogram_policy", "/stat", "/cmdline", "/environ",
		"journalctl -b -n 400", "COREDUMP_COMM=fluent-bit", "COREDUMP_SIGNAL=11",
		"ExecMainStartTimestamp", "-u fluent-bit.service", "--since", "--until",
		"add_metric_histogram", "finish_duplicate_histogram_summary_sum_count",
		"parse_histogram_summary_name", "dpkg-query",
		"sd_journal_next[(][)] returned error -[0-9]+", "journal_reader_errors_10m",
		"journal_reader_ebadmsg_errors_10m", "-n 401", "10 minutes ago",
	} {
		if !strings.Contains(logShipperCommand, want) {
			t.Errorf("command lacks %q", want)
		}
	}
	for _, forbidden := range []string{"docker", "coredumpctl", `bounded_nul_bytes "$2" allow-sudo`, `bounded_nul_bytes "$1" allow-sudo`} {
		if strings.Contains(logShipperCommand, forbidden) {
			t.Errorf("command contains unrelated boundary %q", forbidden)
		}
	}
	window := `--since "$restart_since" --until "$restart_until"`
	if count := strings.Count(logShipperCommand, window); count != 2 {
		t.Errorf("generation window uses=%d, want one per journal selector", count)
	}
}

func TestLogShipperCommandReducesJournalReadLossWithoutLeakingJournalText(t *testing.T) {
	binDir := t.TempDir()
	writeCommand := func(name string, body string) {
		t.Helper()
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCommand("systemctl", `case "$*" in
*"show fluent-bit.service"*) printf '%s\n' ActiveState=active SubState=running Result=success NRestarts=0 LimitNOFILE=65536 LimitNOFILESoft=65536 'ExecMainStartTimestamp=Mon 2024-01-01 00:00:00 UTC' ;;
*"show redis-exporter.service"*) printf '%s\n' LoadState=not-found ActiveState=inactive SubState=dead MainPID=0 ;;
*) exit 91 ;;
esac
`)
	writeCommand("dpkg-query", "printf '%s' '4.2.3-fixture'\n")
	writeCommand("journalctl", `printf '%s\n' \
'[fixture] sd_journal_next() returned error -74; journal is re-opened, unread logs are lost; sd_journal_seek_head() returned 0 synthetic-private-value' \
'[fixture] sd_journal_next() returned error -74; journal is re-opened, unread logs are lost; sd_journal_seek_head() returned 0 another-private-value'
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := exec.Command("sh", "-c", logShipperCommand).CombinedOutput()
	if err != nil {
		t.Fatalf("log shipper fixture: %v: %s", err, output)
	}
	if strings.Contains(string(output), "synthetic-private-value") || strings.Contains(string(output), "another-private-value") {
		t.Fatalf("raw journal text escaped reducer: %q", output)
	}
	sample, err := parseLogShipperSample(string(output))
	if err != nil {
		t.Fatal(err)
	}
	if sample.journalReaderState != "ebadmsg" || sample.journalReaderErrors != 2 || sample.journalReaderEBADMSGErrors != 2 {
		t.Fatalf("journal reader reduction=%+v", sample)
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
		"healthy with errors": logShipperFixture(map[string]string{
			"journal_reader_errors_10m": "1",
		}),
		"ebadmsg without errors": logShipperFixture(map[string]string{
			"journal_reader_state": "ebadmsg",
		}),
		"other with only ebadmsg": logShipperFixture(map[string]string{
			"journal_reader_state": "other-error", "journal_reader_errors_10m": "1", "journal_reader_ebadmsg_errors_10m": "1",
		}),
		"truncated below cap": logShipperFixture(map[string]string{
			"journal_reader_state": "truncated", "journal_reader_errors_10m": "400",
		}),
		"unobservable with counts": logShipperFixture(map[string]string{
			"journal_reader_state": "unobservable", "journal_reader_errors_10m": "1",
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLogShipperSample(observation); err == nil {
				t.Fatal("malformed crash metadata was accepted")
			}
		})
	}
}
