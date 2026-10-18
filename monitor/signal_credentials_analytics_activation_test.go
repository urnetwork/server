// Analytics readiness must preserve unknown activation through the real
// settings loader, without inventing provider credentials or leaking config.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Installs only synthetic resources. The signal under test reads metadata and
// never contacts the synthetic host or initializes a database pool.
func analyticsActivationFixture(t *testing.T, service string) string {
	t.Helper()
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_HOME", t.TempDir())
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	configHome := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", configHome)
	t.Setenv("WARP_SITE_HOME", t.TempDir())
	t.Cleanup(server.Vault.PushSimpleResource("monitor.yml", []byte("ssh:\n  user: synthetic\nhosts:\n  - name: synthetic-pg\n    overlay_ip: 192.0.2.10\n    roles: [pg-primary]\n")))
	t.Cleanup(server.Vault.PushSimpleResource("services.yml", []byte("domain: synthetic.example\nversions:\n  - services:\n      "+service+":\n        blocks:\n          - synthetic: 100\n")))
	return configHome
}

// Restricts assertions to analytics, while preserving every other requirement
// emitted by the production settings assembly.
func analyticsActivationAlerts(t *testing.T) (SignalSettings, Alerts) {
	t.Helper()
	settings, err := LoadSignalSettings()
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	analyticsAlerts := Alerts{}
	for _, alert := range alerts {
		if alert.Target == "analytics" {
			analyticsAlerts = append(analyticsAlerts, alert)
		}
	}
	return settings, analyticsAlerts
}

// Unknown activation is visibility, so it cannot clear a previously observed
// credential incident; no parser detail or configuration value is retained.
func requireUnknownAnalyticsActivation(t *testing.T, privateMarker string) SignalSettings {
	t.Helper()
	settings, alerts := analyticsActivationAlerts(t)
	if len(alerts) != 1 || alerts[0].Class != "credential-activation-unobservable" {
		t.Fatalf("analytics alerts = %+v; want one unknown activation finding", alerts)
	}
	alert := alerts[0]
	if alert.Severity != SeverityWarn || !alert.unobservable || alert.Frame != "config_resource=analytics.yml" {
		t.Fatalf("unknown activation lost visibility identity: %+v", alert)
	}
	if !strings.Contains(alert.Observed, "activation=unknown") {
		t.Fatalf("activation uncertainty absent: %s", alert.Observed)
	}
	if privateMarker != "" {
		requireAlertOmits(t, alert, privateMarker)
		if strings.Contains(fmt.Sprintf("%+v", settings.Credentials), privateMarker) {
			t.Fatal("configuration value escaped into credential readiness")
		}
	}
	return settings
}

// Malformed YAML and shared validation failures used to return no fields and
// disappear exactly like disabled analytics.
func TestAnalyticsActivationInvalidConfigRemainsObservable(t *testing.T) {
	analyticsActivationFixture(t, "taskworker")
	for _, config := range []string{
		"enabled: [synthetic-private-activation-marker]\n",
		"enabled: true\nsearch:\n  enabled: true\ntaskworker:\n  interval: synthetic-private-activation-marker\n",
	} {
		pop := server.Config.PushSimpleResource("analytics.yml", []byte(config))
		requireUnknownAnalyticsActivation(t, "synthetic-private-activation-marker")
		pop()
	}
}

// A directory where a resource file belongs is a deterministic unavailable
// lookup, independent of privilege or filesystem permission conventions.
func TestAnalyticsActivationUnavailableConfigRemainsObservable(t *testing.T) {
	configHome := analyticsActivationFixture(t, "taskworker")
	if err := os.Mkdir(filepath.Join(configHome, "analytics.yml"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := model.LoadAnalyticsConfig(); !errors.Is(err, server.ErrResourceUnavailable) {
		t.Fatalf("unavailable analytics config became disabled: %v", err)
	}
	requireUnknownAnalyticsActivation(t, configHome)
}

// Genuine absence and successfully loaded disabled policies keep their
// existing optional contract.
func TestAnalyticsActivationAbsentAndDisabledNoop(t *testing.T) {
	analyticsActivationFixture(t, "taskworker")
	for _, config := range []string{
		"",
		"enabled: false\n",
		"enabled: true\nsearch:\n  enabled: false\n",
		"enabled: true\nsearch:\n  enabled: true\nproviders:\n  google_search_console:\n    enabled: true\n    mode: import\n  bing_webmaster:\n    enabled: true\n    mode: api\n    protocol: soap\nsites:\n  - name: synthetic\n    origin: https://synthetic.example\n    properties:\n      google_search_console: synthetic-property\n      bing_webmaster: synthetic-property\n",
	} {
		pop := func() {}
		if config != "" {
			pop = server.Config.PushSimpleResource("analytics.yml", []byte(config))
		}
		settings, alerts := analyticsActivationAlerts(t)
		pop()
		if len(alerts) != 0 {
			t.Fatalf("disabled or unused analytics emitted alerts: %+v", alerts)
		}
		for _, requirement := range settings.Credentials {
			if requirement.Key == "analytics" {
				t.Fatal("disabled or unused analytics retained a credential requirement")
			}
		}
	}
}

// Without a declared taskworker, unknown collector activation must not invent
// an active consumer from the presence of an API service.
func TestAnalyticsActivationUnknownFollowsTaskworkerScope(t *testing.T) {
	analyticsActivationFixture(t, "api")
	t.Cleanup(server.Config.PushSimpleResource("analytics.yml", []byte("enabled: [\n")))
	_, alerts := analyticsActivationAlerts(t)
	if len(alerts) != 0 {
		t.Fatalf("inactive taskworker emitted analytics visibility: %+v", alerts)
	}
}

// Known enabled providers retain the original two-field missing-credential
// PAGE; the visibility correction cannot weaken that readiness contract.
func TestAnalyticsActivationEnabledMissingCredentialsRetainPage(t *testing.T) {
	analyticsActivationFixture(t, "taskworker")
	t.Cleanup(server.Config.PushSimpleResource("analytics.yml", []byte(`
enabled: true
search:
  enabled: true
providers:
  google_search_console:
    enabled: true
    mode: api
  bing_webmaster:
    enabled: true
    mode: api
    protocol: rest
sites:
  - name: synthetic
    origin: https://synthetic.example
    properties:
      google_search_console: synthetic-property
      bing_webmaster: synthetic-property
`)))
	t.Cleanup(server.Vault.PushSimpleResource("analytics.yml", []byte("{}\n")))
	_, alerts := analyticsActivationAlerts(t)
	if len(alerts) != 1 || alerts[0].Class != "credential-fields-missing" || alerts[0].Severity != SeverityPage || alerts[0].unobservable {
		t.Fatalf("enabled missing credentials lost their PAGE: %+v", alerts)
	}
	if !strings.Contains(alerts[0].Observed, "missing_fields=bing_webmaster.api_key,google_search_console.service_account_json") {
		t.Fatalf("enabled credentials lost exact missing fields: %s", alerts[0].Observed)
	}
}

// The generation checker must distinguish unknown activation from a known
// disabled successor, otherwise an old watcher could silently clear findings.
func TestAnalyticsActivationGenerationChangeIsVisible(t *testing.T) {
	analyticsActivationFixture(t, "taskworker")
	pop := server.Config.PushSimpleResource("analytics.yml", []byte("enabled: [\n"))
	settings, err := LoadSignalSettings()
	pop()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Config.PushSimpleResource("analytics.yml", []byte("enabled: false\n")))
	same, err := settings.SettingsGenerationCheck(context.Background(), settings)
	if err != nil || same {
		t.Fatalf("unknown-to-disabled generation: same=%t err=%v", same, err)
	}
}
