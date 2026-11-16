//go:build acklineagetrace && h1memorytrace && darwin && cgo

package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

var h1MemoryHookLock sync.Mutex
var h1MemoryHookOwner *h1MemoryPhaseOwner

type h1MemoryPhaseOwner struct {
	mu      sync.RWMutex
	closed  bool
	sampler *h1MemorySampler
}

func (owner *h1MemoryPhaseOwner) mark(phase h1MemoryPhase) {
	owner.mu.RLock()
	defer owner.mu.RUnlock()
	if !owner.closed {
		owner.sampler.mark(phase)
	}
}

func h1MemoryProbePhase(name string) (h1MemoryPhase, bool) {
	switch name {
	case "idle-start":
		return h1MemoryIdle, true
	case "idle-end":
		return h1MemoryBulkSetup, true
	case "loaded-start":
		return h1MemoryLoad, true
	case "loaded-end":
		return h1MemoryBulkJoin, true
	case "post-load-start":
		return h1MemoryPostLoad, true
	case "post-load-end":
		return h1MemoryPostJoin, true
	default:
		return 0, false
	}
}

func installH1MemoryPhases(sampler *h1MemorySampler) (func(), error) {
	h1MemoryHookLock.Lock()
	defer h1MemoryHookLock.Unlock()
	if h1MemoryHookOwner != nil || newFullTunLatencyProbeObserverForTest != nil || newFullTunConstructionHooksForTest != nil {
		return nil, errors.New("memory-phase-hook-owned")
	}
	owner := &h1MemoryPhaseOwner{sampler: sampler}
	h1MemoryHookOwner = owner
	newFullTunConstructionHooksForTest = func() *fullTunConstructionTestHooks {
		owner.mark(h1MemoryRouteConstruction)
		return nil
	}
	newFullTunLatencyProbeObserverForTest = func(*fullTunPath) *fullTunLatencyProbeTestObserver {
		owner.mark(h1MemoryWorkloadSetup)
		return &fullTunLatencyProbeTestObserver{phase: func(name string) { phase, _ := h1MemoryProbePhase(name); owner.mark(phase) }, finish: func() { owner.mark(h1MemoryRouteTeardown) }}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			h1MemoryHookLock.Lock()
			defer h1MemoryHookLock.Unlock()
			owner.mu.Lock()
			defer owner.mu.Unlock()
			owner.closed = true
			if h1MemoryHookOwner == owner {
				newFullTunLatencyProbeObserverForTest = nil
				newFullTunConstructionHooksForTest = nil
				h1MemoryHookOwner = nil
			}
		})
	}, nil
}

// This is a separate memory diagnostic. It always emits the unchanged
// schema14 record, including headroom invalidity. No PERF gate is waived.
func TestH1CleanHighResolutionMemoryDiagnostic(t *testing.T) {
	mode := os.Getenv("URNETWORK_H1_MEMORY_MODE")
	if mode == "" {
		t.Skip("explicit source-pinned memory diagnostic required")
	}
	arm := os.Getenv("URNETWORK_H1_ENDED_PROBE_GUARD_ARM")
	if mode != "on" && mode != "off" || arm != "control" && arm != "candidate" || os.Getenv("URNETWORK_H1_LOADED_DOWNLOAD_REPLAY_SLOT") != "granted" {
		t.Fatal("invalid memory diagnostic ownership or arm")
	}
	if runtime.GOMAXPROCS(0) != 8 || !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0)) || perfvarRaceEnabled || perfvarProgressTraceEnabled() || clientconnect.DefaultLogger().V(1).Enabled() || newPerfvarProgressTraceForTest != nil {
		t.Fatal("memory diagnostic requires original CPU8 non-race runtime, V0 and no packet observers")
	}
	scenario := h1EndedProbeGuardScenario(t, "clean-lan")
	var sampler *h1MemorySampler
	if mode == "on" {
		sampler = newH1MemorySampler(h1MemoryReader())
	}
	defer sampler.stop()
	cleanup, err := installH1MemoryPhases(sampler)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	var record perfvarRunRecord
	environment.Run(t, func(t testing.TB) {
		defer sampler.mark(h1MemoryFixtureTeardown)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		var err error
		record, err = measurePerfvarRun(ctx, t, scenario, 1)
		record.Host.MeasurementKind = "diagnostic-clean-memory-" + arm + "-" + mode
		emitPerfvarRecord(t, record)
		if err != nil {
			t.Error(err)
		}
	})
	cleanup()
	reading := sampler.stop()
	b, err := json.Marshal(struct {
		Kind             string          `json:"kind"`
		Arm              string          `json:"arm"`
		Mode             string          `json:"mode"`
		BaselineEligible bool            `json:"baseline_eligible"`
		CanonicalCorrect bool            `json:"canonical_correct"`
		CanonicalInvalid string          `json:"canonical_invalid_reason"`
		Reading          h1MemoryReading `json:"reading"`
	}{"clean32m-memory-v1", arm, mode, false, record.Correct, record.InvalidReason, reading})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("[h1-memory] %s", b)
	if !record.Correct || record.InvalidReason != "" && record.InvalidReason != perfvarHeadroomReason {
		t.Errorf("memory workload failed: correct=%t invalid=%s", record.Correct, record.InvalidReason)
	}
	if mode == "on" && !reading.Valid {
		t.Errorf("memory sampling invalid: %s", reading.Failure)
	}
}

func TestH1MemoryPhaseOwnershipAndPrivacy(t *testing.T) {
	cleanup, err := installH1MemoryPhases(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if other, err := installH1MemoryPhases(nil); err == nil || other != nil {
		t.Fatal("competing phase owner admitted")
	}
	stale := newFullTunLatencyProbeObserverForTest(nil)
	cleanup()
	if newFullTunLatencyProbeObserverForTest != nil || newFullTunConstructionHooksForTest != nil {
		t.Fatal("phase factories leaked")
	}
	newCleanup, err := installH1MemoryPhases(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer newCleanup()
	cleanup()
	stale.phase("private-address-or-credential")
	stale.finish()
	if h1MemoryHookOwner == nil || newFullTunLatencyProbeObserverForTest == nil {
		t.Fatal("stale cleanup removed new owner")
	}
	if phase, ok := h1MemoryProbePhase("private-address-or-credential"); ok || phase.name() != "invalid" {
		t.Fatal("private/raw phase escaped allowlist")
	}
}
