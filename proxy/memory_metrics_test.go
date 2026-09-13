package proxy

import (
	"testing"

	"github.com/urnetwork/sdk"
)

// TestAggregateProxyDeviceMemoryUsagePreservesCarrierSaturationSignal proves
// every DeviceLocal contributes to the identity-free aggregate, including the
// pending-H1 signature that identified the main proxy outage.
func TestAggregateProxyDeviceMemoryUsagePreservesCarrierSaturationSignal(t *testing.T) {
	usage := aggregateProxyDeviceMemoryUsage([]*sdk.DeviceLocalMemoryUsage{
		{
			TargetByteCount:                   24 * 1024 * 1024,
			TotalByteCount:                    7 * 1024 * 1024,
			PlatformTransportBudgetByteCount:  6 * 1024 * 1024,
			PlatformTransportUsedByteCount:    4 * 1024 * 1024,
			PlatformTransportMaxCount:         16,
			PlatformTransportUsedCount:        16,
			PlatformTransportPendingH1Count:   9,
			PlatformTransportPendingH1Bytes:   3 * 1024 * 1024,
			PlatformTransportPreemptedH3Count: 7,
		},
		nil,
		{
			TargetByteCount:                   24 * 1024 * 1024,
			TotalByteCount:                    5 * 1024 * 1024,
			PlatformTransportBudgetByteCount:  6 * 1024 * 1024,
			PlatformTransportUsedByteCount:    2 * 1024 * 1024,
			PlatformTransportMaxCount:         16,
			PlatformTransportUsedCount:        8,
			PlatformTransportPendingH1Count:   0,
			PlatformTransportPendingH1Bytes:   0,
			PlatformTransportPreemptedH3Count: 2,
		},
	})

	if usage.DeviceCount != 2 ||
		usage.TargetByteCount != 48*1024*1024 ||
		usage.UsedByteCount != 12*1024*1024 ||
		usage.PlatformBudgetByteCount != 12*1024*1024 ||
		usage.PlatformUsedByteCount != 6*1024*1024 ||
		usage.PlatformMaxTransportCount != 32 ||
		usage.PlatformUsedTransportCount != 24 ||
		usage.PlatformPendingH1Count != 9 ||
		usage.PlatformPendingH1ByteCount != 3*1024*1024 ||
		usage.PlatformSlotFullPendingH1DeviceCount != 1 {
		t.Fatalf("aggregate usage = %+v", usage)
	}
}

func TestProxyPlatformPreemptionTrackerPreservesMonotonicProcessCounter(t *testing.T) {
	tracker := &proxyPlatformPreemptionTracker{}
	first := new(sdk.DeviceLocal)
	second := new(sdk.DeviceLocal)

	delta, slotFullDelta := tracker.observe(map[*sdk.DeviceLocal]proxyPlatformPreemptionSample{
		first:  {count: 4, slotFullPendingH1: true},
		second: {count: 2},
	})
	if delta != 6 || slotFullDelta != 0 {
		t.Fatalf("initial deltas = %d/%d, want 6/0", delta, slotFullDelta)
	}
	delta, slotFullDelta = tracker.observe(map[*sdk.DeviceLocal]proxyPlatformPreemptionSample{
		first:  {count: 7},
		second: {count: 2, slotFullPendingH1: true},
	})
	if delta != 3 || slotFullDelta != 0 {
		t.Fatalf("incremental deltas = %d/%d, want 3/0", delta, slotFullDelta)
	}
	delta, slotFullDelta = tracker.observe(map[*sdk.DeviceLocal]proxyPlatformPreemptionSample{
		second: {count: 2, slotFullPendingH1: true},
	})
	if delta != 0 || slotFullDelta != 0 {
		t.Fatalf("removed device changed the process counters by %d/%d", delta, slotFullDelta)
	}
	third := new(sdk.DeviceLocal)
	delta, slotFullDelta = tracker.observe(map[*sdk.DeviceLocal]proxyPlatformPreemptionSample{
		second: {count: 1, slotFullPendingH1: true},
		third:  {count: 5, slotFullPendingH1: true},
	})
	if delta != 6 || slotFullDelta != 0 {
		t.Fatalf("new/reset epoch deltas = %d/%d, want 6/0", delta, slotFullDelta)
	}
	delta, slotFullDelta = tracker.observe(map[*sdk.DeviceLocal]proxyPlatformPreemptionSample{
		second: {count: 3, slotFullPendingH1: true},
		third:  {count: 7, slotFullPendingH1: true},
	})
	if delta != 4 || slotFullDelta != 4 {
		t.Fatalf("joined current-epoch deltas = %d/%d, want 4/4", delta, slotFullDelta)
	}
}
