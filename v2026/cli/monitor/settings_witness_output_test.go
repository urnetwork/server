// Synthetic CLI witness controls prove startup wiring without running native
// process identity discovery or creating an authoritative watcher.
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	servermonitor "github.com/urnetwork/server/v2026/monitor"
)

// Count one owner close after the command joins its signal invocations.
type settingsWitnessTestCloser struct{ calls int }

// Record deterministic command-owned release without opening a real stream.
func (self *settingsWitnessTestCloser) Close() error { self.calls++; return nil }

// Ambiguous output and absent freshness selection refuse before settings load.
func TestMonitorSettingsWitnessOptionsRequireDistinctOutputAndFreshness(t *testing.T) {
	for _, args := range [][]string{
		{"-list-signals", "-settings-witness-output", "synthetic.jsonl"},
		{"-once", "-output", "synthetic.jsonl", "-settings-witness-output", "synthetic.jsonl"},
		{"-url-probe-coverage-output", "synthetic.jsonl", "-settings-witness-output", "synthetic.jsonl"},
		{"-provider-selection-output", "synthetic.jsonl", "-settings-witness-output", "synthetic.jsonl"},
	} {
		if _, err := parseMonitorOptions(args); err == nil {
			t.Fatal("ambiguous settings witness output was accepted")
		}
	}
	loads := 0
	err := runWithSettingsLoader([]string{"-once", "-include-signal", "url-probe-coverage", "-skip-tier-zero-vitals", "-settings-witness-output", "synthetic.jsonl"}, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
		loads++
		return servermonitor.SignalSettings{}, errors.New("synthetic loader must not run")
	})
	if err == nil || loads != 0 {
		t.Fatal("missing freshness selection loaded settings or accepted output")
	}
}

// The witness binds the post-override startup without adding a loader call.
func TestMonitorSettingsWitnessWiringUsesPostOverrideStartupAndClosesOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.jsonl")
	loads, configured := 0, 0
	owner := &settingsWitnessTestCloser{}
	loader := func() (servermonitor.SignalSettings, error) {
		loads++
		return servermonitor.SignalSettings{Environment: "synthetic", Source: &urlProbeCoverageNoContactSource{}}, nil
	}
	admission := func(context.Context, servermonitor.SignalSettings, string, bool) (servermonitor.SignalSettings, error) {
		t.Fatal("unconfigured admission was invoked")
		return servermonitor.SignalSettings{}, nil
	}
	configure := func(ctx context.Context, startup servermonitor.SignalSettings, destination string) (servermonitor.SignalSettings, io.Closer, error) {
		configured++
		if ctx.Err() != nil || destination != path || startup.AddressMode != servermonitor.AddressModeOverlay || startup.SettingsGenerationCheck == nil {
			t.Fatal("witness did not receive the actual effective startup and callback")
		}
		return startup, owner, nil
	}
	err := runWithSettingsAdmissionAndWitness([]string{"-once", "-mode", "overlay", "-include-signal", "settings-freshness", "-skip-tier-zero-vitals", "-settings-witness-output", path}, &bytes.Buffer{}, loader, admission, configure)
	if err != nil || loads != 2 || configured != 1 || owner.calls != 1 {
		t.Fatal("witness wiring added a load or lost its output owner")
	}
}

// Failed initialization cannot dispatch a freshness invocation or leak an owner.
func TestMonitorSettingsWitnessInitializationFailureDoesNotRunFreshness(t *testing.T) {
	loads := 0
	loader := func() (servermonitor.SignalSettings, error) {
		loads++
		return servermonitor.SignalSettings{Environment: "synthetic", Source: &urlProbeCoverageNoContactSource{}}, nil
	}
	configure := func(context.Context, servermonitor.SignalSettings, string) (servermonitor.SignalSettings, io.Closer, error) {
		return servermonitor.SignalSettings{}, nil, errors.New("synthetic initialization refused")
	}
	err := runWithSettingsAdmissionAndWitness([]string{"-once", "-include-signal", "settings-freshness", "-skip-tier-zero-vitals", "-settings-witness-output", filepath.Join(t.TempDir(), "synthetic.jsonl")}, &bytes.Buffer{}, loader, nil, configure)
	if err == nil || loads != 1 {
		t.Fatal("failed witness initialization ran an extra callback")
	}
}
