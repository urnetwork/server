package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// Exact success remains false, but a validated partial prefix must survive
// its wait's deadline instead of becoming an apparently empty prefix.
func TestProviderReturnAdmissionReceiptUnderTargetDeadlineKeepsObservedPrefix(t *testing.T) {
	for _, matchingPackets := range []int{2, 1, 0} {
		name := map[int]string{2: "two_observed", 1: "one_observed", 0: "observed_zero"}[matchingPackets]
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const packetBytes = clientconnect.ByteCount(1028)
				const targetPackets = int64(3)
				const targetBytes = clientconnect.ByteCount(targetPackets) * packetBytes
				returns := newProviderReturnSendTracker()
				defer returns.close()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				expected := providerReturnTrackerTestFlow(187)
				wrongOwner := expected
				wrongOwner.DestinationId = clientconnect.NewId()
				window, ok := returns.beginFlowWindow(ctx, expected)
				if !ok {
					t.Fatal("fixture did not begin the exact flow window")
				}
				matchingBytes := clientconnect.ByteCount(matchingPackets) * packetBytes
				wantEntries := 0
				if matchingPackets != 0 {
					wantEntries = 1
					observeProviderReturnStarted(returns, 1, expected, matchingPackets, matchingBytes)
					observeProviderReturnCompleted(returns, 1, expected, matchingPackets, matchingBytes, true)
				}
				// Same UDP tuple, different authenticated owner. Its totals equal
				// the target but cannot satisfy or inflate the expected prefix.
				observeProviderReturnStarted(returns, 2, wrongOwner, int(targetPackets), targetBytes)
				observeProviderReturnCompleted(returns, 2, wrongOwner, int(targetPackets), targetBytes, true)
				observed, valid := returns.request(ctx, &providerReturnRequest{
					kind:                    providerReturnRequestBoundary,
					window:                  window,
					expectedPacketCount:     targetPackets,
					expectedPacketByteCount: targetBytes,
				})
				if !valid || observed.exact || observed.boundary.packetCount != int64(matchingPackets) ||
					observed.boundary.packetByteCount != matchingBytes || len(observed.boundary.entries) != wantEntries {
					t.Fatalf("fixture did not establish a validated under-target prefix: valid=%t response=%+v", valid, observed)
				}
				type boundaryResult struct {
					boundary providerReturnFlowBoundary
					ok       bool
				}
				done := make(chan boundaryResult, 1)
				go func() {
					boundary, exact := returns.flowBoundary(ctx, window, targetPackets, targetBytes)
					done <- boundaryResult{boundary: boundary, ok: exact}
				}()
				synctest.Wait()
				select {
				case early := <-done:
					t.Fatalf("under-target flow returned before deadline: %+v", early)
				default:
				}
				time.Sleep(time.Second)
				result := <-done
				if result.ok || ctx.Err() != context.DeadlineExceeded || returns.invalid.Load() {
					t.Fatalf("exact gate or tracker validity changed: exact=%t context=%v invalid=%t", result.ok, ctx.Err(), returns.invalid.Load())
				}
				path := &fullTunPath{providerReturns: returns}
				failure := path.providerReturnSourceFailure(ctx, result.boundary, result.ok, targetPackets, targetBytes, 0)
				if !errors.Is(failure, context.DeadlineExceeded) {
					t.Fatalf("source failure no longer unwraps deadline: %v", failure)
				}
				receipt := providerReturnReceiptForTest(t, failure)
				if receipt.BoundaryCaptured || receipt.ContextStatus != "deadline-exceeded" || receipt.TargetPackets != targetPackets || receipt.TargetBytes != targetBytes {
					t.Fatalf("partial evidence was promoted to success: %+v", receipt)
				}
				if receipt.PrefixPackets != int64(matchingPackets) || receipt.PrefixBytes != matchingBytes || receipt.PrefixEntries != wantEntries {
					t.Fatalf("deadline erased validated observed prefix: got packets=%d bytes=%d entries=%d; want %d/%d/%d (exact boundary must remain false)", receipt.PrefixPackets, receipt.PrefixBytes, receipt.PrefixEntries, matchingPackets, matchingBytes, wantEntries)
				}
				if receipt.PrefixPendingEntries != 0 || receipt.PrefixRejectedPackets != 0 ||
					receipt.GlobalStartedPackets != int64(matchingPackets)+targetPackets || receipt.GlobalFailuresAfter != 0 {
					t.Fatalf("observed partial/foreign ownership changed: %+v", receipt)
				}
				_, suffix, _ := strings.Cut(failure.Error(), providerReturnAdmissionReceiptMarker)
				var availability struct {
					PrefixSnapshotPresent bool `json:"prefix_snapshot_present"`
				}
				if err := json.Unmarshal([]byte(suffix), &availability); err != nil {
					t.Fatal(err)
				}
				if !availability.PrefixSnapshotPresent {
					t.Fatal("observed zero/partial prefix is indistinguishable from unavailable evidence")
				}
				if strings.Contains(failure.Error(), expected.DestinationId.String()) ||
					strings.Contains(failure.Error(), wrongOwner.DestinationId.String()) {
					t.Fatal("raw owner identity leaked into the normal receipt")
				}
			})
		})
	}
}

// An expired wait before any owner response cannot manufacture an observed
// zero from unrelated global history.
func TestProviderReturnAdmissionReceiptBeforeObservationStaysUnavailable(t *testing.T) {
	returns := newProviderReturnSendTracker()
	defer returns.close()
	setup, cancelSetup := context.WithTimeout(context.Background(), time.Second)
	defer cancelSetup()
	flow := providerReturnTrackerTestFlow(188)
	window, ok := returns.beginFlowWindow(setup, flow)
	if !ok {
		t.Fatal("begin fixture")
	}
	// Hold the owner so an expired caller cannot win a ready-queue select
	// and obtain a real zero snapshot. The barrier proves no response exists.
	ownerHeld := make(chan struct{})
	releaseOwner := make(chan struct{})
	defer close(releaseOwner)
	returns.setBeforeTerminalReleaseForTest(func(observation clientconnect.RemoteUserNatProviderReturnSendObservation) {
		if observation.Token == 99 {
			close(ownerHeld)
			<-releaseOwner
		}
	})
	other := flow
	other.SourcePort++
	observeProviderReturnStarted(returns, 99, other, 1, 1028)
	observeProviderReturnCompleted(returns, 99, other, 1, 1028, true)
	select {
	case <-ownerHeld:
	case <-setup.Done():
		t.Fatal("fixture owner did not reach its terminal barrier")
	}
	failed, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	boundary, exact := returns.flowBoundary(failed, window, 1, 1028)
	if exact {
		t.Fatal("expired unobserved boundary succeeded")
	}
	path := &fullTunPath{providerReturns: returns}
	failure := path.providerReturnSourceFailure(failed, boundary, exact, 1, 1028, 0)
	receipt := providerReturnReceiptForTest(t, failure)
	_, suffix, _ := strings.Cut(failure.Error(), providerReturnAdmissionReceiptMarker)
	var availability struct {
		PrefixSnapshotPresent bool `json:"prefix_snapshot_present"`
	}
	if err := json.Unmarshal([]byte(suffix), &availability); err != nil {
		t.Fatal(err)
	}
	if availability.PrefixSnapshotPresent || receipt.BoundaryCaptured || receipt.PrefixPackets != 0 ||
		receipt.PrefixBytes != 0 || !errors.Is(failure, context.DeadlineExceeded) {
		t.Fatalf("invented observed-zero evidence: %+v availability=%+v", receipt, availability)
	}
}

type providerReturnBoundaryResultForTest struct {
	boundary providerReturnFlowBoundary
	exact    bool
}

func startProviderReturnBoundaryForTest(ctx context.Context, tracker *providerReturnSendTracker,
	window providerReturnFlowWindow, packets int64, bytes clientconnect.ByteCount) <-chan providerReturnBoundaryResultForTest {
	done := make(chan providerReturnBoundaryResultForTest, 1)
	go func() {
		boundary, exact := tracker.flowBoundary(ctx, window, packets, bytes)
		done <- providerReturnBoundaryResultForTest{boundary: boundary, exact: exact}
	}()
	return done
}

func providerReturnPrefixSnapshotPresentForTest(t *testing.T, failure error) bool {
	t.Helper()
	_, suffix, found := strings.Cut(failure.Error(), providerReturnAdmissionReceiptMarker)
	if !found {
		t.Fatal("missing bounded failure receipt")
	}
	var fields struct {
		Present bool `json:"prefix_snapshot_present"`
	}
	if err := json.Unmarshal([]byte(suffix), &fields); err != nil {
		t.Fatal(err)
	}
	return fields.Present
}

// The last valid owner response survives cancellation or tracker shutdown.
// Neither can turn an under-target snapshot into an exact boundary.
func TestProviderReturnAdmissionReceiptUnderTargetKeepsLatestValidatedSnapshot(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "caller_canceled"
		if shutdown {
			name = "tracker_closed"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tracker := newProviderReturnSendTracker()
				defer tracker.close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				flow := providerReturnTrackerTestFlow(189)
				window, ok := tracker.beginFlowWindow(ctx, flow)
				if !ok {
					t.Fatal("begin partial window")
				}
				observeProviderReturnStarted(tracker, 1, flow, 1, 1028)
				observeProviderReturnCompleted(tracker, 1, flow, 1, 1028, true)
				done := startProviderReturnBoundaryForTest(ctx, tracker, window, 3, 3084)
				synctest.Wait()
				observeProviderReturnStarted(tracker, 2, flow, 1, 1028)
				observeProviderReturnCompleted(tracker, 2, flow, 1, 1028, true)
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("partial count satisfied an exact gate")
				default:
				}
				wantStatus := "canceled"
				var wantCause error = context.Canceled
				if shutdown {
					tracker.close()
					wantStatus, wantCause = "active", nil
				} else {
					cancel()
				}
				result := <-done
				path := &fullTunPath{providerReturns: tracker}
				failure := path.providerReturnSourceFailure(ctx, result.boundary, result.exact, 3, 3084, 0)
				receipt := providerReturnReceiptForTest(t, failure)
				if result.exact || receipt.BoundaryCaptured || !providerReturnPrefixSnapshotPresentForTest(t, failure) ||
					result.boundary.window != window || receipt.PrefixPackets != 2 || receipt.PrefixBytes != 2056 || receipt.PrefixEntries != 2 ||
					receipt.PrefixPendingEntries != 0 || receipt.PrefixRejectedPackets != 0 ||
					receipt.ContextStatus != wantStatus || errors.Unwrap(failure) != wantCause || tracker.invalid.Load() {
					t.Fatalf("latest validated prefix or error cause lost: %+v exact=%t", receipt, result.exact)
				}
				// Only the nested failure text changes, never run classification.
				record := perfvarRunRecord{SchemaVersion: perfvarSchemaVersion, RecordType: "run", Correct: false,
					FailureStage: "workload", FailureReason: failure.Error()}
				data, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				var decoded perfvarRunRecord
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded.SchemaVersion != 14 || decoded.Correct || decoded.FailureStage != "workload" || decoded.FailureReason != failure.Error() {
					t.Fatal("partial prefix changed schema14 workload-failure classification")
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				for _, forbidden := range []string{"prefix_snapshot_present", "provider_return_admission_receipt_v1", "flow_key", "destination_id", "token"} {
					if _, present := fields[forbidden]; present {
						t.Fatalf("diagnostic escaped failure_reason: %s", forbidden)
					}
				}
				if strings.Contains(failure.Error(), flow.DestinationId.String()) || strings.Contains(failure.Error(), "192.0.2.189") {
					t.Fatal("raw flow identity escaped into normal receipt")
				}
			})
		})
	}
}

// Prefix membership/counts are the last validated owner cut; terminal status
// remains the existing receipt-time sample of those same entries.
func TestProviderReturnAdmissionReceiptUnderTargetKeepsPendingAndRejectedEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tracker := newProviderReturnSendTracker()
		defer tracker.close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		flow := providerReturnTrackerTestFlow(190)
		window, ok := tracker.beginFlowWindow(ctx, flow)
		if !ok {
			t.Fatal("begin mixed-outcome window")
		}
		observeProviderReturnStarted(tracker, 1, flow, 1, 1028)
		observeProviderReturnStarted(tracker, 2, flow, 2, 2056)
		observeProviderReturnCompleted(tracker, 2, flow, 2, 2056, false)
		done := startProviderReturnBoundaryForTest(ctx, tracker, window, 4, 4112)
		synctest.Wait()
		time.Sleep(time.Second)
		result := <-done
		path := &fullTunPath{providerReturns: tracker}
		failure := path.providerReturnSourceFailure(ctx, result.boundary, result.exact, 4, 4112, 0)
		receipt := providerReturnReceiptForTest(t, failure)
		if result.exact || receipt.BoundaryCaptured || !providerReturnPrefixSnapshotPresentForTest(t, failure) ||
			receipt.PrefixPackets != 3 || receipt.PrefixBytes != 3084 || receipt.PrefixEntries != 2 ||
			receipt.PrefixPendingEntries != 1 || receipt.PrefixRejectedPackets != 2 ||
			receipt.GlobalFailuresAfter != 2 || !errors.Is(failure, context.DeadlineExceeded) || tracker.invalid.Load() {
			t.Fatalf("under-target outcomes erased or promoted: %+v", receipt)
		}
		// Formatting does not request another owner snapshot. A later Start
		// is excluded; retained token1's atomic terminal status may advance.
		observeProviderReturnCompleted(tracker, 1, flow, 1, 1028, true)
		observeProviderReturnStarted(tracker, 3, flow, 1, 1028)
		synctest.Wait()
		later := providerReturnReceiptForTest(t, path.providerReturnSourceFailure(ctx, result.boundary, false, 4, 4112, 0))
		if later.BoundaryCaptured || !later.PrefixSnapshotPresent || later.PrefixPackets != 3 || later.PrefixBytes != 3084 ||
			later.PrefixEntries != 2 || later.PrefixPendingEntries != 0 || later.PrefixRejectedPackets != 2 ||
			later.GlobalStartedPackets != 4 {
			t.Fatalf("formatting mutated frozen prefix membership or forged an exact boundary: %+v", later)
		}
	})
}

// An invalid owner response never becomes a valid prefix. A prior valid
// response may survive, visibly alongside the now-invalid tracker.
func TestProviderReturnAdmissionReceiptInvalidResponseCannotReplaceValidPrefix(t *testing.T) {
	for _, priorValid := range []bool{true, false} {
		name := "without_prior"
		if priorValid {
			name = "with_prior"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tracker := newProviderReturnSendTracker()
				defer tracker.close()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				flow := providerReturnTrackerTestFlow(191)
				window, ok := tracker.beginFlowWindow(ctx, flow)
				if !ok {
					t.Fatal("begin invalidation window")
				}
				var done <-chan providerReturnBoundaryResultForTest
				if priorValid {
					observeProviderReturnStarted(tracker, 1, flow, 1, 1028)
					observeProviderReturnCompleted(tracker, 1, flow, 1, 1028, true)
					done = startProviderReturnBoundaryForTest(ctx, tracker, window, 3, 3084)
					synctest.Wait()
				}
				observeProviderReturnStarted(tracker, 2, flow, 4, 4112)
				observeProviderReturnCompleted(tracker, 2, flow, 4, 4112, true)
				if !priorValid {
					done = startProviderReturnBoundaryForTest(ctx, tracker, window, 3, 3084)
				}
				result := <-done
				path := &fullTunPath{providerReturns: tracker}
				failure := path.providerReturnSourceFailure(ctx, result.boundary, result.exact, 3, 3084, 0)
				receipt := providerReturnReceiptForTest(t, failure)
				wantPackets, wantEntries := int64(0), 0
				if priorValid {
					wantPackets, wantEntries = 1, 1
				}
				if result.exact || receipt.BoundaryCaptured || !receipt.ReturnTrackerInvalid || ctx.Err() != nil ||
					providerReturnPrefixSnapshotPresentForTest(t, failure) != priorValid ||
					receipt.PrefixPackets != wantPackets || receipt.PrefixBytes != clientconnect.ByteCount(wantPackets)*1028 ||
					receipt.PrefixEntries != wantEntries || receipt.ContextStatus != "active" || errors.Unwrap(failure) != nil {
					t.Fatalf("invalid response replaced valid diagnostic evidence or changed error cause: %+v", receipt)
				}
			})
		})
	}
}

func TestProviderReturnAdmissionReceiptOldV1SnapshotAvailability(t *testing.T) {
	for _, test := range []struct {
		name    string
		receipt string
		exact   bool
		packets int64
	}{
		{"unavailable", `{"receipt_version":1,"boundary_captured":false,"prefix_packets":0}`, false, 0},
		{"exact", `{"receipt_version":1,"boundary_captured":true,"prefix_packets":3}`, true, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := errors.New("provider return source did not complete measured flow" + providerReturnAdmissionReceiptMarker + test.receipt)
			receipt := providerReturnReceiptForTest(t, failure)
			if receipt.PrefixSnapshotPresent || receipt.BoundaryCaptured != test.exact || receipt.PrefixPackets != test.packets {
				t.Fatalf("old v1 receipt invented availability or lost exact evidence: %+v", receipt)
			}
		})
	}
}
