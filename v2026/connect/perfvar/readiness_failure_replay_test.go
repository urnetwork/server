//go:build acklineagetrace

package perfvar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Record 36 failed during the first, exchange-backed discovery echo, not on
// the selected P2P data plane. Preserve its original five-run scenario hash;
// choosing RunCount=1 silently changes that identity.
func readinessFailureRun1Scenario(t testing.TB) perfvarScenario {
	t.Helper()
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "p2p-legacy", "CONNECT_PERFVAR_PROFILE": "cell-edge-256k-down-64k-up",
		"CONNECT_PERFVAR_WORKLOAD": "tcp", "CONNECT_PERFVAR_DIRECTION": "download",
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
	if err != nil || hash != "43df3a7fb026e4a979b5d544baa93ee178942893bde03c1c3207115d56405048" {
		t.Fatalf("readiness scenario changed: %s %v", hash, err)
	}
	trace, err := perfvarTraceForRun(scenario, 1)
	want := perfvarTrace{Version: 1, RunIndex: 1, IdentityHash: "175838c270792fda68eb955a495d89790818b965b9989a3f24486e5bcce0db51", ApplicationOrDirectSeed: 2840024661566869501, ProviderSeed: 758656184240495006, InternalSeed: 5187342190360445809}
	if err != nil || trace != want || scenario.RunCount != 5 || scenario.PayloadByteCount != 65536 || scenario.ApplicationMtu != 1100 {
		t.Fatalf("readiness workload/trace changed: scenario=%+v trace=%+v err=%v", scenario, trace, err)
	}
	t.Logf("[readiness-failure-identity] run_index=1 original_run_count=5 scenario=%s trace=%+v baseline_eligible=false", hash, trace)
	return scenario
}

func TestReadinessFailureRun1Identity(t *testing.T) { readinessFailureRun1Scenario(t) }

// Only the echo's application reads are counted. No packet observer, deadline,
// retry, or production queue is changed. The registry is bounded independently
// of Tun's raced-dial count and flags incomplete evidence explicitly.
type readinessFailureReader struct {
	connection net.Conn
	bytes      atomic.Int64
	calls      atomic.Uint64
	errors     atomic.Uint64
}

func (self *readinessFailureReader) Read(buffer []byte) (int, error) {
	n, err := self.connection.Read(buffer)
	self.bytes.Add(int64(n))
	self.calls.Add(1)
	if err != nil {
		self.errors.Add(1)
	}
	return n, err
}

type readinessFailureReadSnapshot struct {
	Connection string
	Bytes      int64
	Calls      uint64
	Errors     uint64
}

type readinessFailureInnerSnapshot struct {
	TCP         h1FailureTCPSnapshot
	Application h1FailureTCPStackCounters
	Reads       []readinessFailureReadSnapshot
	Overflow    uint64
	Probe       uint64
}

type readinessFailureRecorder struct {
	base     *h1FailureSnapshotRecorder
	innerTCP h1FailureTCPRecorder
	mutex    sync.Mutex
	readers  []*readinessFailureReader
	overflow atomic.Uint64
	probe    atomic.Uint64
	inner    atomic.Pointer[readinessFailureInnerSnapshot]
}

func newReadinessFailureRecorder() *readinessFailureRecorder {
	self := &readinessFailureRecorder{base: newH1FailureSnapshotRecorder()}
	// The existing typed, one-shot ACK-lifetime logger calls stack only after
	// attributing the exact live flow. Snapshot the inner connection at that
	// same owner boundary, before terminal callbacks remove the exit.
	self.base.stack = func(buffer []byte, all bool) int {
		n := runtime.Stack(buffer, all)
		snapshot := &readinessFailureInnerSnapshot{TCP: self.innerTCP.snapshot(nil), Overflow: self.overflow.Load(), Probe: self.probe.Load()}
		if path := self.base.path.Load(); path != nil {
			snapshot.Application = h1FailureTCPStats(path.appTun)
		}
		self.mutex.Lock()
		for _, reader := range self.readers {
			snapshot.Reads = append(snapshot.Reads, readinessFailureReadSnapshot{Connection: fmt.Sprintf("%p", reader.connection), Bytes: reader.bytes.Load(), Calls: reader.calls.Load(), Errors: reader.errors.Load()})
		}
		self.mutex.Unlock()
		self.inner.Store(snapshot)
		return n
	}
	return self
}

func (self *readinessFailureRecorder) reader(connection net.Conn) io.Reader {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if len(self.readers) >= 16 {
		self.overflow.Add(1)
		return connection
	}
	reader := &readinessFailureReader{connection: connection}
	self.readers = append(self.readers, reader)
	return reader
}

func (self *readinessFailureRecorder) hooks() *fullTunConstructionTestHooks {
	hooks := self.base.hooks()
	after := hooks.afterStage
	hooks.afterStage = func(stage fullTunConstructionStage, path *fullTunPath) error {
		if stage == fullTunConstructionStageRouteReady {
			// This recorder is exclusively construction evidence. The existing
			// H1 loaded-flow recorder keeps its stronger workload gate intact.
			self.base.ready.Store(false)
			return nil
		}
		if err := after(stage, path); err != nil {
			return err
		}
		if stage == fullTunConstructionStageBridge {
			self.base.path.Store(path)
			path.readinessRequestReaderForTest = self.reader
			path.readinessClientConnectionForTest = func(connection net.Conn) {
				if address, ok := connection.RemoteAddr().(*net.TCPAddr); ok {
					self.innerTCP.observeDial("application", connection, address.Port)
				} else {
					self.innerTCP.unsupported.Add(1)
				}
			}
			path.beforeReadinessClientWriteForTest = func() {
				self.probe.Add(1)
				self.base.refreshPin(path, "discovery-echo-write")
				self.base.ready.Store(true)
			}
		}
		return nil
	}
	return hooks
}

func (self *readinessFailureRecorder) dump(t testing.TB) {
	t.Helper()
	snapshot, inner := self.base.snapshot.Load(), self.inner.Load()
	complete := !self.base.claimed.Load() || snapshot != nil && inner != nil &&
		!snapshot.StackTruncated && !snapshot.TransportSearchTruncated && !snapshot.TCP.Incomplete && !inner.TCP.Incomplete && inner.Overflow == 0
	t.Logf("[readiness-failure-summary] captured=%t complete=%t malformed=%d unattributed=%d rejected=%d probes=%d observer_violations=%d baseline_eligible=false", snapshot != nil, complete, self.base.malformed.Load(), self.base.unattributed.Load(), self.base.rejected.Load(), self.probe.Load(), self.base.observerViolations.Load())
	if snapshot != nil {
		metadata, err := json.Marshal(struct {
			Outer *h1FailureSnapshot
			Inner *readinessFailureInnerSnapshot
		}{snapshot, inner})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("[readiness-failure-snapshot] %s", metadata)
		t.Logf("[readiness-failure-stack] bytes=%d truncated=%t\n%s\n[readiness-failure-stack-end]", snapshot.StackBytes, snapshot.StackTruncated, snapshot.stack)
	}
	if !complete || self.base.malformed.Load() != 0 || self.base.unattributed.Load() != 0 || self.base.observerViolations.Load() != 0 || self.probe.Load() == 0 {
		t.Error("readiness failure evidence incomplete or not armed")
	}
}

func readinessFailureCaptureMode(value string) (bool, error) {
	switch value {
	case "none":
		return false, nil
	case "terminal":
		return true, nil
	default:
		return false, fmt.Errorf("unsupported readiness capture mode %q", value)
	}
}

type readinessFailureTestConn struct {
	net.Conn
	reader io.Reader
}

func (self *readinessFailureTestConn) Read(buffer []byte) (int, error) {
	return self.reader.Read(buffer)
}

func TestReadinessFailureObserverBoundsAndReadIdentity(t *testing.T) {
	recorder := newReadinessFailureRecorder()
	payload := []byte("an unchanged readiness request")
	connection := &readinessFailureTestConn{reader: bytes.NewReader(payload)}
	reader := recorder.reader(connection)
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, payload) || recorder.readers[0].bytes.Load() != int64(len(payload)) || recorder.readers[0].errors.Load() != 1 {
		t.Fatalf("read observer altered content/result: got=%q err=%v", got, err)
	}
	for range 20 {
		recorder.reader(connection)
	}
	if len(recorder.readers) != 16 || recorder.overflow.Load() != 5 {
		t.Fatal("echo reader registry not bounded")
	}
	for _, value := range []string{"none", "terminal", "", "typo"} {
		capture, err := readinessFailureCaptureMode(value)
		if capture != (value == "terminal") || (err == nil) != (value == "none" || value == "terminal") {
			t.Fatalf("capture mode %q silently changed: %t %v", value, capture, err)
		}
	}
}

func TestReadinessFailureRun1Replay(t *testing.T) {
	const selector = "CONNECT_PERFVAR_READINESS_FAILURE_REPLAY"
	if os.Getenv(selector) == "" {
		t.Skip("explicit one-cell readiness diagnostic required")
	}
	if os.Getenv(selector) != "p2p-legacy-run1" || os.Getenv("URNETWORK_ACK_LINEAGE_REPLAY_SLOT") != "granted" {
		t.Fatal("readiness replay requires exact selector and a parent-owned host/source slot")
	}
	if !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0)) {
		t.Fatal("readiness diagnostic requires the pinned Go/platform and an explicit two- or eight-core limit")
	}
	if perfvarProgressTraceEnabled() || clientconnect.DefaultLogger().V(1).Enabled() || newPerfvarProgressTraceForTest != nil || newFullTunConstructionHooksForTest != nil {
		t.Fatal("readiness replay requires V0 and unowned diagnostic hooks")
	}
	capture, err := readinessFailureCaptureMode(os.Getenv("CONNECT_PERFVAR_READINESS_CAPTURE"))
	if err != nil {
		t.Fatal(err)
	}
	// Two cores is a bounded diagnostic departure from record36's eight. The
	// original profile/trace remains exact; the CPU setting is never hidden.
	t.Logf("[readiness-failure-mode] capture=%t go_max_procs=%d original_go_max_procs=8 per_packet_observers=false baseline_eligible=false", capture, runtime.GOMAXPROCS(0))
	scenario := readinessFailureRun1Scenario(t)
	defer func() { newFullTunConstructionHooksForTest = nil }()
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		var recorder *readinessFailureRecorder
		if capture {
			recorder = newReadinessFailureRecorder()
			newFullTunConstructionHooksForTest = recorder.hooks
		}
		ctx, cancel := context.WithTimeout(context.Background(), perfvarRunTimeout(scenario))
		record, err := measurePerfvarRun(ctx, t, scenario, 1)
		cancel()
		if recorder != nil {
			recorder.dump(t)
		}
		if err != nil {
			t.Fatal(err)
		}
		record.Host.MeasurementKind = "diagnostic-readiness-failure-snapshot"
		emitPerfvarRecord(t, record)
		if !record.Correct {
			t.Errorf("readiness diagnostic reproduced failure: %s: %s", record.FailureStage, record.FailureReason)
		}
	})
}
