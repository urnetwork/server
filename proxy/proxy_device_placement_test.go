package proxy

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// A placement that offers nothing; the tests compare its identity only.
type testProxyPlatformPlacement struct{}

func (self *testProxyPlatformPlacement) PlatformPlacementTargets(host string) []netip.Addr {
	return nil
}

func (self *testProxyPlatformPlacement) PlatformPlacementFailed(host string, targetIps []netip.Addr) {
}

// The manager's placement reaches every device it opens, and each device hands
// it to its DeviceLocal, whose windows dial the edge it chooses
// (connect/MOVE.md §13). Without one, the windows resolve the platform host.
func TestProxyDevicesCarryTheManagerPlacement(t *testing.T) {
	placement := &testProxyPlatformPlacement{}
	managerSettings := DefaultProxyDeviceManagerSettings()
	managerSettings.PlatformPlacement = placement
	manager := NewProxyDeviceManager(context.Background(), managerSettings)
	defer manager.cancel()

	deviceSettings := manager.proxyDeviceSettings()
	if deviceSettings.PlatformPlacement != connect.PlatformPlacement(placement) {
		t.Fatalf("device placement = %v, want the manager's", deviceSettings.PlatformPlacement)
	}
	deviceSettings.DisableWindowIdentityPersistence = true
	local := newProxyDeviceLocalSettings(context.Background(), &model.ProxyDeviceConfig{}, deviceSettings)
	if local.PlatformPlacement != connect.PlatformPlacement(placement) {
		t.Fatalf("DeviceLocal placement = %v, want the manager's", local.PlatformPlacement)
	}

	unplaced := DefaultProxyDeviceSettings()
	unplaced.DisableWindowIdentityPersistence = true
	if local := newProxyDeviceLocalSettings(context.Background(), &model.ProxyDeviceConfig{}, unplaced); local.PlatformPlacement != nil {
		t.Fatalf("a device without a placement places its windows with %v", local.PlatformPlacement)
	}
}
