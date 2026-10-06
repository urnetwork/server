// Coverage records must be opt-in alongside the existing alert stream.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	servermonitor "github.com/urnetwork/server/monitor"
)

// The standing watcher needs a destination even when coverage has no alert.
func TestMonitorAcceptsUrlProbeCoverageOutput(t *testing.T) {
	if _, err := parseMonitorOptions([]string{"-url-probe-coverage-output", filepath.Join(t.TempDir(), "coverage.jsonl")}); err != nil {
		t.Fatalf("continuous coverage output cannot be armed: %v", err)
	}
}

// The CLI appends disabled and unavailable samples without contacting any source.
func TestMonitorUrlProbeCoverageOutputAppendsWithoutAlerts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.jsonl")
	loader := func() (servermonitor.SignalSettings, error) {
		return servermonitor.SignalSettings{Environment: "synthetic"}, nil
	}
	for _, configuration := range []string{"enabled: false", "enabled: true"} {
		pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte(configuration))
		err := runWithSettingsLoader([]string{"-once", "-include-signal", "url-probe-coverage", "-url-probe-coverage-output", path}, &bytes.Buffer{}, loader)
		pop()
		if err != nil {
			t.Fatal(err)
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) != 2 {
		t.Fatalf("observations were dropped or overwritten: %d", len(lines))
	}
	for index, reason := range []string{"workflow_disabled", "desired_configuration_unavailable"} {
		var record servermonitor.UrlProbeCoverageObservation
		if err := json.Unmarshal([]byte(lines[index]), &record); err != nil {
			t.Fatal(err)
		}
		if record.CensusReason != reason || record.AllCurrent != nil || record.KnownMatureQuotaPercent != nil {
			t.Fatalf("unavailable data became healthy: %+v", record)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("coverage output was not created privately")
	}
}

// Invalid signal selection and conflicting destinations fail before settings or contact.
func TestMonitorUrlProbeCoverageOutputRequiresSelectedSignal(t *testing.T) {
	for _, args := range [][]string{
		{"-list-signals", "-url-probe-coverage-output", "unused.jsonl"},
		{"-once", "-output", "same.jsonl", "-url-probe-coverage-output", "same.jsonl"},
		{"-exclude-signal", "url-probe-coverage", "-url-probe-coverage-output", "unused.jsonl"},
	} {
		err := runWithSettingsLoader(args, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
			t.Fatal("invalid coverage output reached settings")
			return servermonitor.SignalSettings{}, nil
		})
		if err == nil {
			t.Fatalf("invalid coverage output accepted: %v", args)
		}
	}
}
