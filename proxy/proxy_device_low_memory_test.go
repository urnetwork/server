// Pins hosted low-profile selection and per-device ownership without changing host policy.
package proxy

import (
	"context"
	"reflect"
	"runtime/metrics"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Missing configuration uses the same target as the explicit SDK low profile.
func TestProxyDeviceLowMemoryDefaults(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("{}\n"))
	defer popConfig()
	manager := DefaultProxyDeviceManagerSettings()
	device := DefaultProxyDeviceSettings()
	if manager.DeviceMemoryTargetByteCount != 32*model.Mib ||
		device.MemoryTargetByteCount != 32*model.Mib ||
		sdk.DefaultLowMemoryDeviceLocalSettings().MemoryTargetByteCount != 32*model.Mib {
		t.Fatalf("low-profile defaults: manager=%d device=%d sdk=%d; want 32 MiB",
			manager.DeviceMemoryTargetByteCount, device.MemoryTargetByteCount,
			sdk.DefaultLowMemoryDeviceLocalSettings().MemoryTargetByteCount)
	}
}

// A numeric target alone must not leave a Linux-hosted device on the normal policy.
func TestProxyDeviceLowMemoryExplicitProfileOnHostedConstructor(t *testing.T) {
	settings := DefaultProxyDeviceSettings()
	settings.MemoryTargetByteCount = 32 * model.Mib
	settings.DisableWindowIdentityPersistence = true
	local := newProxyDeviceLocalSettings(context.Background(), &model.ProxyDeviceConfig{}, settings)
	if !local.LowMemoryProfile || local.MemoryTargetByteCount != 32*model.Mib {
		t.Fatalf("hosted policy: low=%t target=%d; want explicit low profile and 32 MiB",
			local.LowMemoryProfile, local.MemoryTargetByteCount)
	}
	if !local.HostedIncompatible || !local.DisableLogging {
		t.Fatal("low-profile selection changed the hosted safety/logging policy")
	}
}

// Positive configured targets scale one owner but never opt it out of the low profile.
func TestProxyDeviceLowMemoryConfiguredTargetPreservesProfile(t *testing.T) {
	for _, shape := range []struct {
		value  string
		target model.ByteCount
	}{
		{value: "24MiB", target: 24 * model.Mib},
		{value: "48MiB", target: 48 * model.Mib},
	} {
		func() {
			popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("device_memory_budget: "+shape.value+"\n"))
			defer popConfig()
			manager := DefaultProxyDeviceManagerSettings()
			settings := DefaultProxyDeviceSettingsWithBufferSize(manager.SequenceBufferSize)
			settings.MemoryTargetByteCount = manager.DeviceMemoryTargetByteCount
			settings.DisableWindowIdentityPersistence = true
			local := newProxyDeviceLocalSettings(context.Background(), &model.ProxyDeviceConfig{}, settings)
			if manager.DeviceMemoryTargetByteCount != shape.target ||
				local.MemoryTargetByteCount != shape.target || !local.LowMemoryProfile {
				t.Fatalf("override %s: manager=%d local=%d low=%t",
					shape.value, manager.DeviceMemoryTargetByteCount, local.MemoryTargetByteCount, local.LowMemoryProfile)
			}
		}()
	}
}

// Direct construction preserves fail-fast refusal of disabled or negative targets.
func TestProxyDeviceLowMemoryRefusesNonpositiveTargets(t *testing.T) {
	for _, target := range []model.ByteCount{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("target %d did not refuse construction", target)
				}
			}()
			settings := DefaultProxyDeviceSettings()
			settings.MemoryTargetByteCount = target
			settings.DisableWindowIdentityPersistence = true
			_ = newProxyDeviceLocalSettings(context.Background(), &model.ProxyDeviceConfig{}, settings)
		}()
	}
}

// The real hosted constructor creates two live low-profile owners; closing one
// cannot retire the other. Exact reservation independence is also tested inside SDK.
func TestProxyDeviceLowMemoryConstructedOwnersRemainIndependent(t *testing.T) {
	first, firstCleanup := newProxyDeviceTransportTestDevice(t)
	var firstOnce sync.Once
	closeFirst := func() { firstOnce.Do(firstCleanup) }
	t.Cleanup(closeFirst)
	second, secondCleanup := newProxyDeviceTransportTestDevice(t)
	var secondOnce sync.Once
	closeSecond := func() { secondOnce.Do(secondCleanup) }
	t.Cleanup(closeSecond)
	for index, device := range []*sdk.DeviceLocal{first, second} {
		usage := device.MemoryUsed()
		if usage.TargetByteCount != 32*model.Mib ||
			usage.TransferRootBudgetByteCount != 21810380 ||
			usage.PackQueueCapacityByteCount != 2181038 ||
			usage.PlatformTransportBudgetByteCount != 8*model.Mib {
			t.Fatalf("device %d did not construct the owned 32 MiB low profile: %+v", index, usage)
		}
	}
	before := second.MemoryUsed()
	closeFirst()
	after := second.MemoryUsed()
	if second.GetDone() ||
		after.TargetByteCount != before.TargetByteCount ||
		after.TransferRootBudgetByteCount != before.TransferRootBudgetByteCount ||
		after.PackQueueCapacityByteCount != before.PackQueueCapacityByteCount ||
		after.PlatformTransportBudgetByteCount != before.PlatformTransportBudgetByteCount {
		t.Fatalf("first owner retirement changed the second: done=%t before=%+v after=%+v",
			second.GetDone(), before, after)
	}
}

// Construction and teardown may use pools, but cannot retune their capacities,
// the normal Connect sizing target, or the process Go soft limit.
func TestProxyDeviceLowMemoryConstructionLeavesProcessPolicyUnchanged(t *testing.T) {
	snapshot := func() (int64, uint64, map[int]int) {
		samples := []metrics.Sample{{Name: "/gc/gomemlimit:bytes"}}
		metrics.Read(samples)
		if samples[0].Value.Kind() != metrics.KindUint64 {
			t.Fatal("Go memory-limit metric is unavailable")
		}
		capacities := map[int]int{}
		for _, class := range connect.GetMessagePoolClassStats() {
			capacities[class.Size] = class.Capacity
		}
		return connect.MemoryBudget(), samples[0].Value.Uint64(), capacities
	}
	beforeTarget, beforeLimit, beforePools := snapshot()
	_, closeDevice := newProxyDeviceTransportTestDevice(t)
	closeDevice()
	afterTarget, afterLimit, afterPools := snapshot()
	if afterTarget != beforeTarget || afterLimit != beforeLimit || !reflect.DeepEqual(afterPools, beforePools) {
		t.Fatalf("per-device construction changed process policy: target=%d/%d limit=%d/%d pools=%v/%v",
			beforeTarget, afterTarget, beforeLimit, afterLimit, beforePools, afterPools)
	}
}
