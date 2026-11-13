package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

const providerReturnAdmissionReceiptMarker = "; provider_return_admission_receipt_v1="
const providerReturnAdmissionReceiptByteLimit = 8192

type providerReturnAdmissionSample struct {
	Ordinal               int    `json:"ordinal"`
	MessageType           int32  `json:"message_type"`
	AckRequired           bool   `json:"ack_required"`
	TypedAdmission        bool   `json:"typed_admission"`
	AdmissionBoundary     string `json:"admission_boundary"`
	TimeoutNanos          int64  `json:"timeout_nanos"`
	RecoveredByOwner      bool   `json:"recovered_by_owner"`
	OwnerTrackingOverflow bool   `json:"owner_tracking_overflow"`
	ErrorKind             string `json:"error_kind"`
}

// Inspect only the producer's exact type and known sentinels. Arbitrary
// Error/As/Unwrap methods and raw boundary/error text are never invoked.
func providerReturnAdmissionMetadata(observation clientconnect.SendPackLifecycleObservation, ordinal int) providerReturnAdmissionSample {
	sample := providerReturnAdmissionSample{Ordinal: ordinal, MessageType: int32(observation.MessageType), AckRequired: observation.AckRequired, AdmissionBoundary: "unavailable"}
	err := observation.Err
	if admission, ok := err.(*clientconnect.SendPackAdmissionError); ok {
		if admission == nil {
			err = nil
		} else {
			sample.TypedAdmission = true
			sample.AdmissionBoundary = "unknown"
			switch admission.Boundary {
			case "loopback", "resend-capacity", "pack-admission", "queue-handoff":
				sample.AdmissionBoundary = admission.Boundary
			}
			sample.TimeoutNanos = int64(admission.Timeout)
			sample.RecoveredByOwner = admission.RecoveredByOwner
			sample.OwnerTrackingOverflow = admission.OwnerTrackingOverflow
			err = admission.Err
		}
	}
	switch err {
	case nil:
		sample.ErrorKind = "missing"
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

type providerReturnAdmissionPrefix struct {
	Present          bool                            `json:"present"`
	Scope            string                          `json:"scope"`
	Capacity         int                             `json:"capacity"`
	CapacityReached  bool                            `json:"capacity_reached"`
	FailedBefore     uint64                          `json:"failed_before"`
	FailedAfter      uint64                          `json:"failed_after"`
	UnsampledAtLeast uint64                          `json:"unsampled_at_least"`
	TrackerInvalid   bool                            `json:"tracker_invalid"`
	Samples          []providerReturnAdmissionSample `json:"samples"`
}

func providerReturnAdmissionSnapshot(tracker *sendPackLifecycleTracker) providerReturnAdmissionPrefix {
	prefix := providerReturnAdmissionPrefix{Scope: "provider-workload-first-failures-point-sample-not-return-token-correlated", Capacity: sendPackLifecycleFailureSampleCapacity}
	if tracker == nil {
		return prefix
	}
	prefix.Present = true
	prefix.FailedBefore = tracker.workloadFailures.Load()
	observations := tracker.workloadFailureSnapshot()
	for i, observation := range observations[:min(len(observations), prefix.Capacity)] {
		prefix.Samples = append(prefix.Samples, providerReturnAdmissionMetadata(observation, i+1))
	}
	prefix.FailedAfter = tracker.workloadFailures.Load()
	prefix.CapacityReached = len(prefix.Samples) == prefix.Capacity
	if uint64(len(prefix.Samples)) < prefix.FailedBefore {
		prefix.UnsampledAtLeast = prefix.FailedBefore - uint64(len(prefix.Samples))
	}
	prefix.TrackerInvalid = tracker.invalid.Load()
	return prefix
}

type providerReturnAdmissionReceipt struct {
	Version               int                                   `json:"receipt_version"`
	BoundaryCaptured      bool                                  `json:"boundary_captured"`
	PrefixSnapshotPresent bool                                  `json:"prefix_snapshot_present"`
	PrefixPointSample     bool                                  `json:"return_prefix_point_sample"`
	ContextStatus         string                                `json:"context_status"`
	TargetPackets         int64                                 `json:"target_packets"`
	TargetBytes           clientconnect.ByteCount               `json:"target_bytes"`
	PrefixPackets         int64                                 `json:"prefix_packets"`
	PrefixBytes           clientconnect.ByteCount               `json:"prefix_bytes"`
	PrefixEntries         int                                   `json:"prefix_entries"`
	PrefixPendingEntries  int                                   `json:"prefix_pending_entries"`
	PrefixRejectedPackets int64                                 `json:"prefix_rejected_packets"`
	ReturnTrackerPresent  bool                                  `json:"return_tracker_present"`
	ReturnTrackerInvalid  bool                                  `json:"return_tracker_invalid"`
	GlobalFailuresBefore  int64                                 `json:"global_return_failures_before"`
	GlobalFailuresAfter   int64                                 `json:"global_return_failures_after"`
	GlobalStartedPackets  int64                                 `json:"global_started_packets"`
	GlobalStartedBytes    int64                                 `json:"global_started_bytes"`
	CongestionPresent     bool                                  `json:"congestion_present"`
	Congestion            clientconnect.ProviderCongestionDrops `json:"congestion_global_point_sample"`
	PackPrefix            providerReturnAdmissionPrefix         `json:"pack_failure_prefix"`
}

type providerReturnSourceFailureError struct {
	text  string
	cause error
}

func (e *providerReturnSourceFailureError) Error() string { return e.text }
func (e *providerReturnSourceFailureError) Unwrap() error { return e.cause }

// Called only at the existing failed measured UDP provider-return boundary.
// Validated return counts and the separate Pack prefix are never token-correlated.
// The existing schema14 failure_reason string carries the receipt, not a new
// record or schema field. No callback, successful run, or admission is changed.
func (path *fullTunPath) providerReturnSourceFailure(ctx context.Context, boundary providerReturnFlowBoundary,
	boundaryOK bool, targetPackets int64, targetBytes clientconnect.ByteCount, failedBefore int64) error {
	receipt := providerReturnAdmissionReceipt{Version: 1, BoundaryCaptured: boundaryOK, PrefixSnapshotPresent: boundaryOK || boundary.snapshotPresent, PrefixPointSample: true, TargetPackets: targetPackets, TargetBytes: targetBytes, GlobalFailuresBefore: failedBefore, PackPrefix: providerReturnAdmissionSnapshot(path.providerPackSends)}
	cause := ctx.Err()
	switch cause {
	case nil:
		receipt.ContextStatus = "active"
	case context.Canceled:
		receipt.ContextStatus = "canceled"
	case context.DeadlineExceeded:
		receipt.ContextStatus = "deadline-exceeded"
	default:
		receipt.ContextStatus = "other"
	}
	if receipt.PrefixSnapshotPresent {
		receipt.PrefixPackets, receipt.PrefixBytes = boundary.packetCount, boundary.packetByteCount
		receipt.PrefixEntries = len(boundary.entries)
		for _, entry := range boundary.entries {
			if entry.state.Load() != providerReturnSendEntryTerminal {
				receipt.PrefixPendingEntries++
			} else if !entry.sent.Load() {
				receipt.PrefixRejectedPackets += int64(entry.packetCount)
			}
		}
	}
	if tracker := path.providerReturns; tracker != nil {
		receipt.ReturnTrackerPresent, receipt.ReturnTrackerInvalid = true, tracker.invalid.Load()
		receipt.GlobalFailuresAfter = tracker.failures.Load()
		receipt.GlobalStartedPackets, receipt.GlobalStartedBytes = tracker.startedPacketCount.Load(), tracker.startedByteCount.Load()
	}
	if path.providerRemoteNat != nil {
		receipt.CongestionPresent = true
		receipt.Congestion = path.providerRemoteNat.CongestionDropStats()
	}
	data, err := json.Marshal(receipt)
	if err != nil || len(data) > providerReturnAdmissionReceiptByteLimit {
		// A diagnostic must never mask or suppress the original failure.
		data = []byte(`{"receipt_version":1,"unavailable":"format-or-size-limit"}`)
	}
	return &providerReturnSourceFailureError{text: "provider return source did not complete measured flow" + providerReturnAdmissionReceiptMarker + string(data), cause: cause}
}

func TestProviderReturnAdmissionReceiptPreservesTypedGate(t *testing.T) {
	returns := newProviderReturnSendTracker()
	defer returns.close()
	packs := newSendPackLifecycleTracker()
	defer packs.close()
	packs.workloadFailureSamples = []clientconnect.SendPackLifecycleObservation{{
		Phase:       clientconnect.SendPackLifecyclePhaseTerminal,
		MessageType: protocol.MessageType_IpIpPacketFromProvider,
		Err:         &clientconnect.SendPackAdmissionError{Boundary: "queue-handoff", Timeout: 25 * time.Millisecond, Err: clientconnect.ErrSendPackNotAdmitted},
	}}
	packs.workloadFailures.Store(1)
	path := &fullTunPath{providerReturns: returns, providerPackSends: packs}
	err := path.providerReturnSourceFailure(context.Background(), providerReturnFlowBoundary{}, false, 4, 128, 0)
	if err == nil || !strings.Contains(err.Error(), `; provider_return_admission_receipt_v1=`) ||
		!strings.Contains(err.Error(), `"admission_boundary":"queue-handoff"`) {
		t.Fatal("provider-return failure lost the existing typed admission gate")
	}
}

func providerReturnReceiptForTest(t *testing.T, err error) providerReturnAdmissionReceipt {
	t.Helper()
	if err == nil {
		t.Fatal("source failure disappeared")
	}
	_, suffix, present := strings.Cut(err.Error(), providerReturnAdmissionReceiptMarker)
	if !present || len(suffix) > providerReturnAdmissionReceiptByteLimit {
		t.Fatal("missing or unbounded receipt")
	}
	var receipt providerReturnAdmissionReceipt
	if err := json.Unmarshal([]byte(suffix), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Version != 1 {
		t.Fatal("unexpected receipt version")
	}
	return receipt
}

type providerReturnCanaryError struct{ secret []byte }

func (providerReturnCanaryError) Error() string { panic("raw Error called") }
func (providerReturnCanaryError) Unwrap() error { panic("raw Unwrap called") }
func (providerReturnCanaryError) As(any) bool   { panic("raw As called") }

func TestProviderReturnAdmissionReceiptTypedUntypedMissingAndCanary(t *testing.T) {
	var nilAdmission *clientconnect.SendPackAdmissionError
	canary := providerReturnCanaryError{secret: []byte("CANARY-RAW-ERROR")}
	for _, test := range []struct {
		name           string
		err            error
		typed          bool
		boundary, kind string
	}{
		{"loopback", &clientconnect.SendPackAdmissionError{Boundary: "loopback", Err: clientconnect.ErrSendPackNotAdmitted}, true, "loopback", "not-admitted"},
		{"resend", &clientconnect.SendPackAdmissionError{Boundary: "resend-capacity", Err: clientconnect.ErrSendPackNotAdmitted}, true, "resend-capacity", "not-admitted"},
		{"pack", &clientconnect.SendPackAdmissionError{Boundary: "pack-admission", Err: clientconnect.ErrSendPackNotAdmitted}, true, "pack-admission", "not-admitted"},
		{"handoff", &clientconnect.SendPackAdmissionError{Boundary: "queue-handoff", Err: context.DeadlineExceeded}, true, "queue-handoff", "deadline-exceeded"},
		{"unknown-canary", &clientconnect.SendPackAdmissionError{Boundary: strings.Repeat("CANARY", 2000), Err: canary}, true, "unknown", "other"},
		{"typed-missing-cause", &clientconnect.SendPackAdmissionError{Boundary: "pack-admission"}, true, "pack-admission", "missing"},
		{"untyped", clientconnect.ErrSendPackNotAdmitted, false, "unavailable", "not-admitted"},
		{"canceled", context.Canceled, false, "unavailable", "canceled"},
		{"wrapped-unavailable", fmt.Errorf("CANARY-WRAPPER: %w", clientconnect.ErrSendPackNotAdmitted), false, "unavailable", "other"},
		{"untyped-canary", canary, false, "unavailable", "other"},
		{"missing", nil, false, "unavailable", "missing"},
		{"typed-nil", nilAdmission, false, "unavailable", "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := clientconnect.SendPackLifecycleObservation{Err: test.err, Token: 991239123, ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId()}
			sample := providerReturnAdmissionMetadata(observation, 1)
			if sample.TypedAdmission != test.typed || sample.AdmissionBoundary != test.boundary || sample.ErrorKind != test.kind {
				t.Fatalf("unexpected finite metadata: %+v", sample)
			}
			data, err := json.Marshal(sample)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"CANARY", observation.ClientId.String(), observation.DestinationId.String(), "991239123"} {
				if strings.Contains(string(data), secret) {
					t.Fatal("raw identity/error escaped metadata")
				}
			}
		})
	}
}

func TestProviderReturnAdmissionReceiptExactPrefixAndErrorPreserved(t *testing.T) {
	returns := newProviderReturnSendTracker()
	defer returns.close()
	flow := providerReturnTrackerTestFlow(91)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	window, ok := returns.beginFlowWindow(ctx, flow)
	if !ok {
		t.Fatal("begin")
	}
	for token := uint64(1); token <= 3; token++ {
		observeProviderReturnStarted(returns, token, flow, 2, 128)
	}
	observeProviderReturnCompleted(returns, 1, flow, 2, 128, true)
	observeProviderReturnCompleted(returns, 2, flow, 2, 128, false)
	boundary, ok := returns.flowBoundary(ctx, window, 6, 384)
	if !ok || returns.waitThrough(ctx, boundary) {
		t.Fatal("failure boundary changed")
	}
	path := &fullTunPath{providerReturns: returns}
	err := path.providerReturnSourceFailure(ctx, boundary, true, 6, 384, 0)
	receipt := providerReturnReceiptForTest(t, err)
	if !receipt.BoundaryCaptured || !receipt.PrefixSnapshotPresent || !receipt.PrefixPointSample || receipt.PrefixEntries != 3 || receipt.PrefixPendingEntries != 1 || receipt.PrefixRejectedPackets != 2 || receipt.PrefixPackets != 6 || receipt.PrefixBytes != 384 {
		t.Fatalf("wrong exact prefix: %+v", receipt)
	}
	if receipt.PackPrefix.Present || receipt.PackPrefix.Samples != nil {
		t.Fatal("invented Pack evidence")
	}
	if strings.Contains(err.Error(), flow.DestinationId.String()) || strings.Contains(err.Error(), "192.0.2.91") {
		t.Fatal("raw flow escaped")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		var failed context.Context
		var finish context.CancelFunc
		if cause == context.Canceled {
			failed, finish = context.WithCancel(ctx)
			finish()
		} else {
			failed, finish = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			defer finish()
		}
		failure := path.providerReturnSourceFailure(failed, boundary, true, 6, 384, 0)
		if !errors.Is(failure, cause) {
			t.Fatal("context error chain changed")
		}
		if providerReturnReceiptForTest(t, failure).ContextStatus == "active" {
			t.Fatal("context cancellation lost from receipt")
		}
	}
}

func TestProviderReturnAdmissionReceiptCapacityAndMissingPrefix(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	for i := 0; i < sendPackLifecycleFailureSampleCapacity; i++ {
		tracker.workloadFailureSamples = append(tracker.workloadFailureSamples, clientconnect.SendPackLifecycleObservation{
			MessageType: protocol.MessageType_IpIpPacketFromProvider,
			Err:         &clientconnect.SendPackAdmissionError{Boundary: "queue-handoff", Timeout: -1, RecoveredByOwner: true, OwnerTrackingOverflow: true, Err: clientconnect.ErrSendPackNotAdmitted},
		})
	}
	tracker.workloadFailures.Store(141)
	tracker.invalid.Store(true)
	path := &fullTunPath{providerPackSends: tracker}
	err := path.providerReturnSourceFailure(context.Background(), providerReturnFlowBoundary{}, false, 100, 200, 0)
	receipt := providerReturnReceiptForTest(t, err)
	prefix := receipt.PackPrefix
	if !prefix.Present || !prefix.CapacityReached || !prefix.TrackerInvalid || len(prefix.Samples) != 16 || prefix.UnsampledAtLeast != 125 || prefix.FailedBefore != 141 || prefix.FailedAfter != 141 {
		t.Fatalf("lost missing/capacity evidence: %+v", prefix)
	}
	for i, sample := range prefix.Samples {
		if sample.Ordinal != i+1 || sample.TimeoutNanos != -1 || !sample.RecoveredByOwner || !sample.OwnerTrackingOverflow {
			t.Fatal("typed fields/order changed")
		}
	}
	if !strings.Contains(prefix.Scope, "not-return-token-correlated") {
		t.Fatal("claimed false per-token attribution")
	}
	if path.providerReturnSourceFailure(context.Background(), providerReturnFlowBoundary{}, false, 100, 200, 0).Error() != err.Error() {
		t.Fatal("non-deterministic receipt ordering")
	}
	tracker.failureLock.Lock()
	tracker.workloadFailureSamples = nil
	tracker.failureLock.Unlock()
	prefix = providerReturnAdmissionSnapshot(tracker)
	if !prefix.Present || prefix.CapacityReached || len(prefix.Samples) != 0 || prefix.UnsampledAtLeast != 141 {
		t.Fatal("unpublished/missing samples inferred as no failures")
	}
}

func TestProviderReturnAdmissionReceiptConcurrentSnapshot(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 64; i++ {
			tracker.workloadFailures.Add(1)
			tracker.failureLock.Lock()
			if len(tracker.workloadFailureSamples) < sendPackLifecycleFailureSampleCapacity {
				tracker.workloadFailureSamples = append(tracker.workloadFailureSamples, clientconnect.SendPackLifecycleObservation{Err: clientconnect.ErrSendPackNotAdmitted})
			}
			tracker.failureLock.Unlock()
		}
	}()
	for i := 0; i < 64; i++ {
		prefix := providerReturnAdmissionSnapshot(tracker)
		if len(prefix.Samples) > 16 || prefix.FailedAfter < prefix.FailedBefore {
			t.Error("invalid concurrent point sample")
		}
	}
	wg.Wait()
}

func TestProviderReturnAdmissionReceiptSchema14AndOldRecords(t *testing.T) {
	path := &fullTunPath{}
	current := path.providerReturnSourceFailure(context.Background(), providerReturnFlowBoundary{}, false, 1, 64, 0).Error()
	old := "provider return source did not complete flow: failures=22"
	if _, _, present := strings.Cut(old, providerReturnAdmissionReceiptMarker); present {
		t.Fatal("invented receipt in old record")
	}
	for _, reason := range []string{old, current} {
		record := perfvarRunRecord{SchemaVersion: perfvarSchemaVersion, RecordType: "run", Correct: false, FailureReason: reason}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var decoded perfvarRunRecord
		if err = json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.SchemaVersion != 14 || decoded.Correct || decoded.FailureReason != reason {
			t.Fatal("diagnostics changed record schema/correctness")
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if _, added := fields["provider_return_admission_receipt_v1"]; added {
			t.Fatal("receipt escaped existing failure_reason field")
		}
	}
}
