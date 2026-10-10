//go:build acklineagetrace

package perfvar

import (
	"context"
	"os"
	"runtime"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Diagnostic arms differ in a source-attested private Go overlay, not a
// runtime feature or scenario setting. The resolver's ordinary workload,
// warmup, deadlines, and payload remain identical within each pair.
func h1EndedProbeGuardScenario(t testing.TB, profile string) perfvarScenario {
	t.Helper()
	if profile != "cell-edge-256k-down-64k-up" && profile != "clean-lan" {
		t.Fatalf("unsupported ended-probe profile %q", profile)
	}
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "exchange-h1", "CONNECT_PERFVAR_PROFILE": profile,
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
		t.Fatalf("ended-probe scenario count=%d err=%v", len(scenarios), err)
	}
	scenario := scenarios[0]
	hash, err := scenario.hash()
	if err != nil {
		t.Fatal(err)
	}
	trace, err := perfvarTraceForRun(scenario, 1)
	if err != nil {
		t.Fatal(err)
	}
	if scenario.RunCount != 5 || scenario.ApplicationMtu != 1100 || len(scenario.Features) != 0 {
		t.Fatal("ended-probe arm changed an unpaired workload setting")
	}
	if profile == "cell-edge-256k-down-64k-up" && (scenario.PayloadByteCount != 65536 ||
		hash != "b0ad123f921465cc398b6a5ef20a0d07c15d6f4852eeca96c8739e38c3693495" ||
		trace.IdentityHash != "48ecee778157849ca495331a0ab8f8989736a60dbf781eb8d9ebda2215f8e042") {
		t.Fatal("original record25 identity changed")
	}
	if profile == "clean-lan" && (scenario.PayloadByteCount != 32*1024*1024 ||
		hash != "3172de64918539dc5a58455c2594ecd7d78995d3f1e9855317dacbcc44110daa" ||
		trace.IdentityHash != "620e6990b15ce3c0fedffc51adf9254e3b15b0604f26b69a5e24ce31d265ce69") {
		t.Fatal("canonical clean-lan identity changed")
	}
	t.Logf("[h1-ended-probe-identity] profile=%s scenario=%s trace=%+v payload=%d run_index=1 original_run_count=5 baseline_eligible=false", profile, hash, trace, scenario.PayloadByteCount)
	return scenario
}

func TestH1EndedProbeGuardIdentity(t *testing.T) {
	for _, profile := range []string{"cell-edge-256k-down-64k-up", "clean-lan"} {
		t.Run(profile, func(t *testing.T) { h1EndedProbeGuardScenario(t, profile) })
	}
}

func TestH1EndedProbeGuardReplay(t *testing.T) {
	profile := os.Getenv("CONNECT_PERFVAR_H1_ENDED_PROBE_GUARD_PROFILE")
	if profile == "" {
		t.Skip("explicit source-pinned ended-probe comparison required")
	}
	arm := os.Getenv("URNETWORK_H1_ENDED_PROBE_GUARD_ARM")
	if arm != "control" && arm != "candidate" || os.Getenv("URNETWORK_H1_LOADED_DOWNLOAD_REPLAY_SLOT") != "granted" {
		t.Fatal("ended-probe comparison requires an attested arm and parent-owned slot")
	}
	if runtime.GOMAXPROCS(0) != 8 || !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0)) ||
		perfvarProgressTraceEnabled() || clientconnect.DefaultLogger().V(1).Enabled() ||
		newPerfvarProgressTraceForTest != nil || newFullTunLatencyProbeObserverForTest != nil || newFullTunConstructionHooksForTest != nil {
		t.Fatal("ended-probe comparison requires original CPU8 runtime, V0 and no observer")
	}
	scenario := h1EndedProbeGuardScenario(t, profile)
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), perfvarRunTimeout(scenario))
		defer cancel()
		record, err := measurePerfvarRun(ctx, t, scenario, 1)
		record.Host.MeasurementKind = "diagnostic-ended-probe-guard-" + arm
		emitPerfvarRecord(t, record)
		if err != nil {
			t.Fatal(err)
		}
		if !record.Correct || record.InvalidReason != "" {
			t.Errorf("ended-probe %s correct=%t invalid=%s failure=%s", arm, record.Correct, record.InvalidReason, record.FailureReason)
		}
	})
}
