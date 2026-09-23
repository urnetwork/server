// The aggregate device memory budget: admission bounds the SUM of installed
// device targets, which is the only term that was previously unbounded by the
// process (MaxClients is 1<<16 and per-client TCP flows are uncapped).
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

// budgetTestManager builds a manager with an explicit aggregate budget and no
// database or network space.
func budgetTestManager(
	t *testing.T,
	ctx context.Context,
	budgetByteCount model.ByteCount,
) *ProxyDeviceManager {
	t.Helper()
	settings := DefaultProxyDeviceManagerSettings()
	settings.NetworkSpace = &sdk.NetworkSpace{}
	settings.DeviceMemoryTargetByteCount = 24 * model.Mib
	settings.ProcessDeviceMemoryBudgetByteCount = budgetByteCount
	return NewProxyDeviceManager(ctx, settings)
}

// Supplies the same initialized lifecycle owners that the manager starts in
// production. A bare ProxyDevice lets HandleError hide a nil receiver panic
// and release the reservation before the lifetime assertion can observe it.
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
		ctx:            deviceCtx,
		cancel:         deviceCancel,
		deviceLocal:    deviceLocal,
		deviceState:    deviceLocal,
		tun:            tun,
		settings:       DefaultProxyDeviceSettings(),
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

// Even cancellation before the worker starts still registers the receive
// callback. Run directly so a malformed fixture cannot pass through recovery.
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

// The idle watcher must not interpret the zero timestamp as an expired device
// and release the reservation before the lifetime test requests shutdown.
func TestProxyDeviceMemoryBudgetFixtureStartsWithActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := budgetTestProxyDevice(t, ctx)
	if device.CancelIfIdle() {
		t.Fatal("new fixture was already idle")
	}
}

// A successful fixture borrows the manager lifetime so CloseAndWait can join
// its packet worker without depending on the test's later caller cancellation.
func TestProxyDeviceMemoryBudgetFixtureFollowsManagerLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx, 24*model.Mib)
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

// The per-device target is unchanged at 24 MiB, and the aggregate default is
// the process's other declared ceiling (the 8 GiB message pool budget).
func TestProxyDeviceMemoryBudgetKeepsThePerDeviceTarget(t *testing.T) {
	settings := DefaultProxyDeviceManagerSettings()
	if settings.DeviceMemoryTargetByteCount != 24*model.Mib {
		t.Fatalf("per-device target = %d, want 24 MiB", settings.DeviceMemoryTargetByteCount)
	}
	if got := defaultProxyProcessDeviceMemoryBudgetByteCount; got != 8*model.Gib {
		t.Fatalf("default aggregate = %d, want 8 GiB", got)
	}
	if devices := defaultProxyProcessDeviceMemoryBudgetByteCount / (24 * model.Mib); devices != 341 {
		t.Fatalf("default aggregate admits %d devices, want 341", devices)
	}
}

// Admission never overcommits: reservations stop exactly at the aggregate and
// the used total never exceeds it.
func TestProxyDeviceMemoryBudgetNeverExceedsTheAggregate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const deviceCount = 4
	manager := budgetTestManager(t, ctx, deviceCount*24*model.Mib)
	total := connect.ByteCount(deviceCount * 24 * model.Mib)

	for i := range deviceCount {
		if !manager.tryReserveDeviceMemory() {
			t.Fatalf("admission %d refused below the aggregate", i)
		}
		if used := manager.deviceMemoryBudget.UsedByteCount(); used > total {
			t.Fatalf("used %d exceeds aggregate %d", used, total)
		}
	}
	if manager.deviceMemoryBudget.UsedByteCount() != total {
		t.Fatalf("used = %d, want the full aggregate %d", manager.deviceMemoryBudget.UsedByteCount(), total)
	}
	// the device after the last affordable one is refused, not overcommitted
	for range 3 {
		if manager.tryReserveDeviceMemory() {
			t.Fatal("admission overcommitted the aggregate")
		}
		if used := manager.deviceMemoryBudget.UsedByteCount(); used != total {
			t.Fatalf("refused admission changed used to %d", used)
		}
	}
	// releasing one device admits exactly one more
	manager.releaseDeviceMemory()
	if used := manager.deviceMemoryBudget.UsedByteCount(); used != total-connect.ByteCount(24*model.Mib) {
		t.Fatalf("release left used = %d", used)
	}
	if !manager.tryReserveDeviceMemory() {
		t.Fatal("released capacity was not reusable")
	}
	if manager.tryReserveDeviceMemory() {
		t.Fatal("admission exceeded the aggregate after a release")
	}
}

// An exhausted budget refuses the open without constructing a device, so no
// gVisor stack or tun is allocated for a device that cannot be afforded.
func TestProxyDeviceMemoryBudgetRefusesWithoutConstructing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx, 24*model.Mib)
	var builderCalls atomic.Int64
	manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
		builderCalls.Add(1)
		return nil, errors.New("refused open reached device construction")
	}

	if !manager.tryReserveDeviceMemory() {
		t.Fatal("first reservation refused")
	}
	pd, err := manager.OpenProxyDevice(server.NewId())
	if !errors.Is(err, ErrProxyDeviceMemoryBudget) {
		t.Fatalf("open err = %v, want ErrProxyDeviceMemoryBudget", err)
	}
	if pd != nil {
		t.Fatal("a refused open returned a device")
	}
	if calls := builderCalls.Load(); calls != 0 {
		t.Fatalf("a refused open constructed %d devices", calls)
	}
	if used := manager.deviceMemoryBudget.UsedByteCount(); used != connect.ByteCount(24*model.Mib) {
		t.Fatalf("refused open changed used to %d", used)
	}
}

// A failed construction releases its reservation rather than leaking it.
func TestProxyDeviceMemoryBudgetReleasedOnConstructionFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx, 24*model.Mib)
	manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
		return nil, errors.New("device does not exist")
	}
	if _, err := manager.OpenProxyDevice(server.NewId()); err == nil {
		t.Fatal("construction failure returned no error")
	}
	if used := manager.deviceMemoryBudget.UsedByteCount(); used != 0 {
		t.Fatalf("construction failure leaked %d reserved bytes", used)
	}
	// the capacity is reusable, so one failure does not permanently shrink it
	if !manager.tryReserveDeviceMemory() {
		t.Fatal("capacity was not released back to the aggregate")
	}
}

// An installed device holds its reservation for its lifetime and releases it
// when it closes.
func TestProxyDeviceMemoryBudgetReleasedOnDeviceClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx, 24*model.Mib)
	manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
		return budgetTestProxyDevice(t, manager.ctx), nil
	}
	device, err := manager.OpenProxyDevice(server.NewId())
	if err != nil {
		t.Fatalf("open err = %v", err)
	}
	if !device.Active() {
		t.Fatal("installed fixture has no active device lifetime")
	}
	if used := manager.deviceMemoryBudget.UsedByteCount(); used != connect.ByteCount(24*model.Mib) {
		t.Fatalf("installed device holds %d reserved bytes, want its target", used)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer closeCancel()
	manager.Close()
	select {
	case <-device.Done():
	default:
		t.Fatal("installed fixture does not borrow its manager lifetime")
	}
	if err := manager.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("close err = %v", err)
	}
	if used := manager.deviceMemoryBudget.UsedByteCount(); used != 0 {
		t.Fatalf("closed device left %d reserved bytes", used)
	}
}

// A zero budget disables admission control, preserving the previous behaviour
// for an environment that sets process_device_memory_budget to 0.
func TestProxyDeviceMemoryBudgetDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx, 0)
	if manager.deviceMemoryBudget != nil {
		t.Fatal("a zero budget built an admission budget")
	}
	for i := range 1000 {
		if !manager.tryReserveDeviceMemory() {
			t.Fatalf("disabled budget refused admission %d", i)
		}
	}
}
