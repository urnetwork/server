package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server/model"
)

// A hosted device's windows stop refilling after ten minutes without user
// traffic, while the device itself is still held for its 45 minutes, and every
// hosted device's windows get the hold.
func TestProxyDeviceIdleRefillHoldDefaults(t *testing.T) {
	for _, settings := range []*ProxyDeviceSettings{DefaultProxyDeviceSettings(), DefaultProxyDeviceSettingsWithBufferSize(64)} {
		if settings.IdleRefillHold != 10*time.Minute {
			t.Fatalf("idle refill hold = %s, want 10m", settings.IdleRefillHold)
		}
		if settings.ProxyDeviceIdleTimeout != 45*time.Minute {
			t.Fatalf("idle hold = %s, want 45m", settings.ProxyDeviceIdleTimeout)
		}
		local := newProxyDeviceLocalSettings(context.Background(), &model.ProxyDeviceConfig{}, settings)
		if local.IdleRefillHold != settings.IdleRefillHold {
			t.Fatalf("the device's windows hold their refill after %s, want %s", local.IdleRefillHold, settings.IdleRefillHold)
		}
	}
	// the hold is shorter than the device's own idle hold, so it has a window
	// to act in
	if DefaultProxyDeviceSettings().ProxyDeviceIdleTimeout <= DefaultProxyDeviceSettings().IdleRefillHold {
		t.Fatal("the idle refill hold is not shorter than the device's idle hold")
	}
}

// The windows read the device's record of user traffic: what the tun sends on
// its own (the DoH keepalive pings it sends through the device) leaves an idle
// device idle to them, as it does to the reaper, and relay traffic is activity.
func TestProxyDeviceWindowsReadTheDeviceRecordOfUserTraffic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := budgetTestProxyDevice(t, ctx)
	var activityDevice atomic.Pointer[ProxyDevice]
	userActivitySince := proxyDeviceUserActivitySince(&activityDevice)

	// while the device is being created it reads as active
	if !userActivitySince(time.Now()) {
		t.Fatal("a device still being created reads as idle")
	}
	activityDevice.Store(device)

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
	time.Sleep(100 * time.Millisecond)
	if userActivitySince(time.Now().Add(-DefaultProxyDeviceSettings().IdleRefillHold)) {
		t.Fatal("tun upkeep made an hour-idle device active to its windows")
	}

	// user traffic is activity to the windows
	device.UpdateActivity()
	if !userActivitySince(time.Now().Add(-time.Second)) {
		t.Fatal("user traffic did not make the device active to its windows")
	}
}

// Only the first activity after a gap signals the windows, so a busy relay
// costs one swap per update, and the gap is far shorter than any idleness a
// window waits out, so that first activity always signals a waiting window.
func TestProxyDeviceActivitySignalsTheWindowsAfterAGap(t *testing.T) {
	now := time.Now().UnixNano()
	if proxyDeviceActivityWakesWindows(now-int64(time.Millisecond), now) {
		t.Fatal("activity a millisecond after the last signals the windows")
	}
	if !proxyDeviceActivityWakesWindows(now-int64(proxyDeviceActivityWakeGap), now) {
		t.Fatal("activity after the gap does not signal the windows")
	}
	if !proxyDeviceActivityWakesWindows(now-int64(time.Hour), now) {
		t.Fatal("activity after an hour does not signal the windows")
	}
	if connect.DefaultMultiClientSettings().BalanceRestoreSpreadIdle <= proxyDeviceActivityWakeGap ||
		DefaultProxyDeviceSettings().IdleRefillHold <= proxyDeviceActivityWakeGap {
		t.Fatal("a window can wait for less idleness than the gap that signals it")
	}

	// a device's activity update records the traffic and signals its windows
	// without failing, with or without a multi client
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := budgetTestProxyDevice(t, ctx)
	device.lastActivityNanos.Store(time.Now().Add(-time.Hour).UnixNano())
	if !device.UpdateActivity() {
		t.Fatal("a live device refused an activity update")
	}
	if time.Minute < proxyDeviceIdleFor(device) {
		t.Fatal("the activity update did not record the traffic")
	}
}
