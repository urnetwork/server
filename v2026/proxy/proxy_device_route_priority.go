// A device that cannot route a new flow yet registers its window clients ahead
// of a busy registration bound. Its first CONNECT waits in WaitForRoute until
// one window exit is added, and each window exit is a registration through the
// manager's bound (localclient.MintAdmission), which every device of the
// process shares. From 2026-11-14 the bound's two slots refused 30-60% of
// registrations under sweep load, and each refusal a new device met added
// seconds to its first CONNECT (P90SPEED.md §3.3). The device's authority now
// reads the device's route state, with WaitForRoute's own test, and while the
// device cannot route it may take the bound's reserved slots.
package proxy

import (
	"sync/atomic"
)

// Whether the device in `device` can route a new flow. A device still being
// built cannot.
func proxyDeviceRoutable(device *atomic.Pointer[ProxyDevice]) func() bool {
	return func() bool {
		proxyDevice := device.Load()
		if proxyDevice == nil {
			return false
		}
		deviceState := proxyDevice.proxyDeviceState()
		return deviceState != nil && windowRoutable(deviceState.GetWindowStatus())
	}
}
