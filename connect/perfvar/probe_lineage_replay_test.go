//go:build acklineagetrace

package perfvar

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

const probeLineageCapacity = 4096

// A diagnostic owns a bounded first prefix; publication never blocks a probe
// worker or retains a packet. Missing publication/overflow invalidates evidence.
type probeLineageRecorder struct {
	next      atomic.Uint64
	events    [probeLineageCapacity]latencyProbeObservation
	published [probeLineageCapacity]atomic.Bool
}

func (self *probeLineageRecorder) observe(event latencyProbeObservation) {
	index := self.next.Add(1) - 1
	if index < probeLineageCapacity {
		self.events[index] = event
		self.published[index].Store(true)
	}
}

func (self *probeLineageRecorder) counts() (reserved, overflow, unpublished uint64) {
	reserved = self.next.Load()
	if probeLineageCapacity < reserved {
		overflow = reserved - probeLineageCapacity
	}
	for index := uint64(0); index < min(reserved, probeLineageCapacity); index++ {
		if !self.published[index].Load() {
			unpublished++
		}
	}
	return
}

func probeLineageNanos(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func (self *probeLineageRecorder) dump(t testing.TB) {
	reserved, overflow, unpublished := self.counts()
	header, _ := json.Marshal(map[string]any{
		"kind": "probe-lineage-header", "scope": "full-tun-generated-echo",
		"capacity": probeLineageCapacity, "reserved_prefix": reserved,
		"overflow": overflow, "unpublished": unpublished,
		"first_events": true, "baseline_eligible": false, "recorder_bytes": unsafe.Sizeof(*self),
	})
	t.Logf("[probe-lineage] %s", header)
	for index := uint64(0); index < min(reserved, probeLineageCapacity); index++ {
		if !self.published[index].Load() {
			continue
		}
		event := self.events[index]
		// Strings preserve full nanosecond/sequence precision in JSON readers.
		row, _ := json.Marshal(struct {
			Kind         string `json:"kind"`
			Ordinal      uint64 `json:"ordinal"`
			Stage        string `json:"stage"`
			Sequence     uint64 `json:"sequence,string"`
			ObservedNano int64  `json:"observed_unix_nano,string"`
			SampleNano   int64  `json:"sample_unix_nano,string"`
			OfferedNano  int64  `json:"offered_unix_nano,string"`
			ErrorKind    string `json:"error_kind,omitempty"`
		}{
			Kind: "probe-lineage-event", Ordinal: index, Stage: event.Stage, Sequence: event.Sequence,
			ObservedNano: probeLineageNanos(event.ObservedTime), SampleNano: probeLineageNanos(event.SampleTime),
			OfferedNano: probeLineageNanos(event.OfferedTime), ErrorKind: event.ErrorKind,
		})
		t.Logf("[probe-lineage] %s", row)
	}
	if overflow != 0 || unpublished != 0 {
		t.Errorf("probe evidence incomplete: overflow=%d unpublished=%d", overflow, unpublished)
	}
}

type probeLineageNoAckCounts struct {
	Offered      uint64 `json:"offered"`
	Written      uint64 `json:"written"`
	Refused      uint64 `json:"refused"`
	Discarded    uint64 `json:"discarded"`
	FastPath     uint64 `json:"fast_path"`
	PackExpired  uint64 `json:"pack_expired"`
	ReceiveDrops uint64 `json:"receive_drops"`
}

func probeLineageNoAckSnapshot(client *clientconnect.Client) probeLineageNoAckCounts {
	if client == nil {
		return probeLineageNoAckCounts{}
	}
	stats := client.ReceiveStats()
	return probeLineageNoAckCounts{
		Offered: stats.SendNoAckOfferedCount, Written: stats.SendNoAckWriteCount,
		Refused: stats.SendNoAckRefusedCount, Discarded: stats.SendNoAckDiscardCount,
		FastPath: stats.SendNoAckFastPathWriteCount, PackExpired: stats.SendPackDeadlineDropCount,
		ReceiveDrops: stats.ReceiveQueueDropCount,
	}
}

type probeLineagePackFailureCounts struct {
	Started        uint64 `json:"started"`
	Failed         uint64 `json:"failed"`
	WorkloadFailed uint64 `json:"workload_failed"`
	DatagramFailed uint64 `json:"datagram_failed"`
	HealthFailed   uint64 `json:"health_failed"`
	Invalid        bool   `json:"invalid"`
}

func probeLineagePackCounts(tracker *sendPackLifecycleTracker) probeLineagePackFailureCounts {
	if tracker == nil {
		return probeLineagePackFailureCounts{}
	}
	return probeLineagePackFailureCounts{
		Started: tracker.started.Load(), Failed: tracker.failures.Load(),
		WorkloadFailed: tracker.workloadFailures.Load(), DatagramFailed: tracker.workloadDatagramFailures.Load(),
		HealthFailed: tracker.healthProbeFailures.Load(), Invalid: tracker.invalid.Load(),
	}
}

// This is metadata from an existing, bounded tracker prefix, not a new hot
// callback. It deliberately omits error strings, endpoint IDs and packet data.
type probeLineagePackFailureSample struct {
	Token                 uint64 `json:"token,string"`
	Phase                 uint8  `json:"phase"`
	MessageType           int32  `json:"message_type"`
	AckRequired           bool   `json:"ack_required"`
	HealthProbe           bool   `json:"health_probe"`
	UpstreamRecoverable   bool   `json:"upstream_recoverable"`
	ErrorKind             string `json:"error_kind"`
	AdmissionBoundary     string `json:"admission_boundary,omitempty"`
	AdmissionTimeoutNanos int64  `json:"admission_timeout_nanos,string"`
	RecoveredByOwner      bool   `json:"recovered_by_owner"`
	OwnerTrackingOverflow bool   `json:"owner_tracking_overflow"`
}

func probeLineagePackFailureMetadata(observation clientconnect.SendPackLifecycleObservation) probeLineagePackFailureSample {
	sample := probeLineagePackFailureSample{
		Token: observation.Token, Phase: uint8(observation.Phase), MessageType: int32(observation.MessageType),
		AckRequired: observation.AckRequired, HealthProbe: observation.HealthProbe,
		UpstreamRecoverable: observation.UpstreamRecoverable,
	}
	err := observation.Err
	// The enqueue producer installs this exact type. Do not inspect arbitrary
	// Error/As/Unwrap implementations or retain an unbounded Boundary string.
	if admission, ok := err.(*clientconnect.SendPackAdmissionError); ok && admission != nil {
		sample.AdmissionBoundary = "unknown"
		switch admission.Boundary {
		case "loopback", "resend-capacity", "pack-admission", "queue-handoff":
			sample.AdmissionBoundary = admission.Boundary
		}
		sample.AdmissionTimeoutNanos = int64(admission.Timeout)
		sample.RecoveredByOwner = admission.RecoveredByOwner
		sample.OwnerTrackingOverflow = admission.OwnerTrackingOverflow
		err = admission.Err
	}
	switch err {
	case nil:
		sample.ErrorKind = "none"
	case clientconnect.ErrSendPackNotAdmitted:
		sample.ErrorKind = "not-admitted"
	case context.Canceled:
		sample.ErrorKind = "canceled"
	case context.DeadlineExceeded:
		sample.ErrorKind = "deadline-exceeded"
	default:
		sample.ErrorKind = "other"
	}
	return sample
}

type probeLineagePackFailurePrefix struct {
	Kind             string                          `json:"kind"`
	Scope            string                          `json:"scope"`
	Present          bool                            `json:"present"`
	Capacity         int                             `json:"capacity"`
	FirstSamples     bool                            `json:"first_samples"`
	PointSample      bool                            `json:"point_sample"`
	FailedBefore     uint64                          `json:"failed_before"`
	FailedAfter      uint64                          `json:"failed_after"`
	UnsampledAtLeast uint64                          `json:"unsampled_at_least"`
	TrackerInvalid   bool                            `json:"tracker_invalid"`
	ClientIDsOmitted bool                            `json:"client_ids_omitted"`
	TokenScope       string                          `json:"token_scope"`
	Samples          []probeLineagePackFailureSample `json:"samples"`
}

func probeLineageFailureSnapshot(tracker *sendPackLifecycleTracker, workload bool) probeLineagePackFailurePrefix {
	prefix := probeLineagePackFailurePrefix{
		Kind: "probe-lineage-provider-pack-failures", Scope: "all",
		Capacity: sendPackLifecycleFailureSampleCapacity, FirstSamples: true, PointSample: true,
		ClientIDsOmitted: true, TokenScope: "per-client-instance",
	}
	if workload {
		prefix.Scope = "workload"
	}
	if tracker == nil {
		return prefix
	}
	prefix.Present = true
	failures, snapshot := &tracker.failures, tracker.failureSnapshot
	if workload {
		failures, snapshot = &tracker.workloadFailures, tracker.workloadFailureSnapshot
	}
	prefix.FailedBefore = failures.Load()
	observations := snapshot()
	for _, observation := range observations[:min(len(observations), prefix.Capacity)] {
		prefix.Samples = append(prefix.Samples, probeLineagePackFailureMetadata(observation))
	}
	prefix.FailedAfter = failures.Load()
	if uint64(len(prefix.Samples)) < prefix.FailedBefore {
		prefix.UnsampledAtLeast = prefix.FailedBefore - uint64(len(prefix.Samples))
	}
	prefix.TrackerInvalid = tracker.invalid.Load()
	return prefix
}

// Phase counters are point samples, not a substitute for the existing exact
// owner boundaries. They identify refusal versus carrier/read delay cheaply.
func newProbeLineageObserver(t testing.TB, path *fullTunPath) *fullTunLatencyProbeTestObserver {
	recorder := &probeLineageRecorder{}
	type phaseRecord struct {
		Kind       string                                `json:"kind"`
		Phase      string                                `json:"phase"`
		AtNano     int64                                 `json:"at_unix_nano,string"`
		Device     probeLineageNoAckCounts               `json:"device_noack"`
		Provider   probeLineageNoAckCounts               `json:"provider_noack"`
		Congestion clientconnect.ProviderCongestionDrops `json:"provider_congestion"`
		PackFails  probeLineagePackFailureCounts         `json:"provider_pack_failures"`
	}
	var phases [8]phaseRecord
	phaseCount := 0
	capturePhase := func(phase string) {
		if phaseCount == len(phases) {
			t.Error("probe phase record capacity exceeded")
			return
		}
		var device *clientconnect.Client
		if path.deviceClient != nil {
			device = path.deviceClient.Load()
		}
		phases[phaseCount] = phaseRecord{
			Kind: "probe-lineage-phase", Phase: phase, AtNano: time.Now().UnixNano(),
			Device: probeLineageNoAckSnapshot(device), Provider: probeLineageNoAckSnapshot(path.providerClient),
			Congestion: path.providerRemoteNat.CongestionDropStats(),
			PackFails:  probeLineagePackCounts(path.providerPackSends),
		}
		phaseCount++
	}
	return &fullTunLatencyProbeTestObserver{
		observe: recorder.observe,
		phase:   capturePhase,
		finish: func() {
			capturePhase("joined")
			for _, phase := range phases[:phaseCount] {
				row, _ := json.Marshal(phase)
				t.Logf("[probe-lineage] %s", row)
			}
			for _, workload := range []bool{false, true} {
				row, _ := json.Marshal(probeLineageFailureSnapshot(path.providerPackSends, workload))
				t.Logf("[probe-lineage] %s", row)
			}
			recorder.dump(t)
		},
	}
}

func probeLineageLegacy256Scenario(t testing.TB) perfvarScenario {
	t.Helper()
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "p2p-legacy", "CONNECT_PERFVAR_PROFILE": "cell-edge-256k-down-64k-up",
		"CONNECT_PERFVAR_WORKLOAD": "latency-under-load", "CONNECT_PERFVAR_DIRECTION": "download",
		"CONNECT_PERFVAR_RESOURCE": "mobile-surrogate", "CONNECT_PERFVAR_TOPOLOGY": "one-hop",
		"CONNECT_PERFVAR_RUN_COUNT": "1", "CONNECT_PERFVAR_SEED": "20260810",
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
	scenarioHash, err := scenario.hash()
	if err != nil || scenarioHash != "52fce633b81726285bf5a53897f28562a65689900a69b9ec44bbbae9188fb4b0" {
		t.Fatalf("probe scenario changed: %s %v", scenarioHash, err)
	}
	profileHash, err := scenario.profilesHash()
	if err != nil || profileHash != "c99c85d26dbff8130d621cdd9d62efbf3b73f96429c6698efa75977d17e7f000" {
		t.Fatalf("probe profile changed: %s %v", profileHash, err)
	}
	trace, err := perfvarTraceForRun(scenario, 1)
	wantTrace := perfvarTrace{
		Version: 1, RunIndex: 1, IdentityHash: "48ecee778157849ca495331a0ab8f8989736a60dbf781eb8d9ebda2215f8e042",
		ApplicationOrDirectSeed: 6034230609153212255, ProviderSeed: 8391793036920076727, InternalSeed: 5615796108803679309,
	}
	if err != nil || trace != wantTrace || scenario.RunCount != 1 || scenario.PayloadByteCount != 65536 {
		t.Fatalf("probe trace/workload changed: %+v bytes=%d runs=%d err=%v", trace, scenario.PayloadByteCount, scenario.RunCount, err)
	}
	t.Logf("[probe-lineage-identity] scenario=%s profile=%s trace=%+v bytes=65536 baseline_eligible=false", scenarioHash, profileHash, trace)
	return scenario
}

func TestProbeLineageLegacy256Identity(t *testing.T) { probeLineageLegacy256Scenario(t) }

func TestProbeLineageRecorder(t *testing.T) {
	recorder := &probeLineageRecorder{}
	if got := testing.AllocsPerRun(100, func() {
		recorder.next.Store(0)
		recorder.observe(latencyProbeObservation{})
	}); got != 0 {
		t.Fatalf("recorder hot allocations=%g", got)
	}
	recorder = &probeLineageRecorder{}
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Go(func() {
			for index := range probeLineageCapacity / 4 {
				recorder.observe(latencyProbeObservation{Sequence: uint64(worker*probeLineageCapacity/4 + index)})
			}
		})
	}
	workers.Wait()
	if reserved, overflow, unpublished := recorder.counts(); reserved != probeLineageCapacity || overflow != 0 || unpublished != 0 {
		t.Fatalf("recorder counts=%d/%d/%d", reserved, overflow, unpublished)
	}
	var seen [probeLineageCapacity]bool
	for _, event := range recorder.events {
		if event.Sequence >= probeLineageCapacity || seen[event.Sequence] {
			t.Fatal("concurrent recorder lost sequence identity")
		}
		seen[event.Sequence] = true
	}
	recorder.observe(latencyProbeObservation{})
	if _, overflow, _ := recorder.counts(); overflow != 1 {
		t.Fatal("recorder overflow not explicit")
	}
	recorder = &probeLineageRecorder{}
	recorder.next.Store(1)
	if _, _, unpublished := recorder.counts(); unpublished != 1 {
		t.Fatal("recorder unpublished reservation not explicit")
	}
}

func TestProbeLineagePackFailureMetadata(t *testing.T) {
	for _, boundary := range []string{"loopback", "resend-capacity", "pack-admission", "queue-handoff", "unbounded input omitted"} {
		observation := clientconnect.SendPackLifecycleObservation{
			Token: 1<<63 + 7, Phase: clientconnect.SendPackLifecyclePhaseTerminal,
			MessageType: protocol.MessageType_IpIpPacketFromProvider, AckRequired: false,
			Err: &clientconnect.SendPackAdmissionError{
				Boundary: boundary, Timeout: 0, Err: clientconnect.ErrSendPackNotAdmitted,
				RecoveredByOwner: true, OwnerTrackingOverflow: true,
			},
		}
		sample := probeLineagePackFailureMetadata(observation)
		wantBoundary := boundary
		if boundary == "unbounded input omitted" {
			wantBoundary = "unknown"
		}
		if sample.Token != observation.Token || sample.Phase != uint8(observation.Phase) ||
			sample.MessageType != int32(observation.MessageType) || sample.AckRequired ||
			sample.ErrorKind != "not-admitted" || sample.AdmissionBoundary != wantBoundary ||
			!sample.RecoveredByOwner || !sample.OwnerTrackingOverflow || sample.AdmissionTimeoutNanos != 0 {
			t.Fatalf("bounded admission metadata=%+v", sample)
		}
		if got := testing.AllocsPerRun(100, func() { _ = probeLineagePackFailureMetadata(observation) }); got != 0 {
			t.Fatalf("metadata allocated %g objects", got)
		}
	}
	for _, err := range []error{unreadableLatencyProbeError{}, &clientconnect.SendPackAdmissionError{
		Boundary: "pack-admission", Timeout: time.Second, Err: unreadableLatencyProbeError{},
	}} {
		sample := probeLineagePackFailureMetadata(clientconnect.SendPackLifecycleObservation{Err: err})
		if sample.ErrorKind != "other" {
			t.Fatalf("opaque error metadata=%+v", sample)
		}
		if _, marshalErr := json.Marshal(sample); marshalErr != nil {
			t.Fatal(marshalErr)
		}
	}
}

func TestProbeLineagePackFailureSnapshot(t *testing.T) {
	if got := probeLineageFailureSnapshot(nil, false); got.Present || len(got.Samples) != 0 {
		t.Fatal("absent tracker invented failures")
	}
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	const failures = sendPackLifecycleFailureSampleCapacity + 7
	for index := range failures {
		observation := clientconnect.SendPackLifecycleObservation{
			Token: uint64(index + 1), MessageType: protocol.MessageType_IpIpPacketFromProvider,
		}
		// One empty control response distinguishes the all/workload prefixes.
		if index == 0 {
			observation.MessageType = protocol.MessageType_IpIpPing
		}
		for _, phase := range []clientconnect.SendPackLifecyclePhase{
			clientconnect.SendPackLifecyclePhaseStarted, clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
			clientconnect.SendPackLifecyclePhaseTerminal,
		} {
			observation.Phase = phase
			if phase != clientconnect.SendPackLifecyclePhaseStarted {
				observation.Err = &clientconnect.SendPackAdmissionError{
					Boundary: "pack-admission", Err: clientconnect.ErrSendPackNotAdmitted,
				}
			}
			observer(observation)
		}
	}
	if boundary, ok := tracker.boundary(t.Context()); !ok || len(boundary.entries) != 0 {
		t.Fatal("failure publications did not join")
	}
	before := probeLineagePackCounts(tracker)
	for _, workload := range []bool{false, true} {
		prefix := probeLineageFailureSnapshot(tracker, workload)
		wantFailures, wantFirst := uint64(failures), uint64(1)
		if workload {
			wantFailures--
			wantFirst++
		}
		if !prefix.Present || prefix.TrackerInvalid || !prefix.FirstSamples || !prefix.PointSample ||
			prefix.Capacity != sendPackLifecycleFailureSampleCapacity || len(prefix.Samples) != prefix.Capacity ||
			prefix.FailedBefore != wantFailures || prefix.FailedAfter != wantFailures ||
			prefix.UnsampledAtLeast != wantFailures-uint64(prefix.Capacity) {
			t.Fatalf("prefix lost explicit sampling bounds: %+v", prefix)
		}
		for index, sample := range prefix.Samples {
			if sample.Token != wantFirst+uint64(index) || sample.AdmissionBoundary != "pack-admission" ||
				sample.ErrorKind != "not-admitted" || sample.AckRequired {
				t.Fatalf("prefix identity/admission=%+v", sample)
			}
		}
		prefix.Samples[0].AdmissionBoundary = "changed snapshot only"
		if got := probeLineageFailureSnapshot(tracker, workload); got.Samples[0].AdmissionBoundary != "pack-admission" {
			t.Fatal("snapshot mutated tracker-owned evidence")
		}
	}
	if got := probeLineagePackCounts(tracker); got != before || got.Failed != failures ||
		got.WorkloadFailed != failures-1 || got.DatagramFailed != failures-1 || got.Invalid {
		t.Fatalf("snapshot changed live counters: before=%+v after=%+v", before, got)
	}
}

func TestProbeLineageLegacy256Replay(t *testing.T) {
	selection := os.Getenv("CONNECT_PERFVAR_PROBE_LINEAGE_REPLAY")
	if selection == "" {
		t.Skip("probe diagnostic requires explicit selection and a parent-owned slot")
	}
	if selection != "legacy256-download1" || os.Getenv("URNETWORK_PROBE_LINEAGE_REPLAY_SLOT") != "granted" {
		t.Fatal("probe diagnostic requires legacy256-download1 and a parent-owned host/source slot")
	}
	maxProcs := runtime.GOMAXPROCS(0)
	if maxProcs != 8 || !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, maxProcs) {
		t.Fatalf("probe runtime drift: Go=%s GOMAXPROCS=%d platform=%s/%s", runtime.Version(), maxProcs, runtime.GOOS, runtime.GOARCH)
	}
	if os.Getenv("CONNECT_PERFVAR_PROGRESS_TRACE") != "1" || !clientconnect.DefaultLogger().V(1).Enabled() {
		t.Fatal("probe diagnostic requires CONNECT_PERFVAR_PROGRESS_TRACE=1 and -args -v=1")
	}
	if newPerfvarProgressTraceForTest != nil || newFullTunLatencyProbeObserverForTest != nil {
		t.Fatal("another diagnostic owns the observer factory")
	}
	scenario := probeLineageLegacy256Scenario(t)
	newPerfvarProgressTraceForTest = newProbeSctpLineageTrace
	newFullTunLatencyProbeObserverForTest = func(path *fullTunPath) *fullTunLatencyProbeTestObserver {
		return newProbeLineageObserver(t, path)
	}
	defer func() {
		newPerfvarProgressTraceForTest = nil
		newFullTunLatencyProbeObserverForTest = nil
	}()
	t.Logf("[probe-lineage-runtime] identity=legacy256-download1-cpu%d-v1 baseline_eligible=false", maxProcs)
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), perfvarRunTimeout(scenario))
		defer cancel()
		record, err := measurePerfvarRun(ctx, t, scenario, 1)
		emitPerfvarRecord(t, record)
		if err != nil {
			t.Fatal(err)
		}
		if !record.Correct {
			t.Errorf("probe diagnostic reproduced failure: %s", record.FailureReason)
		}
		if record.InvalidReason != "" {
			t.Errorf("probe diagnostic invalid: %s", record.InvalidReason)
		}
	})
}
