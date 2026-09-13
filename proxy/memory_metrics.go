package proxy

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sdk"
)

var proxyDeviceMemoryTargetBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "device_memory_target_bytes",
		Help:      "Sum of steady-state memory targets for installed proxy DeviceLocals",
	},
)

var proxyDeviceMemoryUsedBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "device_memory_tracked_used_bytes",
		Help:      "Sum of live tracked memory use for installed proxy DeviceLocals",
	},
)

var proxyPlatformTransportBudgetBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "platform_transport_budget_bytes",
		Help:      "Sum of private platform-carrier byte budgets for installed proxy DeviceLocals",
	},
)

var proxyPlatformTransportUsedBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "platform_transport_used_bytes",
		Help:      "Sum of acquired platform-carrier bytes for installed proxy DeviceLocals",
	},
)

var proxyPlatformTransportMaxGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "platform_transports_max",
		Help:      "Sum of private platform-carrier count limits for installed proxy DeviceLocals",
	},
)

var proxyPlatformTransportUsedGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "platform_transports_used",
		Help:      "Sum of acquired platform carriers for installed proxy DeviceLocals",
	},
)

var proxyPlatformTransportPendingH1Gauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "platform_transports_pending_h1",
		Help:      "H1 carriers waiting for private DeviceLocal admission",
	},
)

var proxyPlatformTransportPendingH1BytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "platform_transports_pending_h1_bytes",
		Help:      "H1 carrier bytes waiting for private DeviceLocal admission",
	},
)

var proxyPlatformTransportSlotFullPendingH1DevicesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "platform_transport_slot_full_pending_h1_devices",
		Help:      "DeviceLocals with H1 admission pending while their private carrier-count cap is full",
	},
)

var proxyPlatformTransportH3PreemptionsCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "platform_transport_h3_preemptions_total",
		Help:      "H3 carrier leases preempted for H1 admission across hosted DeviceLocals",
	},
)

var proxyPlatformTransportSlotFullH3PreemptionsCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "platform_transport_slot_full_pending_h1_h3_preemptions_total",
		Help:      "H3 preemption deltas sampled from DeviceLocals whose H1 waits at a full carrier-count cap",
	},
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

// proxyDeviceMemoryUsage is one instance-wide sample without per-customer
// labels. It preserves operational visibility without exporting proxy ids.
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
	PlatformH3PreemptionDelta            int64
	PlatformSlotFullH3PreemptionDelta    int64
}

type proxyPlatformPreemptionSample struct {
	count             int64
	slotFullPendingH1 bool
}

// proxyPlatformPreemptionTracker converts the independently monotonic
// DeviceLocal counters into one process-monotonic Prometheus counter. Removed
// DeviceLocals are forgotten only after their final observed increments have
// been retained in the process counter.
type proxyPlatformPreemptionTracker struct {
	stateLock sync.Mutex
	observed  map[*sdk.DeviceLocal]int64
}

func (t *proxyPlatformPreemptionTracker) observe(
	current map[*sdk.DeviceLocal]proxyPlatformPreemptionSample,
) (delta int64, slotFullDelta int64) {
	t.stateLock.Lock()
	defer t.stateLock.Unlock()

	if t.observed == nil {
		t.observed = map[*sdk.DeviceLocal]int64{}
	}
	for deviceLocal, sample := range current {
		count := sample.count
		previous, ok := t.observed[deviceLocal]
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
		t.observed[deviceLocal] = count
	}
	for deviceLocal := range t.observed {
		if _, ok := current[deviceLocal]; !ok {
			delete(t.observed, deviceLocal)
		}
	}
	return delta, slotFullDelta
}

func init() {
	prometheus.MustRegister(proxyDeviceMemoryTargetBytesGauge)
	prometheus.MustRegister(proxyDeviceMemoryUsedBytesGauge)
	prometheus.MustRegister(proxyPlatformTransportBudgetBytesGauge)
	prometheus.MustRegister(proxyPlatformTransportUsedBytesGauge)
	prometheus.MustRegister(proxyPlatformTransportMaxGauge)
	prometheus.MustRegister(proxyPlatformTransportUsedGauge)
	prometheus.MustRegister(proxyPlatformTransportPendingH1Gauge)
	prometheus.MustRegister(proxyPlatformTransportPendingH1BytesGauge)
	prometheus.MustRegister(proxyPlatformTransportSlotFullPendingH1DevicesGauge)
	prometheus.MustRegister(proxyPlatformTransportH3PreemptionsCounter)
	prometheus.MustRegister(proxyPlatformTransportSlotFullH3PreemptionsCounter)
	prometheus.MustRegister(proxyLockCacheEntriesGauge)
	prometheus.MustRegister(proxyLockCacheCapacityGauge)
	prometheus.MustRegister(proxyLockCacheHitsCounter)
	prometheus.MustRegister(proxyLockCacheMissesCounter)
	prometheus.MustRegister(proxyLockCacheExpirationsCounter)
	prometheus.MustRegister(proxyLockCacheEvictionsCounter)
	prometheus.MustRegister(proxyWireGuardReturnBackpressureCounter)
	prometheus.MustRegister(proxyWireGuardReturnBackpressureDuration)
	proxyLockCacheCapacityGauge.Set(proxyLockCacheMaxEntries)
}

// aggregateProxyDeviceMemoryUsage reduces independently sampled DeviceLocals
// without retaining customer identity or mutable SDK objects.
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
		if 0 < usage.PlatformTransportPendingH1Count &&
			0 < usage.PlatformTransportMaxCount &&
			usage.PlatformTransportMaxCount <= usage.PlatformTransportUsedCount {
			aggregate.PlatformSlotFullPendingH1DeviceCount++
		}
	}
	return aggregate
}

// DeviceMemoryUsage samples every installed DeviceLocal after releasing the
// manager map locks. DeviceLocal owns synchronization for its internal sample.
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

// updateProxyDeviceMemoryGauges publishes one identity-free aggregate sample.
func updateProxyDeviceMemoryGauges(usage proxyDeviceMemoryUsage) {
	devicesLiveGauge.Set(float64(usage.DeviceCount))
	proxyDeviceMemoryTargetBytesGauge.Set(float64(usage.TargetByteCount))
	proxyDeviceMemoryUsedBytesGauge.Set(float64(usage.UsedByteCount))
	proxyPlatformTransportBudgetBytesGauge.Set(float64(usage.PlatformBudgetByteCount))
	proxyPlatformTransportUsedBytesGauge.Set(float64(usage.PlatformUsedByteCount))
	proxyPlatformTransportMaxGauge.Set(float64(usage.PlatformMaxTransportCount))
	proxyPlatformTransportUsedGauge.Set(float64(usage.PlatformUsedTransportCount))
	proxyPlatformTransportPendingH1Gauge.Set(float64(usage.PlatformPendingH1Count))
	proxyPlatformTransportPendingH1BytesGauge.Set(float64(usage.PlatformPendingH1ByteCount))
	proxyPlatformTransportSlotFullPendingH1DevicesGauge.Set(float64(usage.PlatformSlotFullPendingH1DeviceCount))
	proxyPlatformTransportH3PreemptionsCounter.Add(float64(usage.PlatformH3PreemptionDelta))
	proxyPlatformTransportSlotFullH3PreemptionsCounter.Add(float64(usage.PlatformSlotFullH3PreemptionDelta))
}
