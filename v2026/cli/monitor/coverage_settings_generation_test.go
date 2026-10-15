package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server/v2026"
	servermonitor "github.com/urnetwork/server/v2026/monitor"
)

// Exercise the actual CLI callback installation and freshness signal together.
// A disabled synthetic coverage source prevents any network or service contact.
func TestMonitorCoverageOutputPreservesSettingsGeneration(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "changed"
		}
		t.Run(name, func(t *testing.T) {
			pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte("enabled: false"))
			defer pop()
			var loads atomic.Int32
			loader := func() (servermonitor.SignalSettings, error) {
				settings := servermonitor.SignalSettings{Environment: "synthetic", Source: &urlProbeCoverageNoContactSource{}}
				if loads.Add(1) > 1 && changed {
					settings.PublicDomain = "changed.example.test"
				}
				return settings, nil
			}
			path := filepath.Join(t.TempDir(), "coverage.jsonl")
			var alerts bytes.Buffer
			err := runWithSettingsLoader([]string{"-once", "-include-signal", "settings-freshness", "-include-signal", "url-probe-coverage", "-url-probe-coverage-output", path}, &alerts, loader)
			if err != nil || loads.Load() < 2 {
				t.Fatal("CLI did not compare its freshly loaded generation")
			}
			if strings.Contains(alerts.String(), "settings-generation-stale") != changed || strings.Contains(alerts.String(), "settings-generation-unobservable") {
				t.Fatal("CLI output callback changed the settings-generation result")
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var observation servermonitor.UrlProbeCoverageObservation
			if json.Unmarshal(bytes.TrimSpace(contents), &observation) != nil || observation.CensusReason != "workflow_disabled" {
				t.Fatal("generation comparison lost the independent coverage observation")
			}
		})
	}
}
