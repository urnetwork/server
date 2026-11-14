// A hosted device that has carried no user traffic since it opened is closed
// ten minutes after its last activity; one that has carried traffic keeps the
// 45-minute hold. These tests drive the production wiring (the HTTP dial
// callback and its relay connection, the WireGuard packet entry and the
// manager's idle close) on the synctest clock, with the lazy-open test devices
// (proxy_device_lazy_open_test.go). They read only what the manager shows,
// so they also run against the code from before the shorter hold.
package proxy

import (
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// A device whose connection carried nothing is closed and forgotten ten
// minutes after it opened, not 45. On 2026-11-14 a script walked about 57k
// bot-made hosted proxies once an hour; since devices open on first use, each
// walk opened 17-20k devices that relayed nothing, and each held its windows'
// consumer connections for the whole 45 minutes.
func TestProxyDeviceThatCarriedNoTrafficClosesAfterTenMinutes(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		window := newLazyOpenTestWindow()
		window.addExit()
		devices := newLazyOpenTestDevices(t, manager, window)
		core := newHttpConnectTestProxy(manager)

		// the walk's touch: admitted and dialed, then closed with no byte
		// either way
		touch := lazyOpenTestHttpDial(manager.ctx, core, proxyId)
		if touch.err != nil {
			t.Fatalf("the CONNECT failed: %v", touch.err)
		}
		_ = touch.conn.Close()
		if devices.starts() != 1 || manager.DeviceCount() != 1 {
			t.Fatalf("the CONNECT started %d constructions, want 1", devices.starts())
		}

		checkTimeout := manager.settings.CheckProxyDeviceIdleTimeout
		time.Sleep(10*time.Minute - checkTimeout)
		synctest.Wait()
		if manager.DeviceCount() != 1 || len(devices.closed()) != 0 {
			t.Fatal("the device closed before ten minutes without activity")
		}
		time.Sleep(2 * checkTimeout)
		synctest.Wait()
		built := devices.built()
		if closed := devices.closed(); manager.DeviceCount() != 0 || len(closed) != 1 || closed[0] != built[0] {
			t.Fatalf("%s after an open that carried nothing: %d devices installed, %d closed; want the device closed and forgotten", 10*time.Minute+checkTimeout, manager.DeviceCount(), len(closed))
		}
	})
}

// A device that carried traffic keeps the 45-minute hold: it outlives the
// unused hold several times over and closes only after 45 minutes without
// activity.
func TestProxyDeviceThatCarriedTrafficIsHeld45Minutes(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		window := newLazyOpenTestWindow()
		window.addExit()
		devices := newLazyOpenTestDevices(t, manager, window)
		core := newHttpConnectTestProxy(manager)

		used := lazyOpenTestHttpDial(manager.ctx, core, proxyId)
		if used.err != nil {
			t.Fatalf("the CONNECT failed: %v", used.err)
		}
		devices.carry(t, used.conn)
		_ = used.conn.Close()

		checkTimeout := manager.settings.CheckProxyDeviceIdleTimeout
		time.Sleep(10*time.Minute + checkTimeout)
		synctest.Wait()
		if manager.DeviceCount() != 1 || len(devices.closed()) != 0 {
			t.Fatal("a device that carried traffic closed at the unused hold")
		}
		time.Sleep(45*time.Minute - 10*time.Minute - 2*checkTimeout)
		synctest.Wait()
		if manager.DeviceCount() != 1 || len(devices.closed()) != 0 {
			t.Fatal("a device that carried traffic closed before 45 minutes without activity")
		}
		time.Sleep(2 * checkTimeout)
		synctest.Wait()
		if manager.DeviceCount() != 0 || len(devices.closed()) != 1 {
			t.Fatalf("after %s without activity: %d devices installed, %d closed; want the used device closed", 45*time.Minute+checkTimeout, manager.DeviceCount(), len(devices.closed()))
		}
	})
}

// A device in use is never closed early: a relay connection that moves a byte
// every 30 minutes, three times the unused hold apart, keeps its first device
// for four hours.
func TestProxyDeviceInUseIsNeverClosed(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		window := newLazyOpenTestWindow()
		window.addExit()
		devices := newLazyOpenTestDevices(t, manager, window)
		core := newHttpConnectTestProxy(manager)

		relay := lazyOpenTestHttpDial(manager.ctx, core, proxyId)
		if relay.err != nil {
			t.Fatalf("the CONNECT failed: %v", relay.err)
		}
		defer relay.conn.Close()
		for i := range 8 {
			devices.carry(t, relay.conn)
			time.Sleep(30 * time.Minute)
			synctest.Wait()
			if manager.DeviceCount() != 1 || len(devices.closed()) != 0 || devices.starts() != 1 {
				t.Fatalf("after %s of traffic every 30 minutes: %d devices, %d closed, %d constructions; want the first device held", time.Duration(i+1)*30*time.Minute, manager.DeviceCount(), len(devices.closed()), devices.starts())
			}
		}
	})
}

// A walk that touches a proxy without traffic gets one device for each pass:
// the touches of a pass hold it, so it is never closed and made again between
// them, and it closes ten minutes after the pass's last touch. The walk here
// passes once an hour, as the 2026-11-14 one did, and touches the proxy three
// times eight minutes apart in each pass.
func TestProxyDeviceWalkWithoutTrafficOpensOneDevicePerPass(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		window := newLazyOpenTestWindow()
		window.addExit()
		devices := newLazyOpenTestDevices(t, manager, window)
		core := newHttpConnectTestProxy(manager)
		checkTimeout := manager.settings.CheckProxyDeviceIdleTimeout

		passStart := time.Now()
		for pass := range 3 {
			// the seeded ip lock lasts an hour; a pass reads it from no database
			seedHttpConnectTestLock(manager, proxyId, true)
			for touch := range 3 {
				if 0 < touch {
					time.Sleep(8 * time.Minute)
				}
				dial := lazyOpenTestHttpDial(manager.ctx, core, proxyId)
				if dial.err != nil {
					t.Fatalf("pass %d touch %d failed: %v", pass, touch, dial.err)
				}
				_ = dial.conn.Close()
				synctest.Wait()
				if manager.DeviceCount() != 1 || devices.starts() != pass+1 || len(devices.closed()) != pass {
					t.Fatalf("pass %d touch %d: %d devices, %d constructions, %d closed; want the pass's one device held through its touches", pass, touch, manager.DeviceCount(), devices.starts(), len(devices.closed()))
				}
			}
			time.Sleep(10*time.Minute - checkTimeout)
			synctest.Wait()
			if manager.DeviceCount() != 1 || len(devices.closed()) != pass {
				t.Fatalf("pass %d: the device closed before ten minutes after the last touch", pass)
			}
			time.Sleep(2 * checkTimeout)
			synctest.Wait()
			if manager.DeviceCount() != 0 || len(devices.closed()) != pass+1 {
				t.Fatalf("pass %d: %d devices, %d closed %s after the last touch; want the pass's device closed", pass, manager.DeviceCount(), len(devices.closed()), 10*time.Minute+checkTimeout)
			}
			passStart = passStart.Add(time.Hour)
			time.Sleep(time.Until(passStart))
		}
		if devices.starts() != 3 {
			t.Fatalf("three passes made %d devices, want one each", devices.starts())
		}
	})
}

// The next connection after the unused close opens a new device and is carried
// through it, and that device, once it carries traffic, keeps the 45-minute
// hold. The closed device is never handed out again.
func TestProxyDeviceReopensAfterTheUnusedClose(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := server.NewId()
		seedHttpConnectTestLock(manager, proxyId, true)
		window := newLazyOpenTestWindow()
		window.addExit()
		devices := newLazyOpenTestDevices(t, manager, window)
		core := newHttpConnectTestProxy(manager)
		checkTimeout := manager.settings.CheckProxyDeviceIdleTimeout

		touch := lazyOpenTestHttpDial(manager.ctx, core, proxyId)
		if touch.err != nil {
			t.Fatalf("the first CONNECT failed: %v", touch.err)
		}
		_ = touch.conn.Close()
		time.Sleep(10*time.Minute + checkTimeout)
		synctest.Wait()
		if manager.DeviceCount() != 0 || len(devices.closed()) != 1 {
			t.Fatalf("%d devices, %d closed; want the unused device closed", manager.DeviceCount(), len(devices.closed()))
		}

		next := lazyOpenTestHttpDial(manager.ctx, core, proxyId)
		if next.err != nil {
			t.Fatalf("the CONNECT after the unused close failed: %v", next.err)
		}
		devices.carry(t, next.conn)
		_ = next.conn.Close()
		built, dials := devices.built(), devices.dials()
		if devices.starts() != 2 || len(built) != 2 || built[1] == built[0] || len(dials) != 2 || dials[1] != built[1] {
			t.Fatalf("%d constructions; the CONNECT after the close dialed through %p, want a new device", devices.starts(), dials[len(dials)-1])
		}

		time.Sleep(45*time.Minute - checkTimeout)
		synctest.Wait()
		if manager.DeviceCount() != 1 || len(devices.closed()) != 1 {
			t.Fatal("the reopened device, which carried traffic, closed before 45 minutes")
		}
		time.Sleep(2 * checkTimeout)
		synctest.Wait()
		if closed := devices.closed(); manager.DeviceCount() != 0 || len(closed) != 2 || closed[1] != built[1] {
			t.Fatal("the reopened device was not closed after 45 minutes without activity")
		}
	})
}

// A WireGuard session's packets are user traffic: the device its first packet
// opened keeps the 45-minute hold, and the session's next packet after the
// idle close opens a new device.
func TestWgSessionDeviceIsHeld45Minutes(t *testing.T) {
	configureLazyOpenTest(t)
	wg := newLazyOpenTestWgProxy(t)
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		devices := newLazyOpenTestDevices(t, manager, newLazyOpenTestWindow())
		changes := &lazyOpenTestProxyClientChanges{}
		clientAddr := netip.MustParseAddr("10.40.0.4")
		changes.add(newTestProxyClient(t, clientAddr))
		watchLazyOpenTestProxyClients(t, manager, wg, changes)
		synctest.Wait()

		packet := buildWgTestPacket(t, clientAddr.String(), "198.51.100.22", "session packet")
		if count, err := wg.Write([][]byte{packet}, 0); count != 1 || err != nil {
			t.Fatalf("the first session packet: count=%d err=%v, want it carried", count, err)
		}
		if devices.starts() != 1 || manager.DeviceCount() != 1 {
			t.Fatalf("the first session packet started %d constructions, want 1", devices.starts())
		}

		checkTimeout := manager.settings.CheckProxyDeviceIdleTimeout
		time.Sleep(45*time.Minute - checkTimeout)
		synctest.Wait()
		if manager.DeviceCount() != 1 || len(devices.closed()) != 0 {
			t.Fatal("the device of a WireGuard session closed before 45 minutes without activity")
		}
		time.Sleep(2 * checkTimeout)
		synctest.Wait()
		if manager.DeviceCount() != 0 || len(devices.closed()) != 1 {
			t.Fatal("the device of a WireGuard session was not closed after 45 minutes without activity")
		}

		if count, err := wg.Write([][]byte{packet}, 0); count != 1 || err != nil {
			t.Fatalf("the session packet after the idle close: count=%d err=%v, want it carried", count, err)
		}
		built := devices.built()
		if devices.starts() != 2 || len(built) != 2 || built[1] == built[0] || manager.DeviceCount() != 1 {
			t.Fatalf("the packet after the idle close started %d constructions in all, want a second device", devices.starts())
		}
	})
}
