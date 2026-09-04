package monitor

import (
	"context"
	"strings"
	"testing"
)

func journalBufferFixture(overrides map[string]string) string {
	keys := []string{
		"observation_schema", "journald_active", "max_use", "max_file_size", "max_files",
		"max_retention", "uptime_seconds", "boundary_checked", "boundary_present",
		"boundary_oldest_seconds", "boundary_newest_seconds",
	}
	values := map[string]string{
		"observation_schema": "1", "journald_active": "active", "max_use": "100G",
		"max_file_size": "256M", "max_files": "1024", "max_retention": "1hour", "uptime_seconds": "7200",
		"boundary_checked": "1", "boundary_present": "1",
		"boundary_oldest_seconds": "3300", "boundary_newest_seconds": "3000",
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
		"healthy":  journalBufferFixture(nil),
		"drift":    journalBufferFixture(map[string]string{"max_retention": "7day"}),
		"short":    journalBufferFixture(map[string]string{"boundary_present": "0"}),
		"inactive": journalBufferFixture(map[string]string{"journald_active": "failed"}),
		"young": journalBufferFixture(map[string]string{
			"uptime_seconds": "1200", "boundary_checked": "0", "boundary_present": "0",
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
		return journalBufferFixture(map[string]string{"boundary_present": "secret\nunknown=value"}), nil
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

func TestJournalBufferCommandIsBoundedAndUsesEffectiveConfig(t *testing.T) {
	for _, want := range []string{
		"systemd-analyze cat-config systemd/journald.conf",
		"--since '55 minutes ago' --until '50 minutes ago' -n 1",
		"SystemMaxUse", "SystemMaxFileSize", "SystemMaxFiles", "MaxRetentionSec",
	} {
		if !strings.Contains(journalBufferCommand, want) {
			t.Errorf("command lacks %q", want)
		}
	}
	for _, forbidden := range []string{"sudo", "journalctl -b", "--since '7 days ago'"} {
		if strings.Contains(journalBufferCommand, forbidden) {
			t.Errorf("command contains unsafe/unbounded boundary %q", forbidden)
		}
	}
}
