package proxy

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestDefaultProxySettingsIngressPorts(t *testing.T) {
	settings := DefaultProxySettings()

	connect.AssertEqual(t, settings.SocksPort, InternalSocksPort)
	connect.AssertEqual(t, settings.HttpPort, InternalHttpPort)
	connect.AssertEqual(t, settings.HttpsPort, InternalHttpsPort)
	connect.AssertEqual(t, settings.WgPort, InternalWgPort)
}
