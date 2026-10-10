package proxy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func budgetTestManager(t *testing.T, ctx context.Context) *ProxyDeviceManager {
	t.Helper()
	settings := DefaultProxyDeviceManagerSettings()
	settings.NetworkSpace = &sdk.NetworkSpace{}
	settings.DeviceMemoryTargetByteCount = 24 * model.Mib
	return NewProxyDeviceManager(ctx, settings)
}

func budgetTestProxyDevice(t *testing.T, ctx context.Context) *ProxyDevice {
	t.Helper()
	deviceLocal, closeDevice := newProxyDeviceTransportTestDevice(t)
	t.Cleanup(closeDevice)
	deviceCtx, deviceCancel := context.WithCancel(ctx)
	t.Cleanup(deviceCancel)
	tun, err := connect.CreateTunWithDefaults(deviceCtx)
	if err != nil {
		t.Fatal(err)
	}
	device := &ProxyDevice{
		ctx: deviceCtx, cancel: deviceCancel, deviceLocal: deviceLocal,
		deviceState: deviceLocal, tun: tun, settings: DefaultProxyDeviceSettings(),
		receiveMonitor: connect.NewMonitor(),
	}
	device.UpdateActivity()
	t.Cleanup(func() {
		if err := device.Close(); err != nil {
			t.Error(err)
		}
	})
	return device
}

func TestProxyDeviceMemoryBudgetFixtureRunAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := budgetTestProxyDevice(t, ctx)
	cancel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("canceled fixture Run panicked: %v", r)
		}
	}()
	device.Run()
}

func TestProxyDeviceMemoryBudgetFixtureStartsWithActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := budgetTestProxyDevice(t, ctx)
	if device.CancelIfIdle() {
		t.Fatal("new fixture was already idle")
	}
}

func TestProxyDeviceMemoryBudgetFixtureFollowsManagerLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx)
	device := budgetTestProxyDevice(t, manager.ctx)
	if err := manager.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-device.Done():
	default:
		t.Fatal("manager shutdown retained the fixture lifetime")
	}
}

func TestProxyDeviceMemoryBudgetIsPerDevice(t *testing.T) {
	settings := DefaultProxyDeviceManagerSettings()
	if settings.DeviceMemoryTargetByteCount != 24*model.Mib {
		t.Fatalf("per-device target = %d, want 24 MiB", settings.DeviceMemoryTargetByteCount)
	}
	first, closeFirst := newProxyDeviceTransportTestDevice(t)
	defer closeFirst()
	second, closeSecond := newProxyDeviceTransportTestDevice(t)
	defer closeSecond()
	for i, device := range []*sdk.DeviceLocal{first, second} {
		usage := device.MemoryUsed()
		if usage.TargetByteCount != sdk.ByteCount(24*model.Mib) {
			t.Fatalf("device %d target = %d, want 24 MiB", i, usage.TargetByteCount)
		}
		if usage.PlatformTransportBudgetByteCount <= 0 || usage.PlatformTransportBudgetByteCount >= usage.TargetByteCount {
			t.Fatalf("device %d carrier budget = %d, target = %d", i, usage.PlatformTransportBudgetByteCount, usage.TargetByteCount)
		}
	}
}

// Separate proxy IDs both reach construction while the first remains active.
// A former manager-wide reservation rejected the second at its limit.
func TestProxyDeviceMemoryBudgetDoesNotRejectAnotherDevice(t *testing.T) {
	// Under the former manager gate this synthetic one-device process ceiling
	// rejected the second open before its private DeviceLocal was constructed.
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte(
		"device_memory_budget: 24MiB\nprocess_device_memory_budget: 24MiB\n",
	))
	defer popConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx)
	var constructed atomic.Int64
	manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
		constructed.Add(1)
		return budgetTestProxyDevice(t, manager.ctx), nil
	}
	first, err := manager.OpenProxyDevice(server.NewId())
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.OpenProxyDevice(server.NewId())
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !first.Active() || !second.Active() || constructed.Load() != 2 {
		t.Fatalf("independent opens: first=%p second=%p constructed=%d", first, second, constructed.Load())
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer closeCancel()
	if err := manager.CloseAndWait(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestProxyDeviceConstructionFailureDoesNotGateNextDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx)
	var calls atomic.Int64
	manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("synthetic construction failure")
		}
		return budgetTestProxyDevice(t, manager.ctx), nil
	}
	failedID := server.NewId()
	if _, err := manager.OpenProxyDevice(failedID); err == nil {
		t.Fatal("first construction unexpectedly succeeded")
	}
	manager.stateLock.RLock()
	_, retained := manager.proxyDevices[failedID]
	manager.stateLock.RUnlock()
	if retained {
		t.Fatal("failed construction retained an empty per-ID manager entry")
	}
	if _, err := manager.OpenProxyDevice(server.NewId()); err != nil {
		t.Fatalf("independent construction was gated: %v", err)
	}
	if err := manager.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProxyDeviceStateRetainsConcurrentOpenersUntilLastRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx)
	id := server.NewId()
	state := &proxyDeviceState{}
	state.users.Store(2)
	manager.proxyDevices[id] = state

	manager.releaseProxyDeviceState(id, state)
	if manager.proxyDevices[id] != state || state.users.Load() != 1 {
		t.Fatal("first failed opener detached state retained by a concurrent opener")
	}
	manager.releaseProxyDeviceState(id, state)
	if _, ok := manager.proxyDevices[id]; ok {
		t.Fatal("last failed opener retained empty manager state")
	}
	if err := manager.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
