// Pins coherent scrape publication and lifetime accounting with explicit samples.
package proxy

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/sdk/v2026"
)

// Preserves each device's contribution to capacity and pending admission.
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

// Keeps observed deltas monotonic across device removal, replacement, and reset.
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

// Pauses collection after its first value, publishes a different aggregate,
// then proves every remaining value belongs to the captured sample.
func TestProxyDeviceMemoryMetricsCollectsOneSnapshot(t *testing.T) {
	metrics := &proxyDeviceMemoryMetrics{}
	metrics.update(proxyDeviceMemoryUsage{
		DeviceCount:                          1,
		TargetByteCount:                      24 * 1024 * 1024,
		UsedByteCount:                        7 * 1024 * 1024,
		PlatformBudgetByteCount:              6 * 1024 * 1024,
		PlatformUsedByteCount:                2 * 1024 * 1024,
		PlatformMaxTransportCount:            16,
		PlatformUsedTransportCount:           16,
		PlatformPendingH1Count:               1,
		PlatformPendingH1ByteCount:           256 * 1024,
		PlatformSlotFullPendingH1DeviceCount: 1,
		PlatformHandoffTransportCount:        1,
		PlatformHandoffByteCount:             256 * 1024,
		PlatformSlotFullHandoffDeviceCount:   1,
		PlatformSlotFullHandoffUnsatisfied:   1,
		PlatformH3PreemptionDelta:            4,
		PlatformSlotFullH3PreemptionDelta:    2,
	})
	metricChannel := make(chan prometheus.Metric)
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		defer close(metricChannel)
		metrics.Collect(metricChannel)
	}()
	snapshot := []prometheus.Metric{<-metricChannel}
	metrics.update(proxyDeviceMemoryUsage{
		DeviceCount:                2,
		TargetByteCount:            48 * 1024 * 1024,
		UsedByteCount:              10 * 1024 * 1024,
		PlatformBudgetByteCount:    12 * 1024 * 1024,
		PlatformUsedByteCount:      3 * 1024 * 1024,
		PlatformMaxTransportCount:  32,
		PlatformUsedTransportCount: 8,
		PlatformH3PreemptionDelta:  3,
	})
	for metric := range metricChannel {
		snapshot = append(snapshot, metric)
	}
	<-collected

	checkSnapshot := func(snapshot []prometheus.Metric, expectedValues map[*prometheus.Desc]float64) {
		t.Helper()
		if len(snapshot) != len(expectedValues) {
			t.Fatalf("snapshot has %d families, want %d", len(snapshot), len(expectedValues))
		}
		for _, metric := range snapshot {
			desc := metric.Desc()
			expected, ok := expectedValues[desc]
			if !ok {
				t.Fatalf("unexpected or duplicate family: %s", desc)
			}
			delete(expectedValues, desc)
			value := &dto.Metric{}
			if err := metric.Write(value); err != nil {
				t.Fatal(err)
			}
			if len(value.Label) != 0 {
				t.Fatalf("memory metric acquired identity labels: %s", desc)
			}
			observed := value.GetGauge().GetValue()
			if value.Counter != nil {
				observed = value.GetCounter().GetValue()
			}
			if observed != expected {
				t.Errorf("%s = %v, want %v from one snapshot", desc, observed, expected)
			}
		}
	}
	checkSnapshot(snapshot, map[*prometheus.Desc]float64{
		devicesLiveDesc:                                      1,
		proxyDeviceMemoryTargetBytesDesc:                     24 * 1024 * 1024,
		proxyDeviceMemoryUsedBytesDesc:                       7 * 1024 * 1024,
		proxyPlatformTransportBudgetBytesDesc:                6 * 1024 * 1024,
		proxyPlatformTransportUsedBytesDesc:                  2 * 1024 * 1024,
		proxyPlatformTransportMaxDesc:                        16,
		proxyPlatformTransportUsedDesc:                       16,
		proxyPlatformTransportPendingH1Desc:                  1,
		proxyPlatformTransportPendingH1BytesDesc:             256 * 1024,
		proxyPlatformTransportSlotFullPendingH1DevicesDesc:   1,
		proxyPlatformTransportHandoffTransportsDesc:          1,
		proxyPlatformTransportHandoffBytesDesc:               256 * 1024,
		proxyPlatformTransportSlotFullHandoffDevicesDesc:     1,
		proxyPlatformTransportSlotFullHandoffUnsatisfiedDesc: 1,
		proxyPlatformTransportSlotFullDemandUnsatisfiedDesc:  0,
		proxyPlatformTransportSlotFullWindowUnknownDesc:      0,
		proxyPlatformTransportH3PreemptionsDesc:              4,
		proxyPlatformTransportSlotFullH3PreemptionsDesc:      2,
	})

	metricChannel = make(chan prometheus.Metric, 18)
	metrics.Collect(metricChannel)
	close(metricChannel)
	snapshot = nil
	for metric := range metricChannel {
		snapshot = append(snapshot, metric)
	}
	checkSnapshot(snapshot, map[*prometheus.Desc]float64{
		devicesLiveDesc:                                      2,
		proxyDeviceMemoryTargetBytesDesc:                     48 * 1024 * 1024,
		proxyDeviceMemoryUsedBytesDesc:                       10 * 1024 * 1024,
		proxyPlatformTransportBudgetBytesDesc:                12 * 1024 * 1024,
		proxyPlatformTransportUsedBytesDesc:                  3 * 1024 * 1024,
		proxyPlatformTransportMaxDesc:                        32,
		proxyPlatformTransportUsedDesc:                       8,
		proxyPlatformTransportPendingH1Desc:                  0,
		proxyPlatformTransportPendingH1BytesDesc:             0,
		proxyPlatformTransportSlotFullPendingH1DevicesDesc:   0,
		proxyPlatformTransportHandoffTransportsDesc:          0,
		proxyPlatformTransportHandoffBytesDesc:               0,
		proxyPlatformTransportSlotFullHandoffDevicesDesc:     0,
		proxyPlatformTransportSlotFullHandoffUnsatisfiedDesc: 0,
		proxyPlatformTransportSlotFullDemandUnsatisfiedDesc:  0,
		proxyPlatformTransportSlotFullWindowUnknownDesc:      0,
		proxyPlatformTransportH3PreemptionsDesc:              7,
		proxyPlatformTransportSlotFullH3PreemptionsDesc:      2,
	})
}

// A sibling's handoff or readiness must never explain this device's full cap.
func TestAggregateProxyDeviceMemoryUsageJoinsHandoffAndWindowAtOwner(t *testing.T) {
	for _, test := range []struct {
		name                                                    string
		active, pending                                         int
		known, ready                                            bool
		handoff, handoffUnsatisfied, demandUnsatisfied, unknown int
	}{
		{"demand", 0, 0, true, false, 0, 0, 1, 0},
		{"active-handoff", 1, 0, true, false, 1, 1, 0, 0},
		{"queued-handoff", 0, 2, true, false, 1, 1, 0, 0},
		{"ready-handoff", 1, 0, true, true, 1, 0, 0, 0},
		{"unknown-window", 1, 0, false, false, 1, 0, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := aggregateProxyDeviceMemoryUsage([]*sdk.DeviceLocalMemoryUsage{
				{
					PlatformTransportMaxCount: 16, PlatformTransportUsedCount: 16,
					PlatformTransportPendingH1Count: 2, PlatformTransportPendingH1Bytes: 512 * 1024,
					PlatformTransportActiveHandoffCount:  test.active,
					PlatformTransportPendingHandoffCount: test.pending,
					ProviderWindowKnown:                  test.known, ProviderWindowMinSatisfied: test.ready,
				},
				{
					// Healthy, unrelated owner with an active handoff.
					PlatformTransportMaxCount: 16, PlatformTransportUsedCount: 1,
					PlatformTransportActiveHandoffCount: 1,
					PlatformTransportHandoffCount:       1, PlatformTransportHandoffByteCount: 256 * 1024,
					ProviderWindowKnown: true, ProviderWindowMinSatisfied: true,
				},
				nil,
			})
			if usage.PlatformSlotFullPendingH1DeviceCount != 1 ||
				usage.PlatformSlotFullHandoffDeviceCount != test.handoff ||
				usage.PlatformSlotFullHandoffUnsatisfied != test.handoffUnsatisfied ||
				usage.PlatformSlotFullDemandUnsatisfied != test.demandUnsatisfied ||
				usage.PlatformSlotFullWindowUnknown != test.unknown ||
				usage.PlatformHandoffTransportCount != 1 || usage.PlatformHandoffByteCount != 256*1024 {
				t.Fatalf("same-owner aggregate = %+v", usage)
			}
		})
	}
}

// Six new gauges remain six unlabeled series regardless of device population;
// a reset/new process starts from zero rather than retaining predecessor state.
func TestProxyDeviceMemoryAdmissionMetricsHaveFixedCardinalityAndReset(t *testing.T) {
	for _, devices := range []int{0, 1, 1000} {
		metrics := &proxyDeviceMemoryMetrics{}
		metrics.update(proxyDeviceMemoryUsage{
			DeviceCount: devices, PlatformSlotFullPendingH1DeviceCount: devices,
			PlatformSlotFullDemandUnsatisfied: devices,
		})
		registry := prometheus.NewPedanticRegistry()
		registry.MustRegister(metrics)
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		if len(families) != 18 {
			t.Fatalf("families=%d for devices=%d", len(families), devices)
		}
		for _, family := range families {
			if len(family.Metric) != 1 || len(family.Metric[0].Label) != 0 {
				t.Fatalf("identity/cardinality leak in %s", family.GetName())
			}
		}
		metrics.update(proxyDeviceMemoryUsage{})
		families, err = registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if value := family.Metric[0].GetGauge(); value != nil && value.GetValue() != 0 {
				t.Fatalf("reset retained gauge %s", family.GetName())
			}
		}
	}
}
