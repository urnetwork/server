package proxy

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// ActiveProxyIds reads only the installed devices' activity timestamps, so it
// can be tested against manager state without full device construction.
func TestActiveProxyIds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager := NewProxyDeviceManagerWithDefaults(ctx)

	newDevice := func(lastActivity time.Time) *ProxyDevice {
		pdCtx, pdCancel := context.WithCancel(ctx)
		pd := &ProxyDevice{
			ctx:    pdCtx,
			cancel: pdCancel,
		}
		pd.lastActivityNanos.Store(lastActivity.UnixNano())
		return pd
	}

	activeProxyId := server.NewId()
	staleProxyId := server.NewId()
	doneProxyId := server.NewId()
	emptyProxyId := server.NewId()

	activePd := newDevice(time.Now().Add(-1 * time.Minute))
	stalePd := newDevice(time.Now().Add(-30 * time.Minute))
	donePd := newDevice(time.Now())
	donePd.Cancel()

	manager.stateLock.Lock()
	manager.proxyDevices[activeProxyId] = &proxyDeviceState{ProxyDevice: activePd}
	manager.proxyDevices[staleProxyId] = &proxyDeviceState{ProxyDevice: stalePd}
	manager.proxyDevices[doneProxyId] = &proxyDeviceState{ProxyDevice: donePd}
	manager.proxyDevices[emptyProxyId] = &proxyDeviceState{}
	manager.stateLock.Unlock()

	proxyIds := manager.ActiveProxyIds(10 * time.Minute)
	if len(proxyIds) != 1 || proxyIds[0] != activeProxyId {
		t.Fatalf("active proxy ids = %v, want [%s]", proxyIds, activeProxyId)
	}

	// a wider window includes the stale device, still not the done/empty ones
	proxyIds = manager.ActiveProxyIds(60 * time.Minute)
	if len(proxyIds) != 2 {
		t.Fatalf("active proxy ids = %v, want 2", proxyIds)
	}
	if !slices.Contains(proxyIds, activeProxyId) || !slices.Contains(proxyIds, staleProxyId) {
		t.Fatalf("active proxy ids = %v", proxyIds)
	}

	if deviceCount := manager.DeviceCount(); deviceCount != 4 {
		t.Fatalf("device count = %d, want 4", deviceCount)
	}
}

// TestProxyPrewarm simulates the replacement instance of a deploy: the old
// instance recorded its active client into the activity set (here, directly);
// a FRESH device manager — the new container — pre-warms from the set and
// must end up holding a ready device for the client (PROXYDRAIN1.md §3.3).
func TestProxyPrewarm(t *testing.T) {
	if testing.Short() {
		return
	}
	// see TestProxy: fixed ports cannot be rebound on a rerun in the same process
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyPrewarm\n")
		h := setupProxyTest(t)
		defer h.cancel()

		// the "old instance" records the client active on this (host, block)
		proxyHost := fmt.Sprintf("prewarmtest%d", time.Now().UnixNano()%1000000)
		block := "g1"
		os.Setenv("WARP_HOST", proxyHost)
		os.Setenv("WARP_BLOCK", block)
		defer os.Unsetenv("WARP_HOST")
		defer os.Setenv("WARP_BLOCK", "test")
		model.TouchProxyClientActivity(h.ctx, proxyHost, block, server.NowUtc(), h.proxyId)

		// the "new container": a fresh manager with no devices
		pdmSettings := DefaultProxyDeviceManagerSettings()
		pdmSettings.NetworkSpace = h.networkSpace
		freshManager := NewProxyDeviceManager(h.ctx, pdmSettings)
		defer freshManager.Close()
		if deviceCount := freshManager.DeviceCount(); deviceCount != 0 {
			t.Fatalf("fresh manager device count = %d, want 0", deviceCount)
		}

		settings := DefaultProxySettings()
		settings.PrewarmReadyTimeout = 60 * time.Second
		settings.PrewarmTimeout = 120 * time.Second

		readyCount := Prewarm(h.ctx, freshManager, settings)
		if readyCount != 1 {
			t.Fatalf("prewarm ready count = %d, want 1", readyCount)
		}

		// the device is installed and ready before any client packet arrives
		pd, err := freshManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("open pre-warmed device: %v", err)
		}
		if !pd.Active() {
			t.Fatalf("pre-warmed device is not active")
		}
		if !pd.WaitForReady(h.ctx, 1*time.Second) {
			t.Fatalf("pre-warmed device is not ready")
		}
		fmt.Printf("[progress]prewarm e2e complete\n")
	})
}
