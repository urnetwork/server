package proxy

import (
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
	DeviceCount                int
	TargetByteCount            sdk.ByteCount
	UsedByteCount              sdk.ByteCount
	PlatformBudgetByteCount    sdk.ByteCount
	PlatformUsedByteCount      sdk.ByteCount
	PlatformMaxTransportCount  int
	PlatformUsedTransportCount int
	PlatformPendingH1Count     int
	PlatformPendingH1ByteCount sdk.ByteCount
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
	prometheus.MustRegister(proxyWireGuardReturnBackpressureCounter)
	prometheus.MustRegister(proxyWireGuardReturnBackpressureDuration)
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
	for _, deviceLocal := range deviceLocals {
		deviceUsages = append(deviceUsages, deviceLocal.MemoryUsed())
	}
	return aggregateProxyDeviceMemoryUsage(deviceUsages)
}

// updateProxyDeviceMemoryGauges publishes one identity-free aggregate sample.
func updateProxyDeviceMemoryGauges(usage proxyDeviceMemoryUsage) {
	proxyDeviceMemoryTargetBytesGauge.Set(float64(usage.TargetByteCount))
	proxyDeviceMemoryUsedBytesGauge.Set(float64(usage.UsedByteCount))
	proxyPlatformTransportBudgetBytesGauge.Set(float64(usage.PlatformBudgetByteCount))
	proxyPlatformTransportUsedBytesGauge.Set(float64(usage.PlatformUsedByteCount))
	proxyPlatformTransportMaxGauge.Set(float64(usage.PlatformMaxTransportCount))
	proxyPlatformTransportUsedGauge.Set(float64(usage.PlatformUsedTransportCount))
	proxyPlatformTransportPendingH1Gauge.Set(float64(usage.PlatformPendingH1Count))
	proxyPlatformTransportPendingH1BytesGauge.Set(float64(usage.PlatformPendingH1ByteCount))
}
