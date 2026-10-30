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

func TestMonitorInvocationWitnessOptionsAreExplicitAndDistinct(t *testing.T) {
	for _, args := range [][]string{
		{"-once", "-invocation-witness-output", "w"},
		{"-list-signals", "-invocation-witness-output", "w"},
		{"-output", "w", "-invocation-witness-output", "w"},
		{"-settings-witness-output", "w", "-invocation-witness-output", "w"},
		{"-url-probe-coverage-output", "w", "-invocation-witness-output", "w"},
		{"-provider-selection-output", "w", "-invocation-witness-output", "w"},
	} {
		if _, err := parseMonitorOptions(args); err == nil {
			t.Fatal("ambiguous/noncontinuous invocation output accepted")
		}
	}
	opts, err := parseMonitorOptions([]string{"-invocation-witness-output", "w"})
	if err != nil || opts.invocationWitnessOutput != "w" {
		t.Fatal("explicit continuous option lost")
	}
}

// Real CLI startup passes its selected signals and post-override settings to
// the protected constructor. Refusal returns before any candidate loop starts.
func TestMonitorInvocationWitnessStartupRefusalPrecedesLoop(t *testing.T) {
	loads, configured := 0, 0
	refused := errors.New("synthetic-startup-refused")
	loader := func() (servermonitor.SignalSettings, error) {
		loads++
		return servermonitor.SignalSettings{Environment: "synthetic", Source: &urlProbeCoverageNoContactSource{}}, nil
	}
	constructor := func(_ context.Context, path string, settings servermonitor.SignalSettings, signals []servermonitor.Signal, diagnostics io.Writer) (*servermonitor.RunInvocationWitness, error) {
		configured++
		if path == "" || settings.Environment != "synthetic" || len(signals) != 1 || signals[0].Key() != "settings-freshness" || diagnostics == nil {
			t.Error("CLI lost selected/startup binding")
		}
		return nil, refused
	}
	err := runWithInvocationWitness([]string{"-include-signal", "settings-freshness", "-skip-tier-zero-vitals", "-invocation-witness-output", filepath.Join(t.TempDir(), "w.jsonl")}, &bytes.Buffer{}, loader, nil, nil, constructor)
	if !errors.Is(err, refused) || loads != 1 || configured != 1 {
		t.Fatal("failed startup loaded again or entered loop")
	}
}

func TestMonitorInvocationWitnessDisabledDoesNotConfigureOrLoad(t *testing.T) {
	configured, loads := 0, 0
	loader := func() (servermonitor.SignalSettings, error) {
		loads++
		return servermonitor.SignalSettings{}, errors.New("must not load")
	}
	constructor := func(context.Context, string, servermonitor.SignalSettings, []servermonitor.Signal, io.Writer) (*servermonitor.RunInvocationWitness, error) {
		configured++
		return nil, errors.New("must not configure")
	}
	if err := runWithInvocationWitness([]string{"-list-signals"}, &bytes.Buffer{}, loader, nil, nil, constructor); err != nil || configured != 0 || loads != 0 {
		t.Fatal("disabled listing performed witness/startup I/O")
	}
}
