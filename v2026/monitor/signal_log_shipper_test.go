package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func logShipperFixture(overrides map[string]string) string {
	keys := []string{"observation_schema", "active_state", "sub_state", "result", "restarts", "nofile_hard", "nofile_soft"}
	values := map[string]string{
		"observation_schema": "1", "active_state": "active", "sub_state": "running",
		"result": "success", "restarts": "0", "nofile_hard": "65536", "nofile_soft": "65536",
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

func TestLogShipperSignalSyntheticProblemClassesAndHostScope(t *testing.T) {
	observations := map[string]string{
		"healthy": logShipperFixture(nil),
		"down":    logShipperFixture(map[string]string{"active_state": "failed", "sub_state": "failed", "result": "exit-code"}),
		"low-fd":  logShipperFixture(map[string]string{"nofile_soft": "1024"}),
		"churn":   logShipperFixture(map[string]string{"restarts": "5"}),
		"pg":      logShipperFixture(nil), "redis": logShipperFixture(nil),
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
	if len(alerts) != 3 {
		t.Fatalf("alerts=%d, want 3: %+v", len(alerts), alerts)
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
	for _, target := range []string{"healthy", "down", "low-fd", "churn", "pg", "redis", "backup", "subtensor"} {
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

func TestLogShipperCommandReadsBothFDLimitsWithoutJournalScan(t *testing.T) {
	for _, want := range []string{"systemctl show fluent-bit.service", "LimitNOFILE", "LimitNOFILESoft", "NRestarts"} {
		if !strings.Contains(logShipperCommand, want) {
			t.Errorf("command lacks %q", want)
		}
	}
	for _, forbidden := range []string{"journalctl", "sudo", "docker"} {
		if strings.Contains(logShipperCommand, forbidden) {
			t.Errorf("command contains unrelated boundary %q", forbidden)
		}
	}
}
