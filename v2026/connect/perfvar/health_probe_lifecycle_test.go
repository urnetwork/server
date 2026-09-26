package perfvar

import (
	"context"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// Health probes are IP packets too. The exact producer marker, not protocol,
// successful application bytes, or the error text, separates their ownership.
func TestSendPackLifecycleHealthProbeKeepsDiagnosticFailure(t *testing.T) {
	for _, probe := range []bool{false, true} {
		tracker := newSendPackLifecycleTracker()
		defer tracker.close()
		observer := tracker.newObserver()
		observation := clientconnect.SendPackLifecycleObservation{
			ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId(),
			Token: 1, AckRequired: true, MessageType: protocol.MessageType_IpIpPacketToProvider,
			HealthProbe: probe,
		}
		observation.Phase = clientconnect.SendPackLifecyclePhaseStarted
		observer(observation)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		workload, ok := tracker.workloadBoundary(ctx)
		if !ok || len(workload.entries) != int(workload.startedCount) ||
			(probe && workload.startedCount != 0) || (!probe && workload.startedCount != 1) {
			t.Fatalf("probe=%t workload boundary=%+v ok=%t", probe, workload, ok)
		}
		all, ok := tracker.boundary(ctx)
		if !ok || len(all.entries) != 1 || all.startedCount != 1 {
			t.Fatalf("probe=%t diagnostic boundary lost live Pack: %+v", probe, all)
		}
		for _, phase := range []clientconnect.SendPackLifecyclePhase{
			clientconnect.SendPackLifecyclePhaseFirstRouteWrite, clientconnect.SendPackLifecyclePhaseTerminal,
		} {
			observation.Phase, observation.Err = phase, clientconnect.ErrSendPackNotAdmitted
			observer(observation)
		}
		if !tracker.waitThrough(ctx, all) || tracker.invalid.Load() || tracker.failures.Load() != 1 ||
			len(tracker.failureSnapshot()) != 1 {
			t.Fatalf("probe=%t diagnostic failure was suppressed or invalid", probe)
		}
		if (probe && (tracker.healthProbeStarted.Load() != 1 || tracker.healthProbeFailures.Load() != 1)) ||
			(!probe && (tracker.healthProbeStarted.Load() != 0 || tracker.healthProbeFailures.Load() != 0)) {
			t.Fatalf("probe=%t separate diagnostic started/failures=%d/%d", probe,
				tracker.healthProbeStarted.Load(), tracker.healthProbeFailures.Load())
		}
		path := &fullTunPath{devicePackSends: tracker, activePackFailureFloor: &perfvarPackFailureCounts{}}
		err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: snapshotPerfvarPackFailures(path)})
		if (err == nil) != probe {
			t.Fatalf("probe=%t measured failure=%v", probe, err)
		}
	}
}

func TestSendPackLifecycleHealthProbeMarkerIsImmutable(t *testing.T) {
	for _, probeAtStart := range []bool{false, true} {
		tracker := newSendPackLifecycleTracker()
		defer tracker.close()
		observer := tracker.newObserver()
		observation := clientconnect.SendPackLifecycleObservation{
			ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId(),
			Token: 1, AckRequired: true, MessageType: protocol.MessageType_IpIpPacketToProvider,
			Phase: clientconnect.SendPackLifecyclePhaseStarted, HealthProbe: probeAtStart,
		}
		observer(observation)
		observation.Phase = clientconnect.SendPackLifecyclePhaseFirstRouteWrite
		observation.HealthProbe = !probeAtStart
		observer(observation)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		tracker.boundary(ctx)
		if !tracker.invalid.Load() {
			t.Fatalf("probe-at-start=%t accepted a changed ownership marker", probeAtStart)
		}
	}
}
