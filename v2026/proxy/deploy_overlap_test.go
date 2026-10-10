package proxy

// The two-container deploy overlap (REVIEW2-UPDATE1 §4.4, FIXPLAN1 decision
// 3). During a deploy, instance A (old) keeps serving through its drain
// grace with a live device whose window runs a persisted identity. Instance
// B (new) becomes ready and — before the beacon gate — both notification
// warmup and customer traffic can open a DeviceLocal. Restoring A's persisted
// identities from either path runs one logical Connect client in two processes:
// A's live-ctx window eviction can then api-remove the identity B is using.
// The gated sequence (cli/proxy/main.go) lets those early opens form fresh
// windows, buffers their snapshots, and releases restoration only after A's
// drain-end export appears (an empty peer set is the completion marker — the
// drain-complete beacon). Post-drain prewarm can safely restore any unopened
// device; poll-budget expiry is the old-instance-crashed fallback.
//
// This test runs both instances for real over the full harness (not
// instrumented fakes): A = the harness device manager + wg server (device
// live, window identity persisted, activity recorded), B = a fresh device
// manager running main's exact sequence (ApplyWgHandoff, release identity
// restoration, then Prewarm). B's
// wgServer never binds the wg port (A still holds it — the point is A stays
// live); with an empty export the apply never touches the wg proxy, so a
// bare wgServer models B's half faithfully. Asserts:
//
//  1. while A drains, B can lazily open a ready device, but does not restore
//     or publish A's identities; generation request published
//  2. A's real drain-end export (ExportWgHandoff) opens the gate; B publishes
//     its fresh identity snapshot and keeps serving with that device
//  3. fallback: with no export ever arriving, B still pre-warms at
//     poll-budget expiry, and the expiry is a clean (non-raising) return

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestProxyDeployOverlapPrewarmGate(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyDeployOverlapPrewarmGate\n")
		h := setupProxyTest(t)
		defer h.close(t)

		// the handoff and activity stores are keyed by (host, block)
		proxyHost := fmt.Sprintf("overlap%d", time.Now().UnixNano()%1000000)
		block := "g1"
		os.Setenv("WARP_HOST", proxyHost)
		os.Setenv("WARP_BLOCK", block)
		defer os.Unsetenv("WARP_HOST")
		defer os.Setenv("WARP_BLOCK", "test")

		// ---- instance A (old): live device with a persisted window identity,
		// its client recorded in the activity set (what its flusher does) ----
		var identities []*model.ProxyWindowClientIdentity
		waitFor(t, 30*time.Second, "window identity snapshot persisted", func() bool {
			identities = model.GetProxyWindowIdentities(h.ctx, h.proxyId)
			return 0 < len(identities)
		})
		windowClientIds := map[server.Id]bool{}
		for _, identity := range identities {
			windowClientIds[identity.ClientId] = true
		}
		model.TouchProxyClientActivity(h.ctx, proxyHost, block, server.NowUtc(), h.proxyId)
		pdA, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("open old-instance device: %v", err)
		}

		// ---- instance B (new): a fresh manager running main's sequence ----
		pdmSettings := DefaultProxyDeviceManagerSettings()
		pdmSettings.NetworkSpace = h.networkSpace
		pdmSettings.HoldWindowIdentityRestore = true
		freshManager := NewProxyDeviceManager(h.ctx, pdmSettings)
		defer closeProxyDeviceManager(t, freshManager)

		bSettings := *DefaultProxySettings()
		bSettings.WgHandoffPollInterval = 20 * time.Millisecond
		// generous: the gate must open on the export, never on the budget
		bSettings.WgHandoffPollTimeout = 5 * time.Minute
		bSettings.PrewarmReadyTimeout = 60 * time.Second
		bSettings.PrewarmTimeout = 120 * time.Second

		bCtx, bCancel := context.WithCancel(h.ctx)
		defer bCancel()
		wgB := &wgServer{
			ctx:                bCtx,
			cancel:             bCancel,
			proxyDeviceManager: freshManager,
			settings:           &bSettings,
		}

		// main's exact post-readiness sequence: the handoff outcome first (in
		// its own error scope), then identity release and pre-warm
		prewarmed := make(chan int, 1)
		go func() {
			prewarmed <- wgB.CompleteDeploymentHandoff(bCtx)
		}()

		// B publishes its generation request at the gate (the real Prepare
		// path, which A's drain-end export will read)
		waitFor(t, 15*time.Second, "replacement generation published", func() bool {
			return model.CurrentProxyWgHandoffGeneration(h.ctx, proxyHost, block) != ""
		})

		// ---- 1. while A drains, B may accept a lazy open, but it must mint
		// fresh identities and leave A's durable snapshot untouched. This is
		// the path notification warmup and a first post-flip request both use. ----
		pdB, err := freshManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("open replacement device before drain completion: %v", err)
		}
		if !pdB.WaitForReady(h.ctx, 60*time.Second) {
			t.Fatal("replacement lazy-open device did not become ready")
		}
		var bufferedIdentities []*connect.WindowClientIdentity
		waitFor(t, 30*time.Second, "replacement fresh identity buffered", func() bool {
			freshManager.windowIdentityGate.stateLock.Lock()
			defer freshManager.windowIdentityGate.stateLock.Unlock()
			for _, pendingSnapshot := range freshManager.windowIdentityGate.pending {
				if 0 < len(pendingSnapshot.identities) {
					bufferedIdentities = cloneWindowClientIdentities(pendingSnapshot.identities)
					return true
				}
			}
			return false
		})
		for _, identity := range bufferedIdentities {
			if identity != nil && windowClientIds[server.Id(identity.ClientId)] {
				t.Fatal("replacement lazy open restored an identity still live in the draining instance")
			}
		}
		persistedDuringDrain := model.GetProxyWindowIdentities(h.ctx, h.proxyId)
		if len(persistedDuringDrain) != len(identities) {
			t.Fatalf("persisted identity count during drain = %d, want old snapshot count %d", len(persistedDuringDrain), len(identities))
		}
		for _, identity := range persistedDuringDrain {
			if !windowClientIds[identity.ClientId] {
				t.Fatal("replacement published a fresh identity before drain completion")
			}
		}
		select {
		case count := <-prewarmed:
			t.Fatalf("pre-warm ran before the old instance's drain-end export (%d ready)", count)
		default:
		}
		if deviceCount := freshManager.DeviceCount(); deviceCount != 1 {
			t.Fatalf("replacement device count during drain = %d, want the one lazy-open device", deviceCount)
		}
		if !pdA.Active() {
			t.Fatalf("old instance device is not live during its drain grace")
		}
		if identities = model.GetProxyWindowIdentities(h.ctx, h.proxyId); len(identities) == 0 {
			t.Fatalf("persisted window identity disappeared while the new instance was gated")
		}
		fmt.Printf("[progress]replacement lazy open used fresh identities while the old instance drains\n")

		// ---- 2. A reaches drain end: the real before-exit export (no peer
		// handshook in this test, so the export is the empty completion
		// marker), then A exits (ctx-done teardown keeps the snapshot) ----
		h.wg.ExportWgHandoff(h.ctx)
		pdA.Cancel()

		var readyCount int
		select {
		case readyCount = <-prewarmed:
		case <-time.After(150 * time.Second):
			t.Fatal("pre-warm did not run after the drain-end export")
		}
		if readyCount != 1 {
			t.Fatalf("pre-warm ready count = %d, want 1", readyCount)
		}
		pdBAfterRelease, err := freshManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("open pre-warmed device: %v", err)
		}
		if pdBAfterRelease != pdB {
			t.Fatal("identity release recreated the live replacement device")
		}
		if !pdB.Active() {
			t.Fatalf("pre-warmed device is not active")
		}
		if !pdB.WaitForReady(h.ctx, 1*time.Second) {
			t.Fatalf("pre-warmed device is not ready")
		}
		// The gate publishes B's buffered fresh snapshot only after A's drain
		// completed. No identity may overlap with A's old live set.
		waitFor(t, 30*time.Second, "replacement fresh identities published", func() bool {
			persisted := model.GetProxyWindowIdentities(h.ctx, h.proxyId)
			if len(persisted) == 0 {
				return false
			}
			for _, identity := range persisted {
				if windowClientIds[identity.ClientId] {
					return false
				}
			}
			return true
		})
		fmt.Printf("[progress]gate opened on the drain-end export; fresh identities published\n")

		// ---- 3. crashed-old fallback: no export ever arrives; the sequence
		// still pre-warms once the poll budget expires, via a clean return ----
		pdB.Cancel()
		waitFor(t, 15*time.Second, "pre-warmed device removed", func() bool {
			freshManager.stateLock.Lock()
			defer freshManager.stateLock.Unlock()
			_, ok := freshManager.proxyDevices[h.proxyId]
			return !ok
		})

		crashHost := fmt.Sprintf("overlapcrash%d", time.Now().UnixNano()%1000000)
		os.Setenv("WARP_HOST", crashHost)
		model.TouchProxyClientActivity(h.ctx, crashHost, block, server.NowUtc(), h.proxyId)
		fallbackManager := NewProxyDeviceManager(h.ctx, pdmSettings)
		defer closeProxyDeviceManager(t, fallbackManager)

		cSettings := bSettings
		cSettings.WgHandoffPollTimeout = 1 * time.Second
		cCtx, cCancel := context.WithCancel(h.ctx)
		defer cCancel()
		wgC := &wgServer{
			ctx:                cCtx,
			cancel:             cCancel,
			proxyDeviceManager: fallbackManager,
			settings:           &cSettings,
		}

		expiryStart := time.Now()
		var expiryCount int
		if r := server.HandleError(func() {
			expiryCount = <-wgC.ApplyWgHandoff(cCtx)
		}); r != nil {
			t.Fatalf("apply at poll-budget expiry raised: %v", r)
		}
		if expiryCount != 0 {
			t.Fatalf("apply at poll-budget expiry re-established %d, want 0", expiryCount)
		}
		if elapsed := time.Since(expiryStart); elapsed < 900*time.Millisecond {
			t.Fatalf("apply returned before the poll budget: %s", elapsed)
		}
		fallbackManager.ReleaseWindowIdentityRestore()
		readyCount = Prewarm(cCtx, fallbackManager, &cSettings)
		if readyCount != 1 {
			t.Fatalf("fallback pre-warm ready count = %d, want 1", readyCount)
		}
		fmt.Printf("[progress]crashed-old fallback pre-warmed at poll-budget expiry\n")
	})
}
