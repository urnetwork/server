package proxy

import (
	"testing"

	"github.com/urnetwork/connect"
	proxylib "github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestDefaultProxySettingsIngressPorts(t *testing.T) {
	settings := DefaultProxySettings()

	connect.AssertEqual(t, settings.SocksPort, InternalSocksPort)
	connect.AssertEqual(t, settings.HttpPort, InternalHttpPort)
	connect.AssertEqual(t, settings.HttpsPort, InternalHttpsPort)
	connect.AssertEqual(t, settings.WgPort, InternalWgPort)
}

func TestDefaultProxySettingsUsesHttpDialRetryPacing(t *testing.T) {
	settings := DefaultProxySettings()

	connect.AssertEqual(
		t,
		settings.ProxyConnectTimeout,
		proxylib.DefaultHttpProxySettings().ProxyConnectTimeout,
	)
}

// TestDefaultProxyDeviceManagerSettingsLoadsDeviceMemoryBudget proves the main
// proxy config controls the single DeviceLocal target rather than a hardcoded
// carrier-only value.
func TestDefaultProxyDeviceManagerSettingsLoadsDeviceMemoryBudget(t *testing.T) {
	popConfig := server.Config.PushSimpleResource(
		"proxy.yml",
		[]byte("device_memory_budget: 24MiB\n"),
	)
	defer popConfig()
	settings := DefaultProxyDeviceManagerSettings()
	connect.AssertEqual(
		t,
		settings.DeviceMemoryTargetByteCount,
		model.ByteCount(24*model.Mib),
	)
}

// TestDefaultProxyDeviceManagerSettingsRejectsInvalidDeviceMemoryBudget pins
// fail-fast startup for a malformed target; silently using the global carrier
// budget recreates the production starvation failure.
func TestDefaultProxyDeviceManagerSettingsRejectsInvalidDeviceMemoryBudget(t *testing.T) {
	popConfig := server.Config.PushSimpleResource(
		"proxy.yml",
		[]byte("device_memory_budget: definitely-not-bytes\n"),
	)
	defer popConfig()
	defer func() {
		if recover() == nil {
			t.Fatal("invalid device_memory_budget did not fail startup")
		}
	}()
	_ = DefaultProxyDeviceManagerSettings()
}

// TestDefaultProxyDeviceManagerSettingsRejectsZeroDeviceMemoryBudget prevents
// an explicit zero from opting hosted devices back into process-global
// admission state.
func TestDefaultProxyDeviceManagerSettingsRejectsZeroDeviceMemoryBudget(t *testing.T) {
	popConfig := server.Config.PushSimpleResource(
		"proxy.yml",
		[]byte("device_memory_budget: 0B\n"),
	)
	defer popConfig()
	defer func() {
		if recover() == nil {
			t.Fatal("zero device_memory_budget did not fail startup")
		}
	}()
	_ = DefaultProxyDeviceManagerSettings()
}
