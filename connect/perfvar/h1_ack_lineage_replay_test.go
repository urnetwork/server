//go:build acklineagetrace

package perfvar

import (
	"context"
	"os"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Preserve the original static scenario and run2 impairment identity. This
// one-cell diagnostic cannot stand in for the canonical five-run cohort.
func h1AckLineageRun2Scenario(t testing.TB) perfvarScenario {
	t.Helper()
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "exchange-h1", "CONNECT_PERFVAR_PROFILE": "cell-edge-256k-down-64k-up",
		"CONNECT_PERFVAR_WORKLOAD": "latency-under-load", "CONNECT_PERFVAR_DIRECTION": "upload",
		"CONNECT_PERFVAR_RESOURCE": "mobile-surrogate", "CONNECT_PERFVAR_TOPOLOGY": "one-hop",
		"CONNECT_PERFVAR_RUN_COUNT": "5", "CONNECT_PERFVAR_SEED": "20260810", "CONNECT_PERFVAR_EXTENDERS": "0",
	}
	config, err := loadPerfvarConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil || len(scenarios) != 1 {
		t.Fatalf("scenario count=%d err=%v", len(scenarios), err)
	}
	scenario := scenarios[0]
	hash, err := scenario.hash()
	if err != nil || hash != "fc7e6e72016846af4e88b02f9883245a305a03a1a3788f019aab7f47511f1606" {
		t.Fatalf("H1 scenario changed: %s %v", hash, err)
	}
	profile, err := scenario.profilesHash()
	if err != nil || profile != "c99c85d26dbff8130d621cdd9d62efbf3b73f96429c6698efa75977d17e7f000" {
		t.Fatalf("H1 profile changed: %s %v", profile, err)
	}
	trace, err := perfvarTraceForRun(scenario, 2)
	want := perfvarTrace{Version: 1, RunIndex: 2, IdentityHash: "c7d7a6212749b94bfa6123a5ed3a13e875d3af9cd8bce25fabee07edf2272e76", ApplicationOrDirectSeed: 8823794618536023859, ProviderSeed: 3860618382366092155, InternalSeed: 8115607585746458005}
	if err != nil || trace != want || scenario.RunCount != 5 || scenario.PayloadByteCount != 65536 {
		t.Fatalf("H1 workload/trace changed: runs=%d bytes=%d trace=%+v err=%v", scenario.RunCount, scenario.PayloadByteCount, trace, err)
	}
	t.Logf("[h1-lineage-identity] selection=h1-run2 run_index=2 original_run_count=5 payload_bytes=65536 scenario=%s profile=%s trace=%+v baseline_eligible=false", hash, profile, trace)
	return scenario
}

func TestH1AckLineageRun2Identity(t *testing.T) { h1AckLineageRun2Scenario(t) }

func h1AckLineageRun1ThenRun2Scenario(t testing.TB) perfvarScenario {
	t.Helper()
	scenario := h1AckLineageRun2Scenario(t)
	trace, err := perfvarTraceForRun(scenario, 1)
	want := perfvarTrace{Version: 1, RunIndex: 1, IdentityHash: "8035bc136a53d0403731a81796ebb7da9813cb4cf5c3ed9e235771a69ea455c1", ApplicationOrDirectSeed: 4179692972341828224, ProviderSeed: 1934060821706129428, InternalSeed: 7071147093947374187}
	if err != nil || trace != want {
		t.Fatalf("H1 preceding run1 identity changed: trace=%+v err=%v", trace, err)
	}
	t.Logf("[h1-lineage-identity] selection=h1-run1-run2 order=1,2 shared_process=true preceding_trace=%+v baseline_eligible=false", trace)
	return scenario
}

func h1AckLineageReplayRunIndices(selection string) []int {
	switch selection {
	case "h1-run2":
		return []int{2}
	case "h1-run1-run2":
		return []int{1, 2}
	default:
		return nil
	}
}

func TestH1AckLineageRun1ThenRun2Identity(t *testing.T) {
	h1AckLineageRun1ThenRun2Scenario(t)
	if !slices.Equal(h1AckLineageReplayRunIndices("h1-run1-run2"), []int{1, 2}) ||
		!slices.Equal(h1AckLineageReplayRunIndices("h1-run2"), []int{2}) ||
		h1AckLineageReplayRunIndices("h1-run2-run1") != nil ||
		h1AckLineageReplayRunIndices("") != nil {
		t.Fatal("H1 diagnostic process ordering changed")
	}
}

// Reuse the lossless first-event recorder. Physical subtype/read summaries and
// unsupported joins are explicit, so no missing/truncated event is mistaken
// for a dropped packet. Only fixed metadata is retained by either observer.
func newH1AckReplayTrace(runIndex int) *perfvarProgressTrace {
	trace := newAckReplayTrace()
	observe, dump := trace.observeForTest, trace.dumpForTest
	var framed, websocket, reads, heartbeatReads, readErrors, unsupported atomic.Uint64
	trace.observeForTest = func(event clientconnect.TransferProgressEvent) {
		switch event.Stage {
		case "h1_connected":
			if event.Outcome == "h1plus" {
				framed.Add(1)
			} else if event.Outcome == "websocket" {
				websocket.Add(1)
			} else {
				unsupported.Add(1)
			}
		case "h1_read":
			reads.Add(1)
			if event.ByteCount == 0 {
				heartbeatReads.Add(1)
			}
		case "h1_read_error", "h1_read_deadline_error":
			readErrors.Add(1)
		case "h1_trace_overflow", "h1_trace_unsupported":
			unsupported.Add(1)
		}
		observe(event)
	}
	trace.dumpForTest = func(t testing.TB, role string) {
		t.Logf("[h1-lineage-run] run_index=%d role=%s baseline_eligible=false", runIndex, role)
		dump(t, role)
		t.Logf("[h1-lineage-physical] run_index=%d role=%s h1plus_connections=%d websocket_connections=%d reads=%d heartbeat_reads=%d read_errors=%d unsupported=%d baseline_eligible=false", runIndex, role, framed.Load(), websocket.Load(), reads.Load(), heartbeatReads.Load(), readErrors.Load(), unsupported.Load())
		if framed.Load()+websocket.Load() == 0 || unsupported.Load() != 0 {
			t.Errorf("H1 physical evidence incomplete: role=%s connections=%d unsupported=%d", role, framed.Load()+websocket.Load(), unsupported.Load())
		}
	}
	return trace
}

func TestH1AckLineageRun2Replay(t *testing.T) {
	testH1AckLineageReplay(t, "h1-run2")
}

// The reproduced stress failure followed run1 in the same process. Preserve
// that ordering and one TestEnv lifetime; every cell still creates its own
// bounded recorders and uses the canonical per-run timeout and teardown.
func TestH1AckLineageRun1ThenRun2Replay(t *testing.T) {
	testH1AckLineageReplay(t, "h1-run1-run2")
}

func testH1AckLineageReplay(t *testing.T, selection string) {
	t.Helper()
	if os.Getenv("CONNECT_PERFVAR_ACK_LINEAGE_REPLAY") == "" {
		t.Skip("explicit H1 diagnostic selector required")
	}
	if os.Getenv("CONNECT_PERFVAR_ACK_LINEAGE_REPLAY") != selection || os.Getenv("URNETWORK_ACK_LINEAGE_REPLAY_SLOT") != "granted" {
		t.Fatalf("H1 diagnostic requires %s and a parent-owned host/source slot", selection)
	}
	if runtime.GOMAXPROCS(0) != 8 || !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0)) {
		t.Fatalf("H1 diagnostic runtime changed: Go=%s CPU=%d platform=%s/%s", runtime.Version(), runtime.GOMAXPROCS(0), runtime.GOOS, runtime.GOARCH)
	}
	// Fixed metadata observes the same boundaries without per-message V(1)
	// logging. Keep verbosity visible so quiet and verbose replays cannot be
	// accidentally treated as the same diagnostic cohort.
	if os.Getenv("CONNECT_PERFVAR_PROGRESS_TRACE") != "1" {
		t.Fatal("H1 diagnostic requires CONNECT_PERFVAR_PROGRESS_TRACE=1")
	}
	t.Logf("[h1-lineage-runtime] verbose_v1=%t baseline_eligible=false", clientconnect.DefaultLogger().V(1).Enabled())
	indices := h1AckLineageReplayRunIndices(selection)
	if len(indices) == 0 {
		t.Fatal("unsupported H1 diagnostic process order")
	}
	var scenario perfvarScenario
	if selection == "h1-run1-run2" {
		scenario = h1AckLineageRun1ThenRun2Scenario(t)
	} else {
		scenario = h1AckLineageRun2Scenario(t)
	}
	defer func() { newPerfvarProgressTraceForTest = nil }()
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		for _, runIndex := range indices {
			newPerfvarProgressTraceForTest = func() *perfvarProgressTrace { return newH1AckReplayTrace(runIndex) }
			ctx, cancel := context.WithTimeout(context.Background(), perfvarRunTimeout(scenario))
			record, err := measurePerfvarRun(ctx, t, scenario, runIndex)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			emitPerfvarRecord(t, record)
			if !record.Correct {
				t.Errorf("H1 diagnostic reproduced failure: run=%d %s", runIndex, record.FailureReason)
			}
			if record.InvalidReason != "" {
				t.Errorf("H1 diagnostic retained invalid flag: %s", record.InvalidReason)
			}
		}
	})
}
