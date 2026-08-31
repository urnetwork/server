package proxy

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	proxyconnect "github.com/urnetwork/proxy"
)

func TestUpdateWgRuntimeMetricsPublishesSharedReceiverFailures(t *testing.T) {
	t.Cleanup(func() {
		updateWgRuntimeMetrics(proxyconnect.WgRuntimeStats{})
	})

	updateWgRuntimeMetrics(proxyconnect.WgRuntimeStats{
		InboundPeerQueueDropPacketCount:       23,
		InboundDecryptionQueueDropPacketCount: 17,
		ReceiveRoutineFailureCount:            2,
	})
	if got := testutil.ToFloat64(wgInboundPeerQueueDropPacketsGauge); got != 23 {
		t.Fatalf("wg peer queue drop packets = %v, want 23", got)
	}
	if got := testutil.ToFloat64(wgReceiveRoutineFailuresGauge); got != 2 {
		t.Fatalf("wg receive routine failures = %v, want 2", got)
	}
	if got := testutil.ToFloat64(wgInboundDecryptionQueueDropPacketsGauge); got != 17 {
		t.Fatalf("wg decryption queue drop packets = %v, want 17", got)
	}
}
