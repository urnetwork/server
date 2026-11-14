package proxy

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/model"
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
	if device.carriedTraffic.Load() {
		t.Fatal("tun upkeep marked the device as having carried user traffic")
	}
	if !device.CancelIfIdle() {
		t.Fatal("an hour without user traffic did not reap the device")
	}
}

// Bytes either way on an HTTP or SOCKS relay connection are user traffic:
// they hold the device they were dialed through, and mark it used.
func TestProxyRelayConnectionTrafficHoldsTheDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := budgetTestProxyDevice(t, ctx)

	relayConn, destinationConn := net.Pipe()
	defer destinationConn.Close()
	relay := instrumentProxyConnection("http", relayConn, device.UpdateActivity)
	defer relay.Close()
	if device.carriedTraffic.Load() {
		t.Fatal("a relay connection that moved nothing marked its device used")
	}

	// client to destination
	device.lastActivityNanos.Store(time.Now().Add(-time.Hour).UnixNano())
	go relay.Write([]byte("request"))
	request := make([]byte, 7)
	if _, err := destinationConn.Read(request); err != nil {
		t.Fatal(err)
	}
	waitForProxyDeviceActivity(t, device, "a relay write did not refresh the device's activity")
	if !device.carriedTraffic.Load() {
		t.Fatal("a relay write did not mark the device as having carried traffic")
	}

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

// A device that has carried no user traffic since it opened is held ten
// minutes past its last activity, and one that has carried traffic the full
// 45. The manager's idle check runs often enough to close either on time.
func TestProxyDeviceUnusedIdleHoldIs10Minutes(t *testing.T) {
	for _, settings := range []*ProxyDeviceSettings{DefaultProxyDeviceSettings(), DefaultProxyDeviceSettingsWithBufferSize(64)} {
		if settings.ProxyDeviceUnusedIdleTimeout != 10*time.Minute {
			t.Fatalf("unused idle hold = %s, want 10m", settings.ProxyDeviceUnusedIdleTimeout)
		}
		if settings.ProxyDeviceIdleTimeout <= settings.ProxyDeviceUnusedIdleTimeout {
			t.Fatalf("the unused hold %s is not shorter than the hold %s", settings.ProxyDeviceUnusedIdleTimeout, settings.ProxyDeviceIdleTimeout)
		}
	}
	if checkTimeout := DefaultProxyDeviceManagerSettings().CheckProxyDeviceIdleTimeout; DefaultProxyDeviceSettings().ProxyDeviceUnusedIdleTimeout < 5*checkTimeout {
		t.Fatalf("the idle check every %s is coarse against the unused hold", checkTimeout)
	}
}

// An open of a live device is activity, not traffic: it refreshes the idle
// clock and leaves the device unused. User traffic marks it used, and nothing
// clears the mark.
func TestProxyDeviceOpenIsActivityNotTraffic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := budgetTestProxyDevice(t, ctx)
	if device.carriedTraffic.Load() {
		t.Fatal("a new device reads as having carried user traffic")
	}

	device.lastActivityNanos.Store(time.Now().Add(-time.Hour).UnixNano())
	if !device.noteOpen() {
		t.Fatal("a live device refused an open")
	}
	if time.Minute < proxyDeviceIdleFor(device) {
		t.Fatal("an open did not refresh the device's activity")
	}
	if device.carriedTraffic.Load() {
		t.Fatal("an open marked the device as having carried user traffic")
	}

	if !device.UpdateActivity() {
		t.Fatal("a live device refused user traffic")
	}
	device.noteOpen()
	if !device.carriedTraffic.Load() {
		t.Fatal("user traffic did not mark the device, or a later open cleared the mark")
	}

	device.Cancel()
	if device.noteOpen() || device.UpdateActivity() {
		t.Fatal("a done device recorded activity")
	}
}

// The idle close waits ProxyDeviceUnusedIdleTimeout for a device that carried
// nothing and ProxyDeviceIdleTimeout for one that carried traffic, and counts
// each close by which it was. A nonpositive unused hold gives an unused device
// the full hold.
func TestProxyDeviceIdleCloseHoldsByUse(t *testing.T) {
	newDevice := func(settings *ProxyDeviceSettings, carriedTraffic bool, idle time.Duration) *ProxyDevice {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		device := &ProxyDevice{ctx: ctx, cancel: cancel, settings: settings}
		device.carriedTraffic.Store(carriedTraffic)
		device.lastActivityNanos.Store(time.Now().Add(-idle).UnixNano())
		return device
	}
	unusedCloses := func() float64 {
		return testutil.ToFloat64(proxyDeviceIdleClosesUnusedCounter)
	}
	usedCloses := func() float64 {
		return testutil.ToFloat64(proxyDeviceIdleClosesUsedCounter)
	}

	settings := DefaultProxyDeviceSettings()
	if newDevice(settings, false, 9*time.Minute).CancelIfIdle() {
		t.Fatal("an unused device closed before its hold")
	}
	unusedBefore, usedBefore := unusedCloses(), usedCloses()
	if !newDevice(settings, false, 10*time.Minute+time.Second).CancelIfIdle() {
		t.Fatal("an unused device was held past ten minutes without activity")
	}
	if unusedCloses() != unusedBefore+1 || usedCloses() != usedBefore {
		t.Fatal("the unused close was not counted as unused")
	}

	if newDevice(settings, true, 44*time.Minute).CancelIfIdle() {
		t.Fatal("a used device closed before 45 minutes without activity")
	}
	unusedBefore, usedBefore = unusedCloses(), usedCloses()
	if !newDevice(settings, true, 45*time.Minute+time.Second).CancelIfIdle() {
		t.Fatal("a used device was held past 45 minutes without activity")
	}
	if usedCloses() != usedBefore+1 || unusedCloses() != unusedBefore {
		t.Fatal("the used close was not counted as used")
	}

	disabled := DefaultProxyDeviceSettings()
	disabled.ProxyDeviceUnusedIdleTimeout = 0
	if newDevice(disabled, false, 44*time.Minute).CancelIfIdle() {
		t.Fatal("with no unused hold, an unused device closed before the full hold")
	}
	if !newDevice(disabled, false, 45*time.Minute+time.Second).CancelIfIdle() {
		t.Fatal("with no unused hold, an unused device was held past the full hold")
	}

	// a hold set shorter than the unused one is the hold for both
	short := DefaultProxyDeviceSettings()
	short.ProxyDeviceIdleTimeout = 3 * time.Second
	if !newDevice(short, false, 4*time.Second).CancelIfIdle() || !newDevice(short, true, 4*time.Second).CancelIfIdle() {
		t.Fatal("a hold shorter than the unused hold did not close both devices")
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
