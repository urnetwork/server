package proxy

// Window identity reuse e2e (PROXYDRAIN1.md §3.5), over the full harness: a
// hosted device forms its window (persisting the window client identities),
// the device is torn down the way a shutdown does it, and the RECREATED
// device must reuse the SAME window client id against the SAME provider
// destination — the invariant that keeps provider-side NAT flows resumable —
// and serve traffic again.

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestProxyWindowIdentityReuse(t *testing.T) {
	if testing.Short() {
		return
	}
	// see TestProxy: fixed ports cannot be rebound on a rerun in the same process
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyWindowIdentityReuse\n")
		h := setupProxyTest(t)
		defer h.cancel()

		// the device is ready (setup warmed it), so its window has formed and
		// the identity snapshot is persisted. The window may run more than
		// one client toward the pinned provider, so capture the whole set.
		var identities []*model.ProxyWindowClientIdentity
		waitFor(t, 30*time.Second, "window identity snapshot persisted", func() bool {
			identities = model.GetProxyWindowIdentities(h.ctx, h.proxyId)
			return 0 < len(identities)
		})
		windowClientIds := map[server.Id]bool{}
		for _, identity := range identities {
			// every destination is the pinned provider
			finalDestinationId := identity.DestinationIds[len(identity.DestinationIds)-1]
			if finalDestinationId != h.providerClientId {
				t.Fatalf("persisted destination = %s, want the pinned provider %s", finalDestinationId, h.providerClientId)
			}
			windowClientIds[identity.ClientId] = true
		}
		fmt.Printf("[progress]window identities persisted: %d client(s) -> provider %s\n", len(windowClientIds), h.providerClientId)

		// confirm traffic before the restart
		testProxyHttps(t, h)

		// ---- tear down the device the way a shutdown does ----
		pd1, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("open proxy device: %v", err)
		}
		pd1.Cancel()
		waitFor(t, 15*time.Second, "device removed from manager", func() bool {
			h.proxyDeviceManager.stateLock.Lock()
			defer h.proxyDeviceManager.stateLock.Unlock()
			_, ok := h.proxyDeviceManager.proxyDevices[h.proxyId]
			return !ok
		})

		// the shutdown teardown must have KEPT the persisted snapshot (the
		// ctx-done guard in the generator): this is what a real deploy
		// restart restores from
		identities = model.GetProxyWindowIdentities(h.ctx, h.proxyId)
		if len(identities) == 0 {
			t.Fatalf("window identity snapshot was wiped by the shutdown teardown")
		}

		// ---- the recreated device must REUSE the identity ----
		pd2, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("re-open proxy device: %v", err)
		}
		if pd2 == pd1 {
			t.Fatalf("expected a new proxy device")
		}
		if ready := pd2.WaitForReady(h.ctx, 60*time.Second); !ready {
			t.Fatalf("recreated proxy device did not become ready")
		}

		// the recreated window re-recorded its live snapshot reusing (at
		// least one of) the persisted identities against the same provider —
		// NOT minting an entirely fresh set. This is the invariant that
		// keeps provider-side NAT flows resumable: flows keyed by a reused
		// client id resume.
		reused := false
		waitFor(t, 30*time.Second, "recreated window reuses a persisted identity", func() bool {
			identities = model.GetProxyWindowIdentities(h.ctx, h.proxyId)
			for _, identity := range identities {
				if windowClientIds[identity.ClientId] {
					finalDestinationId := identity.DestinationIds[len(identity.DestinationIds)-1]
					if finalDestinationId != h.providerClientId {
						t.Fatalf("reused identity destination = %s, want %s", finalDestinationId, h.providerClientId)
					}
					reused = true
				}
			}
			return reused
		})
		fmt.Printf("[progress]window identity reused across the device recreate\n")

		// and the recreated device serves traffic with the reused identity
		testProxyHttps(t, h)
		fmt.Printf("[progress]window identity reuse e2e complete\n")
	})
}
