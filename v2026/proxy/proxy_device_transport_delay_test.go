package proxy

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// A hosted device's window transports run from the data center to the edge
// and never cross the user's link, so the device offers no delay header and
// reports no transport quality (connect/DELAY.md §10.4).
func TestProxyDevicesDoNotMeasureTransportDelay(t *testing.T) {
	settings := DefaultProxyDeviceSettings()
	settings.DisableWindowIdentityPersistence = true
	local := newProxyDeviceLocalSettings(context.Background(), &model.ProxyDeviceConfig{}, settings)
	if local.EnableTransportDelay {
		t.Fatal("a hosted device measures the transport delay of its data center transports")
	}
}
