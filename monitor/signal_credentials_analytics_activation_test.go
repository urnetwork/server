// Exercise activation through the consumer's real Config loader without any
// installed resources, external providers, database work, or wall-clock waits.
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

// Give both resolvers isolated roots so optional lookup cannot fall through to
// a developer's Config or Vault generation.
func analyticsActivationFixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("WARP_ENV", "")
	t.Setenv("WARP_CONFIG_HOME", root)
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	return root
}

// Keep the real inventory assembly while limiting this test's alert scope to
// the integration whose activation is being observed.
func analyticsActivationFixtureSettings(t *testing.T, services []string) SignalSettings {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{})
	settings.Credentials = nil
	for _, requirement := range loadCredentialRequirements("main", false, services) {
		if requirement.Key == "analytics" {
			settings.Credentials = append(settings.Credentials, requirement)
		}
	}
	return settings
}

// Unknown activation must remain observable alongside independent readiness
// failures without retaining the original filesystem or parser detail.
func requireAnalyticsActivationUnknown(t *testing.T, privateValues ...string) {
	t.Helper()
	settings := analyticsActivationFixtureSettings(t, []string{"taskworker"})
	settings.Credentials = append(settings.Credentials, CredentialRequirement{
		Key: "synthetic-payment", Resource: "synthetic-payment.yml", Required: true,
	})
	alerts, err := NewWithSignals(settings, NewCredentialsSignal()).Run(context.Background())
	if err != nil {
		t.Fatal("credential metadata did not remain available to the monitor")
	}
	var unknown *Alert
	for index := range alerts {
		if alerts[index].Class == "credential-activation-unobservable" {
			unknown = &alerts[index]
		}
	}
	if unknown == nil {
		t.Fatal("unavailable or invalid analytics configuration disappeared as disabled instead of remaining observable")
	}
	if unknown.Target != "analytics" || unknown.Frame != "config=analytics.yml" ||
		unknown.SignalID != "deployment/credentials" || unknown.SignalKey != "credentials" ||
		unknown.SignalNumber != "8.7" || unknown.Severity != SeverityWarn ||
		unknown.Sustain != 1 || !unknown.unobservable {
		t.Fatal("unknown analytics activation lost its observation boundary")
	}
	if len(alerts) != 2 || requireAlertClass(t, alerts, "credential-resource-missing").Target != "synthetic-payment" {
		t.Fatal("unknown activation hid an independent required credential failure")
	}
	for _, expected := range []string{"activation=unknown", "configuration_status=unavailable-or-invalid", "Vault completeness was not inferred"} {
		if !strings.Contains(unknown.Markdown(), expected) {
			t.Fatalf("unknown activation omits its fixed metadata %q", expected)
		}
	}
	for _, value := range privateValues {
		if strings.Contains(alerts.Markdown(), value) || strings.Contains(fmt.Sprintf("%+v", settings.Credentials), value) {
			t.Fatal("analytics activation retained private resource detail")
		}
	}
}

// A malformed scalar may be quoted by the YAML decoder; only a fixed unknown
// result may cross into credential settings or Markdown.
func TestAnalyticsActivationMalformedConfigRemainsUnknown(t *testing.T) {
	root := analyticsActivationFixtureRoot(t)
	const privateValue = "synthetic-private-analytics-config-value"
	pop := server.Config.PushSimpleResource("analytics.yml", []byte("enabled: "+privateValue+"\n"))
	defer pop()
	requireAnalyticsActivationUnknown(t, privateValue, root)
}

// Valid YAML whose consumer validation fails is also not an explicit disable.
func TestAnalyticsActivationInvalidConfigRemainsUnknown(t *testing.T) {
	root := analyticsActivationFixtureRoot(t)
	const privateValue = "synthetic-private-invalid-duration"
	pop := server.Config.PushSimpleResource("analytics.yml", []byte("enabled: true\nsearch:\n  enabled: true\ntaskworker:\n  interval: "+privateValue+"\n"))
	defer pop()
	requireAnalyticsActivationUnknown(t, privateValue, root)
}

// A nonregular resource proves lookup failure rather than optional absence.
func TestAnalyticsActivationUnavailableConfigRemainsUnknown(t *testing.T) {
	root := analyticsActivationFixtureRoot(t)
	if err := os.Mkdir(filepath.Join(root, "analytics.yml"), 0o700); err != nil {
		t.Fatal("could not create the nonregular analytics fixture")
	}
	if _, err := server.Config.SimpleResource("analytics.yml"); !errors.Is(err, server.ErrResourceUnavailable) {
		t.Fatal("nonregular analytics fixture did not reach the unavailable lookup boundary")
	}
	requireAnalyticsActivationUnknown(t, root)
}

// A dangling entry cannot prove that an optional resource is absent.
func TestAnalyticsActivationDanglingConfigRemainsUnknown(t *testing.T) {
	root := analyticsActivationFixtureRoot(t)
	const privateTarget = "synthetic-private-missing-config-target"
	if err := os.Symlink(privateTarget, filepath.Join(root, "analytics.yml")); err != nil {
		t.Fatal("could not create the dangling analytics fixture")
	}
	if _, err := server.Config.SimpleResource("analytics.yml"); !errors.Is(err, server.ErrResourceUnavailable) {
		t.Fatal("dangling analytics fixture did not reach the unavailable lookup boundary")
	}
	requireAnalyticsActivationUnknown(t, root, privateTarget)
}

// Authoritative absence retains the consumer's optional no-op contract.
func TestAnalyticsActivationAbsentConfigRetainsOptionalNoop(t *testing.T) {
	analyticsActivationFixtureRoot(t)
	if _, err := server.Config.SimpleResource("analytics.yml"); !errors.Is(err, server.ErrResourceNotFound) || errors.Is(err, server.ErrResourceUnavailable) {
		t.Fatal("empty analytics fixture did not prove optional absence")
	}
	settings := analyticsActivationFixtureSettings(t, []string{"taskworker"})
	alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
	if err != nil || len(settings.Credentials) != 0 || len(alerts) != 0 {
		t.Fatal("authoritatively absent analytics configuration lost its optional no-op")
	}
}

// Valid feature gates remain known no-ops without requiring unused credentials.
func TestAnalyticsActivationDisabledConfigRetainsNoop(t *testing.T) {
	analyticsActivationFixtureRoot(t)
	for _, configuration := range []string{
		"enabled: false\nsearch:\n  enabled: true\n",
		"enabled: true\nsearch:\n  enabled: false\n",
		"enabled: true\nsearch:\n  enabled: true\nproviders:\n  google_search_console:\n    enabled: true\n    mode: manual_import\n",
	} {
		pop := server.Config.PushSimpleResource("analytics.yml", []byte(configuration))
		settings := analyticsActivationFixtureSettings(t, []string{"taskworker"})
		pop()
		alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
		if err != nil || len(settings.Credentials) != 0 || len(alerts) != 0 {
			t.Fatal("valid disabled analytics activation became a credential finding")
		}
	}
}

// An invalid configuration cannot create a credential dependency for an absent
// taskworker; the activation observation is scoped to its known consumer.
func TestAnalyticsActivationUnknownRequiresActiveTaskworker(t *testing.T) {
	analyticsActivationFixtureRoot(t)
	pop := server.Config.PushSimpleResource("analytics.yml", []byte("enabled: [\n"))
	defer pop()
	settings := analyticsActivationFixtureSettings(t, []string{"api"})
	alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
	if err != nil || len(settings.Credentials) != 0 || len(alerts) != 0 {
		t.Fatal("unknown analytics activation escaped its taskworker consumer scope")
	}
}

// The observed missing-field boundary remains a PAGE when both API consumers
// and matching properties are explicitly enabled.
func TestAnalyticsActivationEnabledMissingCredentialsRetainPage(t *testing.T) {
	analyticsActivationFixtureRoot(t)
	popConfig := server.Config.PushSimpleResource("analytics.yml", []byte(`
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
`))
	defer popConfig()
	popVault := server.Vault.PushSimpleResource("analytics.yml", []byte("google_search_console: {}\nbing_webmaster: {}\n"))
	defer popVault()
	settings := analyticsActivationFixtureSettings(t, []string{"taskworker"})
	alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatal("enabled analytics lost its credential readiness finding")
	}
	alert := requireAlertClass(t, alerts, "credential-fields-missing")
	if alert.Severity != SeverityPage || alert.Target != "analytics" || alert.Frame != "resource=analytics.yml" ||
		!strings.Contains(alert.Observed, "missing_fields=bing_webmaster.api_key,google_search_console.service_account_json") {
		t.Fatal("unknown-activation handling changed the enabled missing-credentials boundary")
	}
}

// Repair must produce a new settings generation; the frozen unknown snapshot
// cannot become healthy through later filesystem changes alone.
func TestAnalyticsActivationRecoveryChangesSettingsGeneration(t *testing.T) {
	analyticsActivationFixtureRoot(t)
	popInvalid := server.Config.PushSimpleResource("analytics.yml", []byte("enabled: [\n"))
	startup := analyticsActivationFixtureSettings(t, []string{"taskworker"})
	popInvalid()
	popDisabled := server.Config.PushSimpleResource("analytics.yml", []byte("enabled: false\n"))
	defer popDisabled()
	current := analyticsActivationFixtureSettings(t, []string{"taskworker"})
	check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	unchanged, err := check(context.Background(), startup)
	if err != nil || unchanged {
		t.Fatal("analytics activation repair did not invalidate the unknown startup generation")
	}
	oldAlerts, err := NewCredentialsSignal().Run(context.Background(), startup)
	if err != nil || len(oldAlerts) != 1 || oldAlerts[0].Class != "credential-activation-unobservable" {
		t.Fatal("a later configuration silently cleared the frozen unknown activation")
	}
	newAlerts, err := NewCredentialsSignal().Run(context.Background(), current)
	if err != nil || len(newAlerts) != 0 {
		t.Fatal("a fresh explicit disabled generation did not establish its no-op")
	}
}

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
	if alert.Severity != SeverityWarn || !alert.unobservable || alert.Frame != "config=analytics.yml" {
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
func TestAnalyticsActivationSettingsLoadPreservesMissingCredentialPage(t *testing.T) {
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
