package proxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Whether a manager built under the current proxy.yml opens hosted devices
// with the handover off.
func windowStrictHandoverTestDeviceLocalDisables(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx)
	defer manager.CloseAndWait(context.Background())
	settings := manager.proxyDeviceSettings()
	settings.DisableWindowIdentityPersistence = true
	deviceLocalSettings := newProxyDeviceLocalSettings(ctx, &model.ProxyDeviceConfig{}, settings)
	if settings.DisableStrictWindowHandover == deviceLocalSettings.StrictWindowHandover {
		t.Fatal("the hosted DeviceLocal does not follow the device's handover switch")
	}
	return !deviceLocalSettings.StrictWindowHandover
}

func TestWindowStrictHandoverIsTheDefault(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("{}\n"))
	defer popConfig()
	if !proxyWindowStrictHandoverFromConfig() {
		t.Fatal("the strict window handover is off without configuration")
	}
	if windowStrictHandoverTestDeviceLocalDisables(t) {
		t.Fatal("a hosted device turned the handover off without configuration")
	}
}

func TestWindowStrictHandoverCanBeTurnedOff(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("window_strict_handover: false\n"))
	defer popConfig()
	if proxyWindowStrictHandoverFromConfig() {
		t.Fatal("window_strict_handover: false left the handover on")
	}
	if !DefaultProxyDeviceManagerSettings().DisableStrictWindowHandover {
		t.Fatal("the default manager settings ignore window_strict_handover: false")
	}
	if !windowStrictHandoverTestDeviceLocalDisables(t) {
		t.Fatal("a hosted device kept the handover on under window_strict_handover: false")
	}
}

func TestWindowStrictHandoverIgnoresAnInvalidValue(t *testing.T) {
	for _, value := range []string{"sometimes", "0", "1", "null", "[]", "offf", `"disabled"`} {
		popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("window_strict_handover: "+value+"\n"))
		enabled := proxyWindowStrictHandoverFromConfig()
		popConfig()
		if !enabled {
			t.Errorf("window_strict_handover: %s turned the handover off", value)
		}
	}
}

// A proxy.yml that does not parse, or cannot be read, keeps the default.
func TestWindowStrictHandoverSurvivesUnreadableConfig(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("window_strict_handover: [false\n"))
	enabled := proxyWindowStrictHandoverFromConfig()
	popConfig()
	if !enabled {
		t.Error("an unparsable proxy.yml turned the handover off")
	}

	home := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", home)
	// a directory where the file belongs is unavailable, not absent
	if err := os.Mkdir(filepath.Join(home, "proxy.yml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !proxyWindowStrictHandoverFromConfig() {
		t.Error("an unreadable proxy.yml turned the handover off")
	}
}

// An operator who writes the kill switch as a YAML 1.1 word gets what the word
// says: the parser reads on, off, yes and no as strings.
func TestWindowStrictHandoverReadsTheYamlBooleanWords(t *testing.T) {
	for value, want := range map[string]bool{
		"off":     false,
		"no":      false,
		"OFF":     false,
		"No":      false,
		`"false"`: false,
		"false":   false,
		"on":      true,
		"yes":     true,
		`"true"`:  true,
		"true":    true,
	} {
		popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("window_strict_handover: "+value+"\n"))
		enabled := proxyWindowStrictHandoverFromConfig()
		popConfig()
		if enabled != want {
			t.Errorf("window_strict_handover: %s read as %t, want %t", value, enabled, want)
		}
	}
}

// Every hosted device holds its windows' refill while its payer is out of
// balance; apps leave connect's default off.
func TestHostedDevicesHoldTheirRefillWhileOutOfBalance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx)
	defer manager.CloseAndWait(context.Background())
	settings := manager.proxyDeviceSettings()
	settings.DisableWindowIdentityPersistence = true
	deviceLocalSettings := newProxyDeviceLocalSettings(ctx, &model.ProxyDeviceConfig{}, settings)
	if !deviceLocalSettings.OutOfBalanceRefillProbe {
		t.Fatal("a hosted device builds window clients its payer's balance refuses")
	}
}
