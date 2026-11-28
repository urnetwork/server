package proxy

import (
	"sync/atomic"
	"testing"
)

// The authority reads the device's route with WaitForRoute's own test: a
// device still being built, or whose window has no provider, cannot route;
// one provider below the minimum, or a ready window, can.
func TestProxyDeviceRoutableReadsTheDeviceWindow(t *testing.T) {
	var device atomic.Pointer[ProxyDevice]
	routable := proxyDeviceRoutable(&device)
	if routable() {
		t.Fatal("a device still being built can route")
	}
	forming := newProxyDeviceReadinessTestSource(false)
	device.Store(&ProxyDevice{ctx: t.Context(), deviceState: forming})
	if routable() {
		t.Fatal("a window with no provider can route")
	}
	publishRouteTestProviders(forming, 1)
	if !routable() {
		t.Fatal("a window with one provider cannot route")
	}
	device.Store(&ProxyDevice{ctx: t.Context(), deviceState: newProxyDeviceReadinessTestSource(true)})
	if !routable() {
		t.Fatal("a ready window cannot route")
	}
}
