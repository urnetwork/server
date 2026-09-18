// Publishes coherent device-memory snapshots and bounded process-lifetime counters.
package proxy

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sdk/v2026"
)

var devicesLiveDesc = prometheus.NewDesc(
	"urnetwork_proxy_devices_live",
	"Proxy ids with an installed embedded device on this instance",
	nil, nil,
)

var proxyDeviceMemoryTargetBytesDesc = prometheus.NewDesc(
	"urnetwork_proxy_device_memory_target_bytes",
	"Sum of steady-state memory targets for installed proxy DeviceLocals",
	nil, nil,
)

var proxyDeviceMemoryUsedBytesDesc = prometheus.NewDesc(
	"urnetwork_proxy_device_memory_tracked_used_bytes",
	"Sum of live tracked memory use for installed proxy DeviceLocals",
	nil, nil,
)

var proxyPlatformTransportBudgetBytesDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_budget_bytes",
	"Sum of private platform-carrier byte budgets for installed proxy DeviceLocals",
	nil, nil,
)

var proxyPlatformTransportUsedBytesDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_used_bytes",
	"Sum of acquired platform-carrier bytes for installed proxy DeviceLocals",
	nil, nil,
)

var proxyPlatformTransportMaxDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transports_max",
	"Sum of private platform-carrier count limits for installed proxy DeviceLocals",
	nil, nil,
)

var proxyPlatformTransportUsedDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transports_used",
	"Sum of acquired platform carriers for installed proxy DeviceLocals",
	nil, nil,
)

var proxyPlatformTransportPendingH1Desc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transports_pending_h1",
	"H1 carriers waiting for private DeviceLocal admission",
	nil, nil,
)

var proxyPlatformTransportPendingH1BytesDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transports_pending_h1_bytes",
	"H1 carrier bytes waiting for private DeviceLocal admission",
	nil, nil,
)

var proxyPlatformTransportSlotFullPendingH1DevicesDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_slot_full_pending_h1_devices",
	"DeviceLocals with H1 admission pending while their private carrier-count cap is full",
	nil, nil,
)

var proxyPlatformTransportHandoffTransportsDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_active_handoff_transports",
	"Temporary carrier-count allowance owned by active private-budget H1/H3 handoffs",
	nil, nil,
)

var proxyPlatformTransportHandoffBytesDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_active_handoff_bytes",
	"Temporary byte allowance owned by active private-budget H1/H3 handoffs",
	nil, nil,
)

var proxyPlatformTransportSlotFullHandoffDevicesDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_slot_full_pending_h1_handoff_devices",
	"Slot-full pending DeviceLocals whose own budget has a pending or active policy handoff",
	nil, nil,
)

var proxyPlatformTransportSlotFullHandoffUnsatisfiedDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_slot_full_pending_h1_handoff_unsatisfied_devices",
	"Slot-full pending DeviceLocals with a same-budget handoff and a known unsatisfied provider window",
	nil, nil,
)

var proxyPlatformTransportSlotFullDemandUnsatisfiedDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_slot_full_pending_h1_demand_unsatisfied_devices",
	"Slot-full pending DeviceLocals without a same-budget handoff and with a known unsatisfied provider window",
	nil, nil,
)

var proxyPlatformTransportSlotFullWindowUnknownDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_slot_full_pending_h1_window_unknown_devices",
	"Slot-full pending DeviceLocals with no current provider-window readiness sample",
	nil, nil,
)

var proxyPlatformTransportH3PreemptionsDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_h3_preemptions_total",
	"H3 carrier leases preempted for H1 admission across hosted DeviceLocals",
	nil, nil,
)

var proxyPlatformTransportSlotFullH3PreemptionsDesc = prometheus.NewDesc(
	"urnetwork_proxy_platform_transport_slot_full_pending_h1_h3_preemptions_total",
	"H3 preemption deltas sampled from DeviceLocals whose H1 waits at a full carrier-count cap",
	nil, nil,
)

var proxyLockCacheEntriesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "lock_cache_entries",
		Help:      "Proxy caller IP-lock cache entries retained by this process",
	},
)

var proxyLockCacheCapacityGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "lock_cache_capacity",
		Help:      "Hard maximum Proxy caller IP-lock cache entries retained by this process",
	},
)

var proxyLockCacheHitsCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "lock_cache_hits_total",
		Help:      "Fresh Proxy caller IP-lock cache lookups served without loading configuration",
	},
)

var proxyLockCacheMissesCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "lock_cache_misses_total",
		Help:      "Proxy caller IP-lock cache lookups that required configuration loading",
	},
)

var proxyLockCacheExpirationsCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "lock_cache_expirations_total",
		Help:      "Expired Proxy caller IP-lock entries removed from this process",
	},
)

var proxyLockCacheEvictionsCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "lock_cache_evictions_total",
		Help:      "Proxy caller IP-lock entries evicted by the hard LRU capacity",
	},
)

var proxyWireGuardReturnBackpressureCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "wireguard_return_backpressure_total",
		Help:      "WireGuard return packets that waited for the bounded process receive queue",
	},
)

var proxyWireGuardReturnBackpressureDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "wireguard_return_backpressure_seconds",
		Help:      "Time WireGuard return packets waited for bounded process receive-queue capacity",
		Buckets:   prometheus.ExponentialBuckets(0.000_001, 4, 12),
	},
)

var proxyDeviceMemoryBudgetBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "device_memory_budget_bytes",
		Help:      "Aggregate device memory budget admitted against by this instance",
	},
)

var proxyDeviceMemoryBudgetUsedBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "device_memory_budget_used_bytes",
		Help:      "Reserved bytes of the aggregate device memory budget",
	},
)

var proxyDeviceAdmissionRefusedCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "device_admission_refused_total",
		Help:      "Device opens refused because the aggregate device memory budget was exhausted",
	},
)

// Holds one instance-wide sample without per-customer labels or proxy ids.
type proxyDeviceMemoryUsage struct {
	DeviceCount                          int
	TargetByteCount                      sdk.ByteCount
	UsedByteCount                        sdk.ByteCount
	PlatformBudgetByteCount              sdk.ByteCount
	PlatformUsedByteCount                sdk.ByteCount
	PlatformMaxTransportCount            int
	PlatformUsedTransportCount           int
	PlatformPendingH1Count               int
	PlatformPendingH1ByteCount           sdk.ByteCount
	PlatformSlotFullPendingH1DeviceCount int
	PlatformHandoffTransportCount        int
	PlatformHandoffByteCount             sdk.ByteCount
	PlatformSlotFullHandoffDeviceCount   int
	PlatformSlotFullHandoffUnsatisfied   int
	PlatformSlotFullDemandUnsatisfied    int
	PlatformSlotFullWindowUnknown        int
	PlatformH3PreemptionDelta            int64
	PlatformSlotFullH3PreemptionDelta    int64
}

// Pairs a device's lifetime count with its state at the current sample.
type proxyPlatformPreemptionSample struct {
	count             int64
	slotFullPendingH1 bool
}

// Accumulates observed device increments without retaining removed devices.
// The slot-full subset describes sample-time state, not each event's state.
type proxyPlatformPreemptionTracker struct {
	stateLock         sync.Mutex
	deviceLocalCounts map[*sdk.DeviceLocal]int64
}

// Serializes delta accounting; new or reset epochs do not imply slot-full history.
func (self *proxyPlatformPreemptionTracker) observe(
	deviceLocalSamples map[*sdk.DeviceLocal]proxyPlatformPreemptionSample,
) (delta int64, slotFullDelta int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.deviceLocalCounts == nil {
		self.deviceLocalCounts = map[*sdk.DeviceLocal]int64{}
	}
	for deviceLocal, sample := range deviceLocalSamples {
		count := sample.count
		previous, ok := self.deviceLocalCounts[deviceLocal]
		var deviceDelta int64
		joinedToCurrentEpoch := ok && previous <= count
		if !ok || count < previous {
			// A newly observed DeviceLocal contributes its complete process-lifetime
			// count. A same-object decrease is defensive reset handling: preserve
			// monotonic exporter semantics and start the new epoch at count.
			deviceDelta = count
		} else {
			deviceDelta = count - previous
		}
		delta += deviceDelta
		if joinedToCurrentEpoch && sample.slotFullPendingH1 {
			slotFullDelta += deviceDelta
		}
		self.deviceLocalCounts[deviceLocal] = count
	}
	for deviceLocal := range self.deviceLocalCounts {
		if _, ok := deviceLocalSamples[deviceLocal]; !ok {
			delete(self.deviceLocalCounts, deviceLocal)
		}
	}
	return delta, slotFullDelta
}

// Keeps every memory gauge and preemption counter in one scrape snapshot.
// Safe for concurrent updates and collection; the zero value is an empty sample.
type proxyDeviceMemoryMetrics struct {
	stateLock             sync.Mutex
	usage                 proxyDeviceMemoryUsage
	h3Preemptions         float64
	slotFullH3Preemptions float64
}

var defaultProxyDeviceMemoryMetrics = &proxyDeviceMemoryMetrics{}

// Describes the fixed, identity-free families emitted by this collector.
func (self *proxyDeviceMemoryMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		devicesLiveDesc,
		proxyDeviceMemoryTargetBytesDesc,
		proxyDeviceMemoryUsedBytesDesc,
		proxyPlatformTransportBudgetBytesDesc,
		proxyPlatformTransportUsedBytesDesc,
		proxyPlatformTransportMaxDesc,
		proxyPlatformTransportUsedDesc,
		proxyPlatformTransportPendingH1Desc,
		proxyPlatformTransportPendingH1BytesDesc,
		proxyPlatformTransportSlotFullPendingH1DevicesDesc,
		proxyPlatformTransportHandoffTransportsDesc,
		proxyPlatformTransportHandoffBytesDesc,
		proxyPlatformTransportSlotFullHandoffDevicesDesc,
		proxyPlatformTransportSlotFullHandoffUnsatisfiedDesc,
		proxyPlatformTransportSlotFullDemandUnsatisfiedDesc,
		proxyPlatformTransportSlotFullWindowUnknownDesc,
		proxyPlatformTransportH3PreemptionsDesc,
		proxyPlatformTransportSlotFullH3PreemptionsDesc,
	} {
		ch <- desc
	}
}

// Captures all fields before the first send, so a concurrent update cannot mix
// device counts, capacities, or counters within one producer scrape.
func (self *proxyDeviceMemoryMetrics) Collect(ch chan<- prometheus.Metric) {
	usage, h3Preemptions, slotFullH3Preemptions := func() (proxyDeviceMemoryUsage, float64, float64) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.usage, self.h3Preemptions, self.slotFullH3Preemptions
	}()
	ch <- prometheus.MustNewConstMetric(devicesLiveDesc, prometheus.GaugeValue, float64(usage.DeviceCount))
	ch <- prometheus.MustNewConstMetric(proxyDeviceMemoryTargetBytesDesc, prometheus.GaugeValue, float64(usage.TargetByteCount))
	ch <- prometheus.MustNewConstMetric(proxyDeviceMemoryUsedBytesDesc, prometheus.GaugeValue, float64(usage.UsedByteCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportBudgetBytesDesc, prometheus.GaugeValue, float64(usage.PlatformBudgetByteCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportUsedBytesDesc, prometheus.GaugeValue, float64(usage.PlatformUsedByteCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportMaxDesc, prometheus.GaugeValue, float64(usage.PlatformMaxTransportCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportUsedDesc, prometheus.GaugeValue, float64(usage.PlatformUsedTransportCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportPendingH1Desc, prometheus.GaugeValue, float64(usage.PlatformPendingH1Count))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportPendingH1BytesDesc, prometheus.GaugeValue, float64(usage.PlatformPendingH1ByteCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportSlotFullPendingH1DevicesDesc, prometheus.GaugeValue, float64(usage.PlatformSlotFullPendingH1DeviceCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportHandoffTransportsDesc, prometheus.GaugeValue, float64(usage.PlatformHandoffTransportCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportHandoffBytesDesc, prometheus.GaugeValue, float64(usage.PlatformHandoffByteCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportSlotFullHandoffDevicesDesc, prometheus.GaugeValue, float64(usage.PlatformSlotFullHandoffDeviceCount))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportSlotFullHandoffUnsatisfiedDesc, prometheus.GaugeValue, float64(usage.PlatformSlotFullHandoffUnsatisfied))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportSlotFullDemandUnsatisfiedDesc, prometheus.GaugeValue, float64(usage.PlatformSlotFullDemandUnsatisfied))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportSlotFullWindowUnknownDesc, prometheus.GaugeValue, float64(usage.PlatformSlotFullWindowUnknown))
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportH3PreemptionsDesc, prometheus.CounterValue, h3Preemptions)
	ch <- prometheus.MustNewConstMetric(proxyPlatformTransportSlotFullH3PreemptionsDesc, prometheus.CounterValue, slotFullH3Preemptions)
}

// Publishes one aggregate and retains its deltas in process-lifetime counters.
func (self *proxyDeviceMemoryMetrics) update(usage proxyDeviceMemoryUsage) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.usage = usage
	self.h3Preemptions += float64(usage.PlatformH3PreemptionDelta)
	self.slotFullH3Preemptions += float64(usage.PlatformSlotFullH3PreemptionDelta)
}

// Registers one coherent memory collector and the independent cache/return metrics.
func init() {
	prometheus.MustRegister(defaultProxyDeviceMemoryMetrics)
	prometheus.MustRegister(proxyLockCacheEntriesGauge)
	prometheus.MustRegister(proxyLockCacheCapacityGauge)
	prometheus.MustRegister(proxyLockCacheHitsCounter)
	prometheus.MustRegister(proxyLockCacheMissesCounter)
	prometheus.MustRegister(proxyLockCacheExpirationsCounter)
	prometheus.MustRegister(proxyLockCacheEvictionsCounter)
	prometheus.MustRegister(proxyWireGuardReturnBackpressureCounter)
	prometheus.MustRegister(proxyWireGuardReturnBackpressureDuration)
	prometheus.MustRegister(proxyDeviceMemoryBudgetBytesGauge)
	prometheus.MustRegister(proxyDeviceMemoryBudgetUsedBytesGauge)
	prometheus.MustRegister(proxyDeviceAdmissionRefusedCounter)
	proxyLockCacheCapacityGauge.Set(proxyLockCacheMaxEntries)
}

// Reduces independently sampled devices without retaining customer identity
// or mutable sdk objects.
func aggregateProxyDeviceMemoryUsage(
	deviceUsages []*sdk.DeviceLocalMemoryUsage,
) proxyDeviceMemoryUsage {
	aggregate := proxyDeviceMemoryUsage{}
	for _, usage := range deviceUsages {
		if usage == nil {
			continue
		}
		aggregate.DeviceCount += 1
		aggregate.TargetByteCount += usage.TargetByteCount
		aggregate.UsedByteCount += usage.TotalByteCount
		aggregate.PlatformBudgetByteCount += usage.PlatformTransportBudgetByteCount
		aggregate.PlatformUsedByteCount += usage.PlatformTransportUsedByteCount
		aggregate.PlatformMaxTransportCount += usage.PlatformTransportMaxCount
		aggregate.PlatformUsedTransportCount += usage.PlatformTransportUsedCount
		aggregate.PlatformPendingH1Count += usage.PlatformTransportPendingH1Count
		aggregate.PlatformPendingH1ByteCount += usage.PlatformTransportPendingH1Bytes
		aggregate.PlatformHandoffTransportCount += usage.PlatformTransportHandoffCount
		aggregate.PlatformHandoffByteCount += usage.PlatformTransportHandoffByteCount
		if 0 < usage.PlatformTransportPendingH1Count &&
			0 < usage.PlatformTransportMaxCount &&
			usage.PlatformTransportMaxCount <= usage.PlatformTransportUsedCount {
			aggregate.PlatformSlotFullPendingH1DeviceCount++
			handoff := usage.PlatformTransportPendingHandoffCount > 0 || usage.PlatformTransportActiveHandoffCount > 0
			if handoff {
				aggregate.PlatformSlotFullHandoffDeviceCount++
			}
			switch {
			case !usage.ProviderWindowKnown:
				aggregate.PlatformSlotFullWindowUnknown++
			case !usage.ProviderWindowMinSatisfied && handoff:
				aggregate.PlatformSlotFullHandoffUnsatisfied++
			case !usage.ProviderWindowMinSatisfied:
				aggregate.PlatformSlotFullDemandUnsatisfied++
			}
		}
	}
	return aggregate
}

// Samples installed devices after releasing the manager map locks. Each device
// owns synchronization for its sample; the activity flusher consumes each delta once.
func (self *ProxyDeviceManager) DeviceMemoryUsage() proxyDeviceMemoryUsage {
	deviceLocals := func() []*sdk.DeviceLocal {
		self.stateLock.RLock()
		defer self.stateLock.RUnlock()
		result := make([]*sdk.DeviceLocal, 0, len(self.proxyDevices))
		for _, state := range self.proxyDevices {
			state.StateLock.Lock()
			proxyDevice := state.ProxyDevice
			state.StateLock.Unlock()
			if proxyDevice != nil && proxyDevice.deviceLocal != nil {
				result = append(result, proxyDevice.deviceLocal)
			}
		}
		return result
	}()

	deviceUsages := make([]*sdk.DeviceLocalMemoryUsage, 0, len(deviceLocals))
	preemptions := make(map[*sdk.DeviceLocal]proxyPlatformPreemptionSample, len(deviceLocals))
	for _, deviceLocal := range deviceLocals {
		usage := deviceLocal.MemoryUsed()
		deviceUsages = append(deviceUsages, usage)
		preemptions[deviceLocal] = proxyPlatformPreemptionSample{
			count: usage.PlatformTransportPreemptedH3Count,
			slotFullPendingH1: 0 < usage.PlatformTransportPendingH1Count &&
				0 < usage.PlatformTransportMaxCount &&
				usage.PlatformTransportMaxCount <= usage.PlatformTransportUsedCount,
		}
	}
	usage := aggregateProxyDeviceMemoryUsage(deviceUsages)
	usage.PlatformH3PreemptionDelta, usage.PlatformSlotFullH3PreemptionDelta =
		self.platformPreemptions.observe(preemptions)
	return usage
}

// Publishes one identity-free aggregate through the coherent memory collector.
func updateProxyDeviceMemoryGauges(usage proxyDeviceMemoryUsage) {
	defaultProxyDeviceMemoryMetrics.update(usage)
}
