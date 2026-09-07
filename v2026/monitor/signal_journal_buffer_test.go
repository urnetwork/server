package monitor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func journalBufferFixture(overrides map[string]string) string {
	keys := []string{
		"observation_schema", "journald_active", "journald_active_seconds", "storage",
		"max_use", "max_file_size", "max_files", "max_file_sec", "max_retention",
		"uptime_seconds", "coverage_checked", "coverage_present",
		"boundary_entry_age_seconds", "coverage_target_seconds",
	}
	values := map[string]string{
		"observation_schema": "3", "journald_active": "active", "journald_active_seconds": "7200",
		"storage": "persistent", "max_use": "100G", "max_file_size": "256M", "max_files": "1024",
		"max_file_sec": "5min", "max_retention": "1hour", "uptime_seconds": "7200",
		"coverage_checked": "1", "coverage_present": "1",
		"boundary_entry_age_seconds": "3300", "coverage_target_seconds": "3000",
	}
	for key, value := range overrides {
		values[key] = value
	}
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := values[key]; ok {
			lines = append(lines, key+"="+value)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestJournalBufferSignalSyntheticProblemClasses(t *testing.T) {
	observations := map[string]string{
		"healthy": journalBufferFixture(nil),
		"drift":   journalBufferFixture(map[string]string{"max_retention": "7day"}),
		"short": journalBufferFixture(map[string]string{
			"coverage_present": "0", "boundary_entry_age_seconds": "1200",
		}),
		"inactive": journalBufferFixture(map[string]string{"journald_active": "failed"}),
		"young": journalBufferFixture(map[string]string{
			"uptime_seconds": "1200", "journald_active_seconds": "1200",
			"coverage_checked": "0", "coverage_present": "0", "boundary_entry_age_seconds": "1200",
		}),
		"recent-journal-restart": journalBufferFixture(map[string]string{
			"journald_active_seconds": "600", "coverage_checked": "0",
			"coverage_present": "0", "boundary_entry_age_seconds": "600",
		}),
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if !strings.Contains(command, journalBufferMarker) {
			t.Fatalf("journal buffer marker absent")
		}
		return observations[host.Name], nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "healthy", Roles: []string{"services"}},
		{Name: "drift", Roles: []string{"services"}},
		{Name: "short", Roles: []string{"services"}},
		{Name: "inactive", Roles: []string{"services"}},
		{Name: "young", Roles: []string{"services"}},
		{Name: "recent-journal-restart", Roles: []string{"services"}},
		{Name: "not-an-edge", Roles: []string{"pg-primary"}},
	}
	alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 3 {
		t.Fatalf("alerts=%d, want 3: %+v", len(alerts), alerts)
	}
	byTarget := map[string]Alert{}
	for _, alert := range alerts {
		byTarget[alert.Target] = alert
	}
	if alert := byTarget["drift"]; alert.Class != "journal-buffer-config" || alert.Severity != SeverityWarn {
		t.Fatalf("drift alert=%+v", alert)
	}
	if alert := byTarget["short"]; alert.Class != "journal-buffer-short" || !strings.Contains(alert.Markdown(), "Loki") {
		t.Fatalf("short alert=%+v", alert)
	}
	if alert := byTarget["inactive"]; alert.Class != "journal-buffer-unavailable" || alert.Severity != SeverityPage {
		t.Fatalf("inactive alert=%+v", alert)
	}
}

func TestJournalBufferSignalSyntheticMalformedIsVisibility(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return journalBufferFixture(map[string]string{"coverage_present": "secret\nunknown=value"}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge", Roles: []string{"services"}}}
	alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if strings.Contains(alert.Markdown(), "secret") {
		t.Fatalf("raw malformed output leaked: %s", alert.Markdown())
	}
}

func TestJournalBufferSampleRejectsPreviousSchema(t *testing.T) {
	_, err := parseJournalBufferSample(journalBufferFixture(map[string]string{"observation_schema": "2"}))
	if err == nil || !strings.Contains(err.Error(), "unsupported observation schema") {
		t.Fatalf("parse error=%v, want unsupported schema", err)
	}
}

func TestJournalBufferSignalSyntheticLowVolumeWholeFileLoss(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return journalBufferFixture(map[string]string{
			"storage": "auto", "max_file_sec": "1month",
			"coverage_present": "0", "boundary_entry_age_seconds": "900",
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "low-volume-proxy", Roles: []string{"services"}}}
	alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	config := requireAlertClass(t, alerts, "journal-buffer-config")
	if !strings.Contains(config.Markdown(), "whole-file rotation") ||
		!strings.Contains(config.Evidence, "storage=auto") ||
		!strings.Contains(config.Evidence, "max_file_sec=1month") {
		t.Fatalf("configuration alert does not explain low-volume loss: %s", config.Markdown())
	}
	short := requireAlertClass(t, alerts, "journal-buffer-short")
	if !strings.Contains(short.Evidence, "age=900s, required=3000s") {
		t.Fatalf("coverage evidence=%q", short.Evidence)
	}
}

func TestJournalBufferCommandIsBoundedAndUsesEffectiveConfig(t *testing.T) {
	for _, want := range []string{
		"systemd-analyze cat-config systemd/journald.conf",
		"timeout 10s journalctl -q -b 0 --reverse -n 1",
		"--until '50 minutes ago' -n 1",
		"--output-fields=__REALTIME_TIMESTAMP -o json",
		"ActiveEnterTimestampMonotonic",
		"Storage", "SystemMaxUse", "SystemMaxFileSize", "SystemMaxFiles", "MaxFileSec", "MaxRetentionSec",
	} {
		if !strings.Contains(journalBufferCommand, want) {
			t.Errorf("command lacks %q", want)
		}
	}
	for _, forbidden := range []string{"sudo", "--list-boots", "mktemp", ">/tmp", "> /tmp"} {
		if strings.Contains(journalBufferCommand, forbidden) {
			t.Errorf("command contains unsafe/unbounded boundary %q", forbidden)
		}
	}
}

func TestJournalBufferCommandCrossSystemdCoverage(t *testing.T) {
	for _, test := range []struct {
		name          string
		latestMode    string
		boundaryMode  string
		wantPresent   bool
		wantCommandOK bool
	}{
		{name: "systemd-249-string-timestamp", latestMode: "string", boundaryMode: "string", wantPresent: true, wantCommandOK: true},
		{name: "systemd-255-numeric-timestamp", latestMode: "numeric", boundaryMode: "numeric", wantPresent: true, wantCommandOK: true},
		{name: "genuinely-short", latestMode: "string", boundaryMode: "empty", wantPresent: false, wantCommandOK: true},
		{name: "latest-empty", latestMode: "empty", boundaryMode: "string"},
		{name: "latest-missing-timestamp", latestMode: "missing", boundaryMode: "string"},
		{name: "latest-malformed", latestMode: "malformed", boundaryMode: "string"},
		{name: "latest-future", latestMode: "future", boundaryMode: "string"},
		{name: "cutoff-missing-timestamp", latestMode: "string", boundaryMode: "missing"},
		{name: "cutoff-duplicate-timestamp", latestMode: "string", boundaryMode: "duplicate"},
		{name: "cutoff-malformed", latestMode: "string", boundaryMode: "malformed"},
		{name: "cutoff-too-young", latestMode: "string", boundaryMode: "young"},
		{name: "access-error", latestMode: "error", boundaryMode: "string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := runJournalBufferCommand(t, test.latestMode, test.boundaryMode)
			if !test.wantCommandOK {
				if err == nil {
					t.Fatalf("command succeeded with latest=%s boundary=%s:\n%s", test.latestMode, test.boundaryMode, output)
				}
				if strings.Contains(output, "private-journal-detail") {
					t.Fatalf("command leaked raw journal failure: %s", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("command failed: %v\n%s", err, output)
			}
			sample, err := parseJournalBufferSample(output)
			if err != nil {
				t.Fatal(err)
			}
			if sample.coveragePresent != test.wantPresent {
				t.Fatalf("coverage_present=%t, want %t:\n%s", sample.coveragePresent, test.wantPresent, output)
			}
			if test.wantPresent && sample.boundaryEntryAgeSeconds < 3000 {
				t.Fatalf("boundary age=%d, want >=3000", sample.boundaryEntryAgeSeconds)
			}
		})
	}
}

func TestJournalBufferCommandSystemd249PlainListBootsUsesEntryQuery(t *testing.T) {
	const systemd249ListBoots = " 0 redacted Fri 2026-09-04 19:45:54 UTC—Fri 2026-09-04 20:45:20 UTC\n"
	if strings.Contains(systemd249ListBoots, `"first_entry"`) {
		t.Fatal("systemd 249 fixture unexpectedly satisfies the retired JSON first_entry parser")
	}

	output, err := runJournalBufferCommand(t, "string", "string")
	if err != nil {
		t.Fatalf("entry-query command failed on systemd 249 fixture: %v\n%s", err, output)
	}
	sample, err := parseJournalBufferSample(output)
	if err != nil {
		t.Fatal(err)
	}
	if !sample.coveragePresent || sample.boundaryEntryAgeSeconds < 3000 {
		t.Fatalf("entry-query sample=%+v, want a valid cutoff witness", sample)
	}
}

func runJournalBufferCommand(t *testing.T, latestMode, boundaryMode string) (string, error) {
	t.Helper()
	bin := t.TempDir()
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("systemctl", `#!/bin/sh
case "$1" in
  is-active) echo active ;;
  show) echo 0 ;;
  *) exit 1 ;;
esac
`)
	writeExecutable("systemd-analyze", `#!/bin/sh
cat <<'EOF'
[Journal]
Storage=persistent
SystemMaxUse=100G
SystemMaxFileSize=256M
SystemMaxFiles=1024
MaxFileSec=5min
MaxRetentionSec=1hour
EOF
`)
	writeExecutable("awk", `#!/bin/sh
case "$*" in
  *'/proc/uptime'*)
    case "$*" in *1000000*) echo 100000000000 ;; *) echo 100000 ;; esac
    exit 0
    ;;
esac
exec /usr/bin/awk "$@"
`)
	writeExecutable("timeout", `#!/bin/sh
shift
exec "$@"
`)
	writeExecutable("journalctl", `#!/bin/sh
case " $* " in
  *' --list-boots '*)
    echo ' 0 redacted Fri 2026-09-04 19:45:54 UTC—Fri 2026-09-04 20:45:20 UTC'
    exit 0
    ;;
esac
mode=$JOURNAL_LATEST_MODE
age=1
case " $* " in
  *' --until '*) mode=$JOURNAL_BOUNDARY_MODE; age=3300 ;;
esac
now=$(date +%s)
timestamp=$(( (now - age) * 1000000 ))
case "$mode" in
  string) printf '{"__REALTIME_TIMESTAMP":"%s"}\n' "$timestamp" ;;
  numeric) printf '{"__REALTIME_TIMESTAMP":%s}\n' "$timestamp" ;;
  future) printf '{"__REALTIME_TIMESTAMP":"%s"}\n' "$(( (now + 1) * 1000000 ))" ;;
  young) printf '{"__REALTIME_TIMESTAMP":"%s"}\n' "$(( (now - 2999) * 1000000 ))" ;;
  empty) : ;;
  missing) echo '{"MESSAGE":"private-journal-detail"}' ;;
  duplicate) printf '{"__REALTIME_TIMESTAMP":"%s","__REALTIME_TIMESTAMP":"%s"}\n' "$timestamp" "$timestamp" ;;
  malformed) echo 'private-journal-detail' ;;
  error) echo 'private-journal-detail' >&2; exit 1 ;;
  *) exit 2 ;;
esac
`)

	command := exec.Command("sh", "-c", journalBufferCommand)
	command.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"JOURNAL_LATEST_MODE="+latestMode,
		"JOURNAL_BOUNDARY_MODE="+boundaryMode,
	)
	output, err := command.CombinedOutput()
	return string(output), err
}
