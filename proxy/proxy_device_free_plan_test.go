// A hosted device of a network on the free plan is cheaper than a Pro one
// (owner, 2026-11-24): it holds for ten minutes past its last activity, not
// 45, and its windows hold two quality exits and one speed exit, not five and
// one. Its plan is its hosted credential's pro claim, read once at
// construction (NewProxyDevice). Pro devices are unchanged.
package proxy

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The owner's values, in the settings every device is opened with.
func TestFreeProxyDeviceDefaults(t *testing.T) {
	settings := DefaultProxyDeviceSettings()
	if settings.FreeProxyDeviceIdleTimeout != 10*time.Minute || settings.ProxyDeviceIdleTimeout != 45*time.Minute {
		t.Errorf("holds: free %s and Pro %s, want 10m0s and 45m0s", settings.FreeProxyDeviceIdleTimeout, settings.ProxyDeviceIdleTimeout)
	}
	if settings.FreeQualityWindowSize != 2 || settings.FreeSpeedWindowSize != 1 {
		t.Errorf("free windows: %d quality and %d speed exits, want 2 and 1", settings.FreeQualityWindowSize, settings.FreeSpeedWindowSize)
	}
}

// A free device that carried traffic closes once it has been idle for the
// free hold, while a Pro device opened at the same time stays open until its
// own hold. The unused hold of either is unchanged.
func TestFreeProxyDeviceHasItsOwnHold(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		window := newLazyOpenTestWindow()
		window.addExit()
		devices := newLazyOpenTestDevices(t, manager, window)
		freeId, proId := server.NewId(), server.NewId()
		seedHttpConnectTestLock(manager, freeId, true)
		seedHttpConnectTestLock(manager, proId, true)
		// as NewProxyDevice reads each device's plan from its credential
		manager.proxyDeviceBuilder = func(proxyId server.Id) (*ProxyDevice, error) {
			pd, err := devices.build(proxyId)
			if err == nil {
				pd.freePlan = proxyId == freeId
			}
			return pd, err
		}
		core := newHttpConnectTestProxy(manager)
		for _, proxyId := range []server.Id{freeId, proId} {
			dial := lazyOpenTestHttpDial(manager.ctx, core, proxyId)
			if dial.err != nil {
				t.Fatal(dial.err)
			}
			devices.carry(t, dial.conn)
			_ = dial.conn.Close()
		}
		built := devices.built()
		if len(built) != 2 || !built[0].freePlan || built[1].freePlan {
			t.Fatalf("built %d devices, want the free one and then the Pro one", len(built))
		}

		settings := DefaultProxyDeviceSettings()
		checkTimeout := manager.settings.CheckProxyDeviceIdleTimeout
		time.Sleep(settings.FreeProxyDeviceIdleTimeout - checkTimeout)
		synctest.Wait()
		if closed := devices.closed(); len(closed) != 0 {
			t.Fatalf("%d devices closed before the free hold", len(closed))
		}
		time.Sleep(2 * checkTimeout)
		synctest.Wait()
		if closed := devices.closed(); len(closed) != 1 || closed[0] != built[0] || manager.DeviceCount() != 1 {
			t.Fatalf("after the free hold: %d closed, %d installed; want the free device closed and the Pro one open", len(closed), manager.DeviceCount())
		}

		time.Sleep(settings.ProxyDeviceIdleTimeout - settings.FreeProxyDeviceIdleTimeout - 2*checkTimeout)
		synctest.Wait()
		if closed := devices.closed(); len(closed) != 1 {
			t.Fatal("the Pro device closed before its hold")
		}
		time.Sleep(2 * checkTimeout)
		synctest.Wait()
		if closed := devices.closed(); len(closed) != 2 || closed[1] != built[1] || manager.DeviceCount() != 0 {
			t.Fatalf("after the Pro hold: %d closed, %d installed; want both closed", len(closed), manager.DeviceCount())
		}
	})
}

// The hold of a device by its plan, past the unused hold.
func TestProxyDeviceHoldsByPlan(t *testing.T) {
	settings := DefaultProxyDeviceSettings()
	for _, c := range []struct {
		freePlan bool
		want     time.Duration
	}{
		{freePlan: false, want: settings.ProxyDeviceIdleTimeout},
		{freePlan: true, want: settings.FreeProxyDeviceIdleTimeout},
	} {
		pd := &ProxyDevice{settings: settings, freePlan: c.freePlan}
		if got := pd.usedIdleTimeout(); got != c.want {
			t.Errorf("free=%t holds %s, want %s", c.freePlan, got, c.want)
		}
	}
	// a nonpositive free hold holds a free device as long as a Pro one
	settings.FreeProxyDeviceIdleTimeout = 0
	if got := (&ProxyDevice{settings: settings, freePlan: true}).usedIdleTimeout(); got != settings.ProxyDeviceIdleTimeout {
		t.Errorf("with no free hold a free device holds %s, want %s", got, settings.ProxyDeviceIdleTimeout)
	}
}

// A free device's windows hold two quality exits and one speed exit, in the
// shapes its memory profile gives them, and it builds no limited window, as a
// Pro device does not; a Pro device keeps its profile's sizes.
func TestFreeProxyDeviceWindowsHoldTwoQualityAndOneSpeedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultProxyDeviceSettings()
	settings.DisableWindowIdentityPersistence = true

	free := newProxyDeviceLocalSettings(ctx, &model.ProxyDeviceConfig{}, settings, true)
	if free.QualityWindowSize == nil || free.SpeedWindowSize == nil {
		t.Fatal("a free device keeps its profile's window sizes")
	}
	connect.AssertEqual(t, *free.QualityWindowSize, connect.WindowSizeSettings{
		WindowSizeMin:            2,
		WindowSizeMax:            2,
		WindowSizeHardMax:        2,
		WindowSizeReconnectScale: 1,
	})
	connect.AssertEqual(t, *free.SpeedWindowSize, connect.WindowSizeSettings{
		WindowSizeMin:            1,
		WindowSizeMax:            1,
		WindowSizeHardMax:        1,
		FixedWindowSize:          1,
		WindowSizeReconnectScale: 1,
	})

	pro := newProxyDeviceLocalSettings(ctx, &model.ProxyDeviceConfig{}, settings, false)
	if pro.QualityWindowSize != nil || pro.SpeedWindowSize != nil {
		t.Fatalf("a Pro device sizes its windows: quality %+v, speed %+v", pro.QualityWindowSize, pro.SpeedWindowSize)
	}
	if !free.DisableLimitedWindow || !pro.DisableLimitedWindow {
		t.Errorf("limited windows disabled: free %t, Pro %t; want neither built", free.DisableLimitedWindow, pro.DisableLimitedWindow)
	}
}
