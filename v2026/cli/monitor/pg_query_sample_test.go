package main

import (
	"bytes"
	servermonitor "github.com/urnetwork/server/v2026/monitor"
	"strings"
	"testing"
)

func TestPgQuerySampleCLIValidation(t *testing.T) {
	for _, args := range [][]string{{"-min-probe-cadence", "15m", "-pg-query-sample-until", "bad-clock"}, {"-pg-query-sample-until", "2026-10-01T20:00:00Z", "-list-signals"}, {"-min-probe-cadence", "15m", "-pg-query-sample-until", "2026-10-01T20:00:00Z", "-exclude-signal", "pg-query-sample"}} {
		loaded := false
		err := runWithSettingsLoader(args, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
			loaded = true
			return servermonitor.SignalSettings{}, nil
		})
		if err == nil || loaded {
			t.Fatal("invalid optional sampler arguments reached settings/contact")
		}
		if strings.Contains(err.Error(), "bad-clock") {
			t.Fatal("arbitrary option copied into error")
		}
	}
}

func TestPgQuerySampleContinuousCLI(t *testing.T) {
	for _, args := range [][]string{
		{"-pg-query-sample-continuous"},
		{"-pg-query-sample-continuous", "-once", "-min-probe-cadence", "15m"},
		{"-pg-query-sample-continuous", "-list-signals", "-min-probe-cadence", "15m"},
		{"-pg-query-sample-continuous", "-pg-query-sample-until", "2026-10-03T00:00:00Z", "-min-probe-cadence", "15m"},
		{"-pg-query-sample-continuous", "-min-probe-cadence", "15m", "-exclude-signal", "pg-query-sample"},
	} {
		loaded := false
		err := runWithSettingsLoader(args, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
			loaded = true
			return servermonitor.SignalSettings{}, nil
		})
		if err == nil || loaded {
			t.Fatal("invalid recurring configuration reached settings")
		}
	}
	opts, err := parseMonitorOptions([]string{"-pg-query-sample-continuous", "-min-probe-cadence", "15m"})
	if err != nil || !opts.pgQuerySampleContinuous || opts.pgQuerySampleUntil != "" {
		t.Fatal("continuous mode not selected")
	}
}
