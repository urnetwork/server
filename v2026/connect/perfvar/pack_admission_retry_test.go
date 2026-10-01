package perfvar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// Connect's real full-queue regression produces these exact terminal
// dispositions. Exercise the actual tracker and measured gate, not a copied
// predicate: successful enclosing admission resolves only its own refusal.
func TestSendPackLifecycleRecoveredAdmissionMeasuredGate(t *testing.T) {
	for _, test := range []struct {
		name        string
		err         error
		wantFailure bool
	}{
		{"recovered same input", &clientconnect.SendPackAdmissionError{Boundary: "pack-admission", RecoveredByOwner: true, Err: clientconnect.ErrSendPackNotAdmitted}, false},
		{"final refused input", &clientconnect.SendPackAdmissionError{Boundary: "pack-admission", Err: clientconnect.ErrSendPackNotAdmitted}, true},
		{"tracking overflow", &clientconnect.SendPackAdmissionError{Boundary: "pack-admission", RecoveredByOwner: true, OwnerTrackingOverflow: true, Err: clientconnect.ErrSendPackNotAdmitted}, true},
		{"unclassified sentinel", clientconnect.ErrSendPackNotAdmitted, true},
		{"post admission error", errors.New("terminal after admission"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := newSendPackLifecycleTracker()
			defer tracker.close()
			path := &fullTunPath{devicePackSends: tracker, activePackFailureFloor: &perfvarPackFailureCounts{}}
			observer := tracker.newObserver()
			observation := clientconnect.SendPackLifecycleObservation{
				ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId(),
				Token: 1, AckRequired: true, MessageType: protocol.MessageType_IpIpPacketToProvider,
			}
			for _, phase := range []clientconnect.SendPackLifecyclePhase{
				clientconnect.SendPackLifecyclePhaseStarted,
				clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
				clientconnect.SendPackLifecyclePhaseTerminal,
			} {
				observation.Phase = phase
				if phase != clientconnect.SendPackLifecyclePhaseStarted {
					observation.Err = test.err
				}
				observer(observation)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			boundary, ok := tracker.boundary(ctx)
			if !ok || !tracker.waitThrough(ctx, boundary) || tracker.invalid.Load() {
				t.Fatal("failed exact lifecycle boundary")
			}
			if tracker.workloadFailures.Load() != 1 || len(tracker.workloadFailureSnapshot()) != 1 {
				t.Fatal("attempt failure was suppressed from diagnostics")
			}
			err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: snapshotPerfvarPackFailures(path)})
			if (err != nil) != test.wantFailure {
				t.Fatalf("measured gate error=%v want failure=%t", err, test.wantFailure)
			}
		})
	}
}

// The real p2p-legacy download failure had three owned UDP refusals, each
// present in both raw counters. Exercise observer publication and the actual
// gate together so disjoint synthetic counters cannot hide this regression.
func TestProviderDatagramRecoveryOverlapMeasuredGate(t *testing.T) {
	for _, allowDatagrams := range []bool{false, true} {
		for _, extra := range []string{"none", "unrecoverable datagram", "unclassified reliable"} {
			t.Run(fmt.Sprintf("allow=%t/%s", allowDatagrams, extra), func(t *testing.T) {
				tracker := newSendPackLifecycleTracker()
				defer tracker.close()
				path := &fullTunPath{providerPackSends: tracker,
					activePackFailureFloor:            &perfvarPackFailureCounts{},
					allowProviderDatagramPackFailures: allowDatagrams}
				observer := tracker.newObserver()
				publish := func(token uint64, ack, recoverable bool, err error) {
					observation := clientconnect.SendPackLifecycleObservation{
						ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId(),
						Token: token, AckRequired: ack, UpstreamRecoverable: recoverable,
						MessageType: protocol.MessageType_IpIpPacketFromProvider,
					}
					for _, phase := range []clientconnect.SendPackLifecyclePhase{
						clientconnect.SendPackLifecyclePhaseStarted,
						clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
						clientconnect.SendPackLifecyclePhaseTerminal,
					} {
						observation.Phase = phase
						if phase != clientconnect.SendPackLifecyclePhaseStarted {
							observation.Err = err
						}
						observer(observation)
					}
				}
				for token := uint64(1); token <= 3; token++ {
					publish(token, false, true, &clientconnect.SendPackAdmissionError{
						Boundary: "pack-admission", RecoveredByOwner: true, Err: clientconnect.ErrSendPackNotAdmitted})
				}
				if extra != "none" {
					publish(4, extra == "unclassified reliable", false, errors.New("unrecovered terminal"))
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				boundary, ok := tracker.workloadBoundary(ctx)
				if !ok || !tracker.waitThrough(ctx, boundary) || tracker.invalid.Load() {
					t.Fatal("failed exact provider lifecycle boundary")
				}
				counts := snapshotPerfvarPackFailures(path)
				wantDatagrams := uint64(3)
				if extra == "unrecoverable datagram" {
					wantDatagrams++
				}
				if counts.providerRecoverableFailureCount != 3 ||
					counts.providerDatagramFailureCount != wantDatagrams ||
					counts.providerRecoverableDatagramFailureCount != 3 ||
					uint64(len(tracker.workloadFailureSnapshot())) != counts.providerFailureCount {
					t.Fatalf("raw failure diagnostics changed: %+v", counts)
				}
				err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: counts})
				wantFailure := extra == "unclassified reliable" || extra == "unrecoverable datagram" && !allowDatagrams
				if (err != nil) != wantFailure {
					t.Fatalf("gate error=%v, want failure=%t counts=%+v", err, wantFailure, counts)
				}
				if !wantFailure {
					path.setAllowProviderDatagramPackFailures(false)
					if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: counts}); err != nil {
						t.Fatalf("accounted overlap poisoned later boundary: %v", err)
					}
				}
			})
		}
	}
}

func TestProviderDatagramRecoveryOverlapRejectsInvalidEvidence(t *testing.T) {
	for _, counts := range []perfvarPackFailureCounts{
		{providerFailureCount: 1, providerRecoverableFailureCount: 1, providerRecoverableDatagramFailureCount: 1},
		{providerFailureCount: 1, providerDatagramFailureCount: 1, providerRecoverableDatagramFailureCount: 1},
		{providerFailureCount: 1, providerRecoverableFailureCount: 2, providerDatagramFailureCount: 2, providerRecoverableDatagramFailureCount: 2},
	} {
		path := &fullTunPath{activePackFailureFloor: &perfvarPackFailureCounts{}, allowProviderDatagramPackFailures: true}
		if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: counts}); err == nil {
			t.Fatalf("invalid overlap passed: %+v", counts)
		}
	}
	path := &fullTunPath{activePackFailureFloor: &perfvarPackFailureCounts{
		providerFailureCount: 1, providerRecoverableFailureCount: 1,
		providerDatagramFailureCount: 1, providerRecoverableDatagramFailureCount: 1}}
	if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: perfvarPackFailureCounts{
		providerFailureCount: 1, providerRecoverableFailureCount: 1, providerDatagramFailureCount: 1,
	}}); err == nil || !strings.Contains(err.Error(), "moved backward") {
		t.Fatalf("backward overlap error=%v", err)
	}
}
