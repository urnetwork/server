// Cadence policy belongs to the continuous watcher, not one-shot diagnostics.
package main

import (
	"bytes"
	"testing"
	"time"

	servermonitor "github.com/urnetwork/server/v2026/monitor"
)

// Zero/default behavior stays compatible; explicit durations are per invocation.
func TestParseMonitorOptionsAcceptsProbeCadenceFloor(t *testing.T) {
	for _, fixture := range []struct {
		args     []string
		expected time.Duration
	}{
		{args: nil, expected: 0},
		{args: []string{"-min-probe-cadence=15m"}, expected: 15 * time.Minute},
		{args: []string{"-min-probe-cadence", "30m"}, expected: 30 * time.Minute},
		{args: []string{"-once", "-min-probe-cadence=0"}, expected: 0},
		{args: []string{"-list-signals", "-min-probe-cadence=0"}, expected: 0},
	} {
		opts, err := parseMonitorOptions(fixture.args)
		if err != nil || opts.minimumProbeCadence != fixture.expected {
			t.Fatalf("cadence options %v = %s, %v", fixture.args, opts.minimumProbeCadence, err)
		}
	}
}

// Invalid or irrelevant timing policy never reaches settings or the environment.
func TestRunRejectsInvalidProbeCadenceBeforeSettings(t *testing.T) {
	for _, args := range [][]string{
		{"-min-probe-cadence=-1m"},
		{"-min-probe-cadence=15"},
		{"-min-probe-cadence=invalid"},
		{"-min-probe-cadence=999999999999999h"},
		{"-once", "-min-probe-cadence=15m"},
		{"-list-signals", "-min-probe-cadence=15m"},
	} {
		var calls int
		err := runWithSettingsLoader(args, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
			calls++
			return servermonitor.SignalSettings{}, nil
		})
		if err == nil || calls != 0 {
			t.Fatalf("invalid cadence %v error=%v settings calls=%d", args, err, calls)
		}
	}
}
