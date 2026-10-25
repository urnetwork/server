//go:build acklineagetrace

package perfvar

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Record25 completed the bulk download but accepted none of its 63 loaded
// probes. Preserve the canonical five-run scenario while executing only run1;
// changing RunCount or selecting the older upload replay changes its identity.
func h1LoadedDownloadRun1Scenario(t testing.TB) perfvarScenario {
	t.Helper()
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "exchange-h1", "CONNECT_PERFVAR_PROFILE": "cell-edge-256k-down-64k-up",
		"CONNECT_PERFVAR_WORKLOAD": "latency-under-load", "CONNECT_PERFVAR_DIRECTION": "download",
		"CONNECT_PERFVAR_RESOURCE": "mobile-surrogate", "CONNECT_PERFVAR_TOPOLOGY": "one-hop",
		"CONNECT_PERFVAR_RUN_COUNT": "5", "CONNECT_PERFVAR_SEED": "20260810", "CONNECT_PERFVAR_EXTENDERS": "0",
	}
	config, err := loadPerfvarConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil || len(scenarios) != 1 {
		t.Fatalf("H1 loaded download scenario count=%d err=%v", len(scenarios), err)
	}
	scenario := scenarios[0]
	hash, err := scenario.hash()
	if err != nil || hash != "b0ad123f921465cc398b6a5ef20a0d07c15d6f4852eeca96c8739e38c3693495" {
		t.Fatalf("H1 loaded download scenario changed: %s %v", hash, err)
	}
	profile, err := scenario.profilesHash()
	if err != nil || profile != "c99c85d26dbff8130d621cdd9d62efbf3b73f96429c6698efa75977d17e7f000" {
		t.Fatalf("H1 loaded download profile changed: %s %v", profile, err)
	}
	trace, err := perfvarTraceForRun(scenario, 1)
	want := perfvarTrace{
		Version: 1, RunIndex: 1, IdentityHash: "48ecee778157849ca495331a0ab8f8989736a60dbf781eb8d9ebda2215f8e042",
		ApplicationOrDirectSeed: 6034230609153212255, ProviderSeed: 8391793036920076727, InternalSeed: 5615796108803679309,
	}
	if err != nil || trace != want || scenario.RunCount != 5 || scenario.PayloadByteCount != 65536 || scenario.ApplicationMtu != 1100 {
		t.Fatalf("H1 loaded download workload/trace changed: runs=%d bytes=%d mtu=%d trace=%+v err=%v", scenario.RunCount, scenario.PayloadByteCount, scenario.ApplicationMtu, trace, err)
	}
	t.Logf("[h1-loaded-download-identity] source_record=25 run_index=1 original_run_count=5 scenario=%s profile=%s trace=%+v baseline_eligible=false", hash, profile, trace)
	return scenario
}

func TestH1LoadedDownloadRun1Identity(t *testing.T) { h1LoadedDownloadRun1Scenario(t) }

// An unobserved control and a probe-only diagnostic share exactly the same
// workload. Full carrier lineage is a separate opt-in because packet hashing
// has nonzero cost; it must never be silently attached to the quiet control.
func h1LoadedDownloadReplayMode(mode string) (probes, carrier bool, err error) {
	switch mode {
	case "control":
		return false, false, nil
	case "probes":
		return true, false, nil
	case "lineage", "transport":
		return true, true, nil
	default:
		return false, false, fmt.Errorf("unsupported H1 loaded download replay mode %q", mode)
	}
}

func TestH1LoadedDownloadReplayMode(t *testing.T) {
	for _, test := range []struct {
		mode            string
		probes, carrier bool
		valid           bool
	}{
		{"control", false, false, true},
		{"probes", true, false, true},
		{"lineage", true, true, true},
		{"transport", true, true, true},
		{"", false, false, false},
		{"CONTROL", false, false, false},
		{"control,lineage", false, false, false},
	} {
		probes, carrier, err := h1LoadedDownloadReplayMode(test.mode)
		if probes != test.probes || carrier != test.carrier || (err == nil) != test.valid {
			t.Fatalf("mode=%q got=%t/%t/%v", test.mode, probes, carrier, err)
		}
	}
}

func TestH1LoadedDownloadRun1Replay(t *testing.T) {
	runH1LoadedDownloadRun1Replay(t, h1LoadedDownloadRun1Scenario)
}

func runH1LoadedDownloadRun1Replay(t *testing.T, scenarioForTest func(testing.TB) perfvarScenario) {
	mode := os.Getenv("CONNECT_PERFVAR_H1_LOADED_DOWNLOAD_REPLAY")
	if mode == "" {
		t.Skip("explicit source-pinned H1 loaded download replay required")
	}
	probes, carrier, err := h1LoadedDownloadReplayMode(mode)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("URNETWORK_H1_LOADED_DOWNLOAD_REPLAY_SLOT") != "granted" {
		t.Fatal("H1 loaded download replay requires a parent-owned host/source slot")
	}
	if runtime.GOMAXPROCS(0) != 8 || !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0)) {
		t.Fatalf("H1 loaded download runtime changed: Go=%s CPU=%d platform=%s/%s", runtime.Version(), runtime.GOMAXPROCS(0), runtime.GOOS, runtime.GOARCH)
	}
	if perfvarProgressTraceEnabled() != carrier || clientconnect.DefaultLogger().V(1).Enabled() ||
		newPerfvarProgressTraceForTest != nil || newFullTunLatencyProbeObserverForTest != nil || newFullTunConstructionHooksForTest != nil {
		t.Fatal("H1 loaded download replay requires V0, exact progress-trace mode, and unowned hooks")
	}
	scenario := scenarioForTest(t)
	var transportRecorder *h1LoadedTransportRecorder
	if mode == "transport" {
		transportRecorder = &h1LoadedTransportRecorder{}
		cleanup, installed := clientconnect.InstallAckLineagePacingObserver(transportRecorder.claim, transportRecorder.observePacing)
		if !installed {
			t.Fatal("H1 pacing diagnostic is already owned")
		}
		transportRecorder.cleanup = append(transportRecorder.cleanup, cleanup)
		defer transportRecorder.finish(t)
	}
	if probes {
		newFullTunLatencyProbeObserverForTest = func(path *fullTunPath) *fullTunLatencyProbeTestObserver {
			observer := newProbeLineageObserver(t, path)
			if transportRecorder != nil {
				observer = transportRecorder.attach(t, path, observer)
			}
			return observer
		}
	}
	if carrier {
		newPerfvarProgressTraceForTest = func() *perfvarProgressTrace { return newH1AckReplayTrace(1) }
	}
	defer func() {
		newFullTunLatencyProbeObserverForTest = nil
		newPerfvarProgressTraceForTest = nil
	}()
	t.Logf("[h1-loaded-download-mode] mode=%s probe_observer=%t carrier_observer=%t original_go_max_procs=8 baseline_eligible=false", mode, probes, carrier)
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), perfvarRunTimeout(scenario))
		defer cancel()
		record, err := measurePerfvarRun(ctx, t, scenario, 1)
		record.Host.MeasurementKind = "diagnostic-h1-loaded-download-" + mode
		emitPerfvarRecord(t, record)
		if err != nil {
			t.Fatal(err)
		}
		if !record.Correct {
			t.Errorf("H1 loaded download reproduced failure: %s", record.FailureReason)
		}
		if record.InvalidReason != "" {
			t.Errorf("H1 loaded download retained invalid flag: %s", record.InvalidReason)
		}
	})
}
