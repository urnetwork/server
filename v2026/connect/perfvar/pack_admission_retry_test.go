package perfvar

import (
	"context"
	"errors"
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
