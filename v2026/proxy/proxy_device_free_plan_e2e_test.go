package proxy

// NewProxyDevice reads a device's plan from its hosted credential, whose pro
// claim the mint takes from the network's balances: a network without Pro
// builds a free device, and a network with an in-window Pro balance a Pro
// one once its proxy's hosted session is minted again. Through the local test
// environment and the real constructor. Each test has its own environment,
// since a harness takes the environment's one proxy client address, and its
// setup builds the proxy's first device.

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Requires the device's plan, as its credential carries it, and its hold.
func requireProxyDevicePlan(t testing.TB, pd *ProxyDevice, pro bool) {
	t.Helper()
	if pd.localControl.Pro() != pro || pd.freePlan == pro {
		t.Fatalf("a device whose credential has pro %t has free plan %t, want pro %t", pd.localControl.Pro(), pd.freePlan, pro)
	}
	wantHold := pd.settings.ProxyDeviceIdleTimeout
	if !pro {
		wantHold = pd.settings.FreeProxyDeviceIdleTimeout
	}
	if got := pd.usedIdleTimeout(); got != wantHold {
		t.Fatalf("a device with pro %t holds %s, want %s", pro, got, wantHold)
	}
}

// Cancels the device and opens the proxy's next one.
func reopenProxyDevicePlanTest(t testing.TB, h *proxyTestHarness, pd *ProxyDevice) *ProxyDevice {
	t.Helper()
	pd.Cancel()
	next, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
	if err != nil {
		t.Fatal(err)
	}
	if next == pd {
		t.Fatal("the canceled device was opened again")
	}
	return next
}

func TestNewProxyDeviceOfANetworkWithoutProIsFree(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		opts := defaultProxyTestOptions()
		opts.controlPlaneOnly = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.close(t)
		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatal(err)
		}
		requireProxyDevicePlan(t, pd, false)
	})
}

// A network given Pro after its proxy's first device keeps that device's
// session, minted without Pro, for the proxy's next device (hosted session
// reuse); a device on a newly minted session is a Pro device.
func TestNewProxyDeviceOfANetworkWithProIsPro(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		opts := defaultProxyTestOptions()
		opts.controlPlaneOnly = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.close(t)
		first, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatal(err)
		}
		requireProxyDevicePlan(t, first, false)

		now := server.NowUtc()
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.Raise(model.AddProTransferBalanceInTx(tx, h.ctx, h.pdNetworkId, model.ByteCount(1024*1024*1024), now.Add(-time.Hour), now.Add(time.Hour)))
		})
		reused := reopenProxyDevicePlanTest(t, h, first)
		requireProxyDevicePlan(t, reused, false)

		func() {
			hostedSessions := h.proxyDeviceManager.hostedSessions
			hostedSessions.stateLock.Lock()
			defer hostedSessions.stateLock.Unlock()
			clear(hostedSessions.entries)
		}()
		minted := reopenProxyDevicePlanTest(t, h, reused)
		requireProxyDevicePlan(t, minted, true)
	})
}
