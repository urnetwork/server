package main

// check-burn's contract with warp_rollout_gate: exit 0 only for clear; burning, unknown, settings that do
// not load and a check that fails are each an error, so the step stops.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// Clear exits 0 with each objective's line; burning and unknown are errors.
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

// Settings that do not load, a failed check and a wrong command line are each an error.
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

func TestCheckBurnRequiresTheReadyFirstEdgeAtTheReleaseVersionThroughoutTheCanary(t *testing.T) {
	const ready = `{"service":"dnsedge-1","block":"g1","version":"new","status":"ok"}`
	for _, tc := range []struct {
		label, start, end string
		wantChecks        int
		clear             bool
	}{
		{"ready", ready, ready, 1, true},
		{"old selector still serving", strings.Replace(ready, "new", "old", 1), ready, 0, false},
		{"new selector still starting", strings.Replace(ready, "ok", "error not ready", 1), ready, 0, false},
		{"wrong authority", strings.Replace(ready, "dnsedge-1", "dnsedge-2", 1), ready, 0, false},
		{"rolled back during canary", ready, strings.Replace(ready, "new", "old", 1), 1, false},
		{"drained during canary", ready, strings.Replace(ready, "ok", "draining", 1), 1, false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			checks := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/status" {
					http.NotFound(w, r)
					return
				}
				if checks == 0 {
					fmt.Fprint(w, tc.start)
				} else {
					fmt.Fprint(w, tc.end)
				}
			}))
			defer server.Close()
			check := func(ctx context.Context, settings servermonitor.SignalSettings, service string) (*servermonitor.SloBurnCheck, error) {
				checks++
				return fixedCheckBurn(servermonitor.SloBurnClear, nil)(ctx, settings, service)
			}
			err := runCheckBurnCommand(t.Context(), []string{"--service=dnsedge-1", "--expected-version=new", "--status-url=" + server.URL + "/status"}, &bytes.Buffer{}, loadedCheckBurnSettings, check)
			if (err == nil) != tc.clear || checks != tc.wantChecks {
				t.Fatalf("result %v, canary ran %d times", err, checks)
			}
		})
	}
}
