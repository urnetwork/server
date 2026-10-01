package main

import (
	"bytes"
	servermonitor "github.com/urnetwork/server/monitor"
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
