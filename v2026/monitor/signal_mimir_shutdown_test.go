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
)

func mimirShutdownFixture(port int, flush bool) string {
	return fmt.Sprintf("instance_begin %d\nflush %t\ninstance_end\nmimir_count 1\n", port, flush)
}

func runMimirShutdownSynthetic(t *testing.T, fixtures map[string]string) []Alert {
	t.Helper()
	var commands atomic.Int64
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		commands.Add(1)
		if !strings.Contains(command, mimirShutdownMarker) || !strings.Contains(command, "/config") {
			return "", fmt.Errorf("unexpected Mimir shutdown command on %s: %s", host.Name, command)
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
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-only", Roles: []string{"pg-primary"}})

	alerts, err := NewMimirShutdownSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if commands.Load() != int64(len(fixtures)) {
		t.Fatalf("Mimir shutdown commands = %d, want %d services hosts", commands.Load(), len(fixtures))
	}
	return alerts
}

func TestMimirShutdownSignalHealthySyntheticFleet(t *testing.T) {
	alerts := runMimirShutdownSynthetic(t, map[string]string{
		"edge-0": mimirShutdownFixture(14819, true),
		"edge-1": mimirShutdownFixture(14818, true),
	})
	if len(alerts) != 0 {
		t.Fatalf("healthy Mimir shutdown configuration alerted: %+v", alerts)
	}
}

func TestMimirShutdownSignalAggregatesDisabledFleet(t *testing.T) {
	alerts := runMimirShutdownSynthetic(t, map[string]string{
		"crisp": mimirShutdownFixture(14818, false),
		"edge-0": "instance_begin 14818\nflush true\ninstance_end\n" +
			"instance_begin 14819\nflush false\ninstance_end\nmimir_count 2\n",
		"edge-1": mimirShutdownFixture(14819, true),
	})
	if len(alerts) != 1 {
		t.Fatalf("disabled Mimir shutdown configuration produced %d alerts, want 1: %+v", len(alerts), alerts)
	}
	alert := alerts[0]
	if alert.Class != "mimir-shutdown-flush-disabled" || alert.Target != "mimir-fleet" ||
		alert.SignalNumber != "11.21" || alert.SignalKey != "mimir-shutdown" || alert.Sustain != 1 {
		t.Fatalf("wrong Mimir shutdown alert identity: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"crisp:14818=false", "edge-0:14819=false", "ephemeral",
		"mimir_instances=4 disabled_instances=2",
		"7176ccd", "§8.13", "§11.20", "Historical Mimir gaps are unrecoverable",
		"never leave the host", "controlled Grafana rollout", "120-second",
		"3,600-second", "60-second timeout stops only the Warpctl controller",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("Mimir shutdown alert lacks %q:\n%s", want, markdown)
		}
	}
}

func TestMimirShutdownSignalSyntheticMissingChild(t *testing.T) {
	alerts := runMimirShutdownSynthetic(t, map[string]string{
		"edge-0": "mimir_count 0\n",
	})
	if len(alerts) != 1 {
		t.Fatalf("missing Mimir child produced %d alerts, want 1: %+v", len(alerts), alerts)
	}
	alert := alerts[0]
	if alert.Class != "mimir-shutdown-child-missing" || alert.Target != "edge-0" || alert.Sustain != 2 {
		t.Fatalf("wrong missing-child alert: %+v", alert)
	}
}

func TestMimirShutdownSignalPreservesDisabledResultThroughObservationLoss(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "unreadable" {
			return "", fmt.Errorf("synthetic SSH loss")
		}
		return mimirShutdownFixture(14819, false), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "edge-0", Roles: []string{"services"}},
		{Name: "unreadable", Roles: []string{"services"}},
	}
	alerts, err := NewMimirShutdownSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]Alert{}
	for _, alert := range alerts {
		classes[alert.Class] = alert
	}
	if _, ok := classes["mimir-shutdown-flush-disabled"]; !ok {
		t.Fatalf("confirmed disabled setting was discarded: %+v", alerts)
	}
	if visibility, ok := classes["cannot-observe"]; !ok || visibility.Target != "unreadable/mimir-shutdown" {
		t.Fatalf("observation loss was discarded: %+v", alerts)
	}
}

func TestParseMimirShutdownHostSampleRejectsMalformedFraming(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		needle string
	}{
		{"count mismatch", "instance_begin 14819\nflush true\ninstance_end\nmimir_count 2\n", "count=2 instances=1"},
		{"missing field", "instance_begin 14819\ninstance_end\nmimir_count 1\n", "omitted flush"},
		{"invalid boolean", "instance_begin 14819\nflush yes\ninstance_end\nmimir_count 1\n", "invalid flush"},
		{"unknown output", "rendered_secret value\nmimir_count 0\n", "unknown field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseMimirShutdownHostSample(test.input)
			if err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("parse error = %v, want %q", err, test.needle)
			}
		})
	}
}

func TestMimirShutdownScriptEmitsOnlySelectedField(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("ss", `#!/bin/sh
printf '%s\n' \
  'LISTEN 0 4096 127.0.0.1:14788 0.0.0.0:*' \
  'LISTEN 0 4096 127.0.0.1:14819 0.0.0.0:*'
`)
	writeExecutable("curl", `#!/bin/sh
case "${*}" in
  *':14819/config')
    printf '%s\n' \
      'blocks_storage:' \
      '  s3:' \
      '    secret_access_key: should-never-leave-host' \
      '  tsdb:' \
      '    flush_blocks_on_shutdown: false'
    ;;
  *':14788/config')
    printf '%s\n' \
      'auth_enabled: false' \
      'storage_config:' \
      '  boltdb_shipper: {}'
    ;;
  *) exit 2 ;;
esac
`)

	command := exec.Command("sh", "-c", mimirShutdownScript)
	command.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Mimir shutdown script: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "should-never-leave-host") || strings.Contains(string(output), "auth_enabled") {
		t.Fatalf("Mimir shutdown script leaked rendered configuration:\n%s", output)
	}
	sample, err := parseMimirShutdownHostSample(string(output))
	if err != nil {
		t.Fatalf("parse Mimir shutdown script output: %v\n%s", err, output)
	}
	if sample.count != 1 || len(sample.instances) != 1 || sample.instances[0].port != 14819 || sample.instances[0].flush {
		t.Fatalf("Mimir shutdown script sample = %+v\n%s", sample, output)
	}
}
