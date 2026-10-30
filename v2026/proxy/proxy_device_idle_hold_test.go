package proxy

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

func proxyDeviceIdleFor(device *ProxyDevice) time.Duration {
	return time.Since(time.Unix(0, device.lastActivityNanos.Load()))
}

// A device is held 45 minutes past its last user traffic.
func TestProxyDeviceIdleHoldIs45Minutes(t *testing.T) {
	if hold := DefaultProxyDeviceSettings().ProxyDeviceIdleTimeout; hold != 45*time.Minute {
		t.Fatalf("idle hold = %s, want 45m", hold)
	}
	if hold := DefaultProxyDeviceSettingsWithBufferSize(64).ProxyDeviceIdleTimeout; hold != 45*time.Minute {
		t.Fatalf("idle hold with a buffer size = %s, want 45m", hold)
	}
}

// What the device's tun sends on its own -- the DoH resolver's keepalive
// pings, its lookups -- and the returns that answer it are upkeep, not user
// traffic: an otherwise idle device is still reaped.
func TestProxyDeviceTunUpkeepDoesNotHoldAnIdleDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := budgetTestProxyDevice(t, ctx)
	idleSince := time.Now().Add(-time.Hour)
	device.lastActivityNanos.Store(idleSince.UnixNano())

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		device.Run()
	}()
	defer func() {
		device.Cancel()
		<-runDone
	}()

	// the tun's own egress, read by Run, and a return to the tun
	upkeep, err := device.tun.DialContext(ctx, "udp", "192.0.2.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer upkeep.Close()
	for range 8 {
		if _, err := upkeep.Write([]byte("keepalive")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	device.deliverReturnPackets([][]byte{make([]byte, 64)})
	time.Sleep(100 * time.Millisecond)

	if !time.Unix(0, device.lastActivityNanos.Load()).Equal(time.Unix(0, idleSince.UnixNano())) {
		t.Fatalf("tun upkeep refreshed the device's activity: idle for %s", proxyDeviceIdleFor(device))
	}
	if !device.CancelIfIdle() {
		t.Fatal("an hour without user traffic did not reap the device")
	}
}

// Bytes either way on an HTTP or SOCKS relay connection are user traffic:
// they hold the device they were dialed through.
func TestProxyRelayConnectionTrafficHoldsTheDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := budgetTestProxyDevice(t, ctx)

	relayConn, destinationConn := net.Pipe()
	defer destinationConn.Close()
	relay := instrumentProxyConnection("http", relayConn, device.UpdateActivity)
	defer relay.Close()

	// client to destination
	device.lastActivityNanos.Store(time.Now().Add(-time.Hour).UnixNano())
	go relay.Write([]byte("request"))
	request := make([]byte, 7)
	if _, err := destinationConn.Read(request); err != nil {
		t.Fatal(err)
	}
	waitForProxyDeviceActivity(t, device, "a relay write did not refresh the device's activity")

	// destination to client
	device.lastActivityNanos.Store(time.Now().Add(-time.Hour).UnixNano())
	go destinationConn.Write([]byte("response"))
	response := make([]byte, 8)
	if _, err := relay.Read(response); err != nil {
		t.Fatal(err)
	}
	waitForProxyDeviceActivity(t, device, "a relay read did not refresh the device's activity")
	if device.CancelIfIdle() {
		t.Fatal("a device moving relay bytes was reaped")
	}
}

func waitForProxyDeviceActivity(t *testing.T, device *ProxyDevice, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Minute < proxyDeviceIdleFor(device) {
		if deadline.Before(time.Now()) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

// Both relay dial paths report their connection's bytes to the device they
// dialed through.
func TestProxyRelayDialsReportActivity(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Count(string(source), "instrumentProxyConnection(")
	reporting := strings.Count(string(source), ", conn, pd.UpdateActivity)")
	if calls != 2 || reporting != calls {
		t.Fatalf("%d of %d relay dials report activity, want both of 2", reporting, calls)
	}
}

// The manager opens every device with its one shared scheduler-pause
// detector, so a host of thousands of devices runs one detector, not one per
// multi client.
func TestProxyDeviceManagerSharesOnePauseDetector(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx)
	defer manager.CloseAndWait(context.Background())

	first := manager.proxyDeviceSettings()
	second := manager.proxyDeviceSettings()
	if first.schedulerPauseDetector == nil || first.schedulerPauseDetector != second.schedulerPauseDetector {
		t.Fatal("the manager's devices do not share one pause detector")
	}
	first.DisableWindowIdentityPersistence = true
	deviceLocalSettings := newProxyDeviceLocalSettings(ctx, &model.ProxyDeviceConfig{}, first)
	if deviceLocalSettings.SchedulerPauseDetector != manager.schedulerPauseDetector {
		t.Fatal("a hosted device's settings do not carry the manager's pause detector")
	}
}
