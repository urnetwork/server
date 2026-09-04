package monitor

import (
	"context"
	"strings"
	"testing"
)

func journalBufferFixture(overrides map[string]string) string {
	keys := []string{
		"observation_schema", "journald_active", "journald_active_seconds", "storage",
		"max_use", "max_file_size", "max_files", "max_file_sec", "max_retention",
		"uptime_seconds", "coverage_checked", "coverage_present",
		"oldest_entry_age_seconds", "coverage_target_seconds",
	}
	values := map[string]string{
		"observation_schema": "2", "journald_active": "active", "journald_active_seconds": "7200",
		"storage": "persistent", "max_use": "100G", "max_file_size": "256M", "max_files": "1024",
		"max_file_sec": "5min", "max_retention": "1hour", "uptime_seconds": "7200",
		"coverage_checked": "1", "coverage_present": "1",
		"oldest_entry_age_seconds": "3300", "coverage_target_seconds": "3000",
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
			"coverage_present": "0", "oldest_entry_age_seconds": "1200",
		}),
		"inactive": journalBufferFixture(map[string]string{"journald_active": "failed"}),
		"young": journalBufferFixture(map[string]string{
			"uptime_seconds": "1200", "journald_active_seconds": "1200",
			"coverage_checked": "0", "coverage_present": "0", "oldest_entry_age_seconds": "1200",
		}),
		"recent-journal-restart": journalBufferFixture(map[string]string{
			"journald_active_seconds": "600", "coverage_checked": "0",
			"coverage_present": "0", "oldest_entry_age_seconds": "600",
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

func TestJournalBufferSignalSyntheticLowVolumeWholeFileLoss(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return journalBufferFixture(map[string]string{
			"storage": "auto", "max_file_sec": "1month",
			"coverage_present": "0", "oldest_entry_age_seconds": "900",
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
		"journalctl -q --list-boots -n 1 -o json",
		"ActiveEnterTimestampMonotonic",
		"Storage", "SystemMaxUse", "SystemMaxFileSize", "SystemMaxFiles", "MaxFileSec", "MaxRetentionSec",
	} {
		if !strings.Contains(journalBufferCommand, want) {
			t.Errorf("command lacks %q", want)
		}
	}
	for _, forbidden := range []string{"sudo", "journalctl -b", "--since", "--until"} {
		if strings.Contains(journalBufferCommand, forbidden) {
			t.Errorf("command contains unsafe/unbounded boundary %q", forbidden)
		}
	}
}
