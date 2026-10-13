//go:build acklineagetrace

package perfvar

import "testing"

// Preserve the first incorrect round-02 static control's resolver identity.
// Selecting RunCount=1 would change the trace even though only run 1 executes.
func h1Static1MLoadedDownloadRun1Scenario(t testing.TB) perfvarScenario {
	t.Helper()
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "exchange-h1", "CONNECT_PERFVAR_PROFILE": "cell-edge-1m-down-250k-up",
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
		t.Fatalf("H1 static 1m scenario count=%d err=%v", len(scenarios), err)
	}
	scenario := scenarios[0]
	hash, err := scenario.hash()
	if err != nil || hash != "7261c32e05be5f5361bc72ec03be471f9c0f67023d3c7bbb844b9e5943a6a08d" {
		t.Fatalf("H1 static 1m scenario changed: %s %v", hash, err)
	}
	profile, err := scenario.profilesHash()
	if err != nil || profile != "581284ba3d969056e028082f06cf8b15e52713a886914dd5c5178a53c9a079e0" {
		t.Fatalf("H1 static 1m profile changed: %s %v", profile, err)
	}
	trace, err := perfvarTraceForRun(scenario, 1)
	want := perfvarTrace{
		Version: 1, RunIndex: 1, IdentityHash: "a565140a59886195535e682fb5211bdb76a61db3e36c2777e41df56a4e63be34",
		ApplicationOrDirectSeed: 1436704778824836438, ProviderSeed: 4610805270373888261, InternalSeed: 5581971181408079923,
	}
	if err != nil || trace != want || scenario.RunCount != 5 || scenario.PayloadByteCount != 262144 || scenario.ApplicationMtu != 1100 {
		t.Fatalf("H1 static 1m workload/trace changed: runs=%d bytes=%d mtu=%d trace=%+v err=%v", scenario.RunCount, scenario.PayloadByteCount, scenario.ApplicationMtu, trace, err)
	}
	t.Logf("[h1-static-1m-identity] run_index=1 original_run_count=5 scenario=%s profile=%s trace=%+v baseline_eligible=false", hash, profile, trace)
	return scenario
}

func TestH1Static1MLoadedDownloadRun1Identity(t *testing.T) {
	h1Static1MLoadedDownloadRun1Scenario(t)
}

func TestH1Static1MLoadedDownloadRun1Replay(t *testing.T) {
	runH1LoadedDownloadRun1Replay(t, h1Static1MLoadedDownloadRun1Scenario)
}

func TestH1StaticCleanLoadedDownloadRun1Replay(t *testing.T) {
	runH1LoadedDownloadRun1Replay(t, func(t testing.TB) perfvarScenario {
		return h1EndedProbeGuardScenario(t, "clean-lan")
	})
}
