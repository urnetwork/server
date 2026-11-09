package main

// check-burn's contract with warp_rollout_gate: exit 0 only for clear; burning, unknown, settings that do
// not load and a check that fails are each an error, so the step stops.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	servermonitor "github.com/urnetwork/server/monitor"
)

// A check answering state with lines, or err.
func fixedCheckBurn(state string, err error, lines ...string) checkBurnFunction {
	return func(ctx context.Context, settings servermonitor.SignalSettings, service string) (*servermonitor.SloBurnCheck, error) {
		if err != nil {
			return nil, err
		}
		return &servermonitor.SloBurnCheck{Service: service, State: state, Lines: lines}, nil
	}
}

// Settings that load.
func loadedCheckBurnSettings() (servermonitor.SignalSettings, error) {
	return servermonitor.SignalSettings{Environment: "synthetic"}, nil
}

func TestCheckBurnExitsZeroOnlyWhenClear(t *testing.T) {
	stdout := &bytes.Buffer{}
	if err := runCheckBurnCommand(t.Context(), []string{"--service=evs"}, stdout, loadedCheckBurnSettings, fixedCheckBurn(servermonitor.SloBurnClear, nil, "availability: clear")); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if want := "evs availability: clear\nevs: clear\n"; stdout.String() != want {
		t.Errorf("output %q, want %q", stdout, want)
	}
	for _, state := range []string{servermonitor.SloBurnBurning, servermonitor.SloBurnUnknown} {
		stdout := &bytes.Buffer{}
		err := runCheckBurnCommand(t.Context(), []string{"--service=evs"}, stdout, loadedCheckBurnSettings, fixedCheckBurn(state, nil))
		if err == nil || !strings.Contains(err.Error(), state) || !strings.Contains(stdout.String(), "evs: "+state) {
			t.Errorf("%s: %v, %q", state, err, stdout)
		}
	}
}

func TestCheckBurnFailsClosed(t *testing.T) {
	broken := func() (servermonitor.SignalSettings, error) {
		return servermonitor.SignalSettings{}, errors.New("no monitor.yml")
	}
	if err := runCheckBurnCommand(t.Context(), []string{"--service=evs"}, &bytes.Buffer{}, broken, fixedCheckBurn(servermonitor.SloBurnClear, nil)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("settings that do not load: %v", err)
	}
	if err := runCheckBurnCommand(t.Context(), []string{"--service=evs"}, &bytes.Buffer{}, loadedCheckBurnSettings, fixedCheckBurn("", errors.New("no env"))); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("a failed check: %v", err)
	}
	for _, args := range [][]string{{}, {"--service="}, {"--service=evs", "extra"}, {"--nope"}} {
		if err := runCheckBurnCommand(t.Context(), args, &bytes.Buffer{}, loadedCheckBurnSettings, fixedCheckBurn(servermonitor.SloBurnClear, nil)); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%q: %v", args, err)
		}
	}
}
