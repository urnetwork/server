//go:build acklineagetrace

package perfvar

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

const ackReplayCapacity = 16384

// First events are never overwritten. Producers reserve disjoint slots and
// publish them atomically, without locks, allocation, I/O or payload retention.
// Dump uses a finite reserved prefix; unpublished slots invalidate the evidence.
// This allocation is diagnostic overhead and cannot qualify mobile memory.
type ackReplayRecorder struct {
	next      atomic.Uint64
	events    [ackReplayCapacity]clientconnect.TransferProgressEvent
	published [ackReplayCapacity]atomic.Bool
}

func (self *ackReplayRecorder) observe(event clientconnect.TransferProgressEvent) {
	index := self.next.Add(1) - 1
	if index < ackReplayCapacity {
		self.events[index] = event
		self.published[index].Store(true)
	}
}

func (self *ackReplayRecorder) counts() (reserved, overflow, pending uint64) {
	reserved = self.next.Load()
	if reserved > ackReplayCapacity {
		overflow = reserved - ackReplayCapacity
	}
	for index := uint64(0); index < min(reserved, ackReplayCapacity); index++ {
		if !self.published[index].Load() {
			pending++
		}
	}
	return
}

func (self *ackReplayRecorder) dump(t testing.TB, role string) {
	reserved, overflow, pending := self.counts()
	header, _ := json.Marshal(map[string]any{
		"kind": "ack-lineage-header", "role": role, "capacity": ackReplayCapacity,
		"reserved_prefix": reserved, "overflow": overflow, "unpublished": pending,
		"first_events": true, "baseline_eligible": false, "recorder_bytes": unsafe.Sizeof(*self),
	})
	t.Logf("[ack-lineage] %s", header)
	for index := uint64(0); index < min(reserved, ackReplayCapacity); index++ {
		if !self.published[index].Load() {
			continue
		}
		event := self.events[index]
		row, _ := json.Marshal(map[string]any{
			"kind": "ack-lineage-event", "role": role, "ordinal": index, "stage": event.Stage,
			"at_unix_nano": event.AtUnixNano, "elapsed_nanos": event.ElapsedNanos,
			"client": progressTraceIdentity(event.ClientId), "peer": progressTraceIdentity(event.PeerId),
			"sequence": progressTraceIdentity(event.SequenceId), "message": progressTraceIdentity(event.MessageId),
			"number": event.SequenceNumber, "wire_hash": fmt.Sprintf("%016x", event.WireHash),
			"bytes": event.ByteCount, "transport": event.TransportType, "no_ack": event.NoAck,
			"selective": event.Selective, "success": event.Success, "error_kind": event.ErrorKind, "outcome": event.Outcome,
		})
		t.Logf("[ack-lineage] %s", row)
	}
	if overflow != 0 || pending != 0 {
		t.Errorf("ACK lineage evidence incomplete: role=%s overflow=%d unpublished=%d", role, overflow, pending)
	}
}

func newAckReplayTrace() *perfvarProgressTrace {
	recorder := &ackReplayRecorder{}
	return &perfvarProgressTrace{observeForTest: recorder.observe, dumpForTest: recorder.dump}
}

func ackLineageLoaded4Scenario(t testing.TB) perfvarScenario {
	t.Helper()
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "p2p-legacy", "CONNECT_PERFVAR_PROFILE": "cell-edge-1m-down-250k-up",
		"CONNECT_PERFVAR_WORKLOAD": "latency-under-load", "CONNECT_PERFVAR_DIRECTION": "upload",
		"CONNECT_PERFVAR_RESOURCE": "mobile-surrogate", "CONNECT_PERFVAR_TOPOLOGY": "one-hop",
		"CONNECT_PERFVAR_RUN_COUNT": "5", "CONNECT_PERFVAR_SEED": "20260810",
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
	if err != nil || hash != "6a00980a2db8921a8893d6a71cc9bd9d988e4c2d947efa4907cf81670c7942a5" {
		t.Fatalf("scenario identity changed: %s %v", hash, err)
	}
	profile, err := scenario.profilesHash()
	if err != nil || profile != "581284ba3d969056e028082f06cf8b15e52713a886914dd5c5178a53c9a079e0" {
		t.Fatalf("profile identity changed: %s %v", profile, err)
	}
	trace, err := perfvarTraceForRun(scenario, 4)
	want := perfvarTrace{Version: 1, RunIndex: 4, IdentityHash: "ce10d67e35306350cdaba984f7ea547a07d508629c36d106e2eae58600f361b9", ApplicationOrDirectSeed: 5973921511112689967, ProviderSeed: 8944033135335427709, InternalSeed: 6290004958075753069}
	if err != nil || trace != want || scenario.RunCount != 5 || scenario.PayloadByteCount != 262144 {
		t.Fatalf("workload or trace identity changed: runs=%d bytes=%d trace=%+v err=%v", scenario.RunCount, scenario.PayloadByteCount, trace, err)
	}
	t.Logf("[ack-lineage-identity] source_record=221 source_line=9708 selection=loaded4 run_index=4 original_run_count=5 payload_bytes=262144 scenario=%s profile=%s trace=%+v baseline_eligible=false", hash, profile, trace)
	return scenario
}

func TestAckLineageLoaded4Identity(t *testing.T) { ackLineageLoaded4Scenario(t) }

func ackLineageReplayRuntimeSupported(goVersion, goos, goarch string, maxProcs int) bool {
	return goVersion == "go1.26.7" && goos == "darwin" && goarch == "arm64" && (maxProcs == 2 || maxProcs == 8)
}

func TestAckLineageReplayRuntimeGate(t *testing.T) {
	for _, test := range []struct {
		name      string
		goVersion string
		goos      string
		goarch    string
		maxProcs  int
		want      bool
	}{
		{"historical_cpu2", "go1.26.7", "darwin", "arm64", 2, true},
		{"canonical_cpu8", "go1.26.7", "darwin", "arm64", 8, true},
		{"unset_cpu", "go1.26.7", "darwin", "arm64", 0, false},
		{"cpu1", "go1.26.7", "darwin", "arm64", 1, false},
		{"cpu4", "go1.26.7", "darwin", "arm64", 4, false},
		{"host_cpu14", "go1.26.7", "darwin", "arm64", 14, false},
		{"go_version_drift", "go1.26.8", "darwin", "arm64", 8, false},
		{"platform_drift", "go1.26.7", "linux", "arm64", 8, false},
		{"architecture_drift", "go1.26.7", "darwin", "amd64", 8, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ackLineageReplayRuntimeSupported(test.goVersion, test.goos, test.goarch, test.maxProcs); got != test.want {
				t.Fatalf("runtime gate=%t want=%t", got, test.want)
			}
		})
	}
}

func TestAckLineageReplayRecorder(t *testing.T) {
	recorder := &ackReplayRecorder{}
	if got := testing.AllocsPerRun(100, func() { recorder.next.Store(0); recorder.observe(clientconnect.TransferProgressEvent{}) }); got != 0 {
		t.Fatalf("hot allocations=%g", got)
	}
	recorder = &ackReplayRecorder{}
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Go(func() {
			for index := range ackReplayCapacity / 4 {
				recorder.observe(clientconnect.TransferProgressEvent{SequenceNumber: uint64(worker*ackReplayCapacity/4 + index)})
			}
		})
	}
	workers.Wait()
	if reserved, overflow, pending := recorder.counts(); reserved != ackReplayCapacity || overflow != 0 || pending != 0 {
		t.Fatalf("reserved=%d overflow=%d pending=%d", reserved, overflow, pending)
	}
	var seen [ackReplayCapacity]bool
	for _, event := range recorder.events {
		if event.SequenceNumber >= ackReplayCapacity || seen[event.SequenceNumber] {
			t.Fatal("concurrent reservation lost identity")
		}
		seen[event.SequenceNumber] = true
	}
	recorder.observe(clientconnect.TransferProgressEvent{})
	if _, overflow, _ := recorder.counts(); overflow != 1 {
		t.Fatal("overflow not explicit")
	}
	recorder = &ackReplayRecorder{}
	recorder.next.Store(1)
	if _, _, pending := recorder.counts(); pending != 1 {
		t.Fatal("unpublished reservation not explicit")
	}
	settings := clientconnect.DefaultClientSettings()
	newAckReplayTrace().configure(settings)
	if settings.SendBufferSettings.ProgressObserver == nil || settings.ReceiveBufferSettings.ProgressObserver == nil || settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.ProgressObserver == nil {
		t.Fatal("observer did not reach every existing carrier/Transfer seam")
	}
	t.Logf("capacity_per_role=%d bytes_per_role=%d hot_allocations=0 payloads=0", ackReplayCapacity, unsafe.Sizeof(*recorder))
}

// This is an explicitly separate diagnostic selector. The canonical plan,
// workload, correctness, timeout and calibration gates remain unchanged.
func TestAckLineageLoaded4Replay(t *testing.T) {
	if os.Getenv("CONNECT_PERFVAR_ACK_LINEAGE_REPLAY") == "" {
		t.Skip("one-cell diagnostic requires an explicit selection")
	}
	if os.Getenv("CONNECT_PERFVAR_ACK_LINEAGE_REPLAY") != "loaded4" || os.Getenv("URNETWORK_ACK_LINEAGE_REPLAY_SLOT") != "granted" {
		t.Fatal("one-cell diagnostic requires loaded4 and a parent-owned host/source slot")
	}
	maxProcs := runtime.GOMAXPROCS(0)
	if !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, maxProcs) {
		t.Fatalf("diagnostic runtime changed: Go=%s GOMAXPROCS=%d platform=%s/%s", runtime.Version(), maxProcs, runtime.GOOS, runtime.GOARCH)
	}
	if os.Getenv("CONNECT_PERFVAR_PROGRESS_TRACE") != "1" || !clientconnect.DefaultLogger().V(1).Enabled() {
		t.Fatal("diagnostic requires CONNECT_PERFVAR_PROGRESS_TRACE=1 and -args -v=1")
	}
	scenario := ackLineageLoaded4Scenario(t)
	newPerfvarProgressTraceForTest = newAckReplayTrace
	defer func() { newPerfvarProgressTraceForTest = nil }()
	t.Logf("[ack-lineage-runtime] identity=loaded4-cpu%d-v1 original_GOMAXPROCS=8 diagnostic_GOMAXPROCS=%d baseline_eligible=false", maxProcs, maxProcs)
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), perfvarRunTimeout(scenario))
		defer cancel()
		record, err := measurePerfvarRun(ctx, t, scenario, 4)
		emitPerfvarRecord(t, record)
		if err != nil {
			t.Fatal(err)
		}
		if !record.Correct {
			t.Errorf("diagnostic reproduced failure: %s", record.FailureReason)
		}
		if record.InvalidReason != "" {
			t.Errorf("diagnostic retained invalid flag: %s", record.InvalidReason)
		}
	})
}
