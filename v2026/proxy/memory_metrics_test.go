package proxy

import (
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

// TestAggregateProxyDeviceMemoryUsagePreservesCarrierSaturationSignal proves
// every DeviceLocal contributes to the identity-free aggregate, including the
// pending-H1 signature that identified the main proxy outage.
func TestAggregateProxyDeviceMemoryUsagePreservesCarrierSaturationSignal(t *testing.T) {
	usage := aggregateProxyDeviceMemoryUsage([]*sdk.DeviceLocalMemoryUsage{
		{
			TargetByteCount:                  24 * 1024 * 1024,
			TotalByteCount:                   7 * 1024 * 1024,
			PlatformTransportBudgetByteCount: 6 * 1024 * 1024,
			PlatformTransportUsedByteCount:   4 * 1024 * 1024,
			PlatformTransportMaxCount:        16,
			PlatformTransportUsedCount:       16,
			PlatformTransportPendingH1Count:  9,
			PlatformTransportPendingH1Bytes:  3 * 1024 * 1024,
		},
		nil,
		{
			TargetByteCount:                  24 * 1024 * 1024,
			TotalByteCount:                   5 * 1024 * 1024,
			PlatformTransportBudgetByteCount: 6 * 1024 * 1024,
			PlatformTransportUsedByteCount:   2 * 1024 * 1024,
			PlatformTransportMaxCount:        16,
			PlatformTransportUsedCount:       8,
			PlatformTransportPendingH1Count:  0,
			PlatformTransportPendingH1Bytes:  0,
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
		usage.PlatformPendingH1ByteCount != 3*1024*1024 {
		t.Fatalf("aggregate usage = %+v", usage)
	}
}
