package monitor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSettingsFreshnessCatalogNamesClassesAndSummaryBoundary(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	start := strings.Index(catalog, "### 1.6 Monitor settings generation freshness")
	end := strings.Index(catalog, "## 2. pg signal catalog")
	if start < 0 || end <= start {
		t.Fatal("settings-freshness catalog section boundaries are missing")
	}
	section := catalog[start:end]
	for _, class := range []string{"settings-generation-stale", "settings-generation-unobservable"} {
		if !strings.Contains(section, "`"+class+"`") {
			t.Errorf("SIGNALS.md §1.6 omits emitted class %q", class)
		}
		if !strings.Contains(catalog, "| "+class) && !strings.Contains(catalog, "/ "+class) {
			t.Errorf("Tier summary omits emitted class %q", class)
		}
	}
}

func settingsFreshnessFixture(t *testing.T) (SignalSettings, *SignalSettings) {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{})
	settings.PostgreSQL.Password = "synthetic-startup-secret"
	current := settings
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
		return current, nil
	})
	return settings, &current
}

func TestSettingsFreshnessSignalAcceptsUnchangedEffectiveSettings(t *testing.T) {
	settings, _ := settingsFreshnessFixture(t)
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("unchanged settings returned alerts: %+v", alerts)
	}
}

// Excluded topology remains part of the complete desired-state generation;
// suppressing its contacts must not hide a changed address or desired member.
func TestSettingsFreshnessHostScopeRetainsExcludedDesiredStateChanges(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = []HostSettings{{Name: "excluded.example.test", LANAddress: "192.0.2.1"}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	current := settings
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) { return ExcludeHosts(current, "excluded.example.test") })
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
		t.Fatalf("unchanged policy became stale: %d %v", len(alerts), err)
	}
	current.Hosts = append([]HostSettings(nil), current.Hosts...)
	current.Hosts[0].LANAddress = "192.0.2.2"
	alerts, err = NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "settings-generation-stale")
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "excluded.example.test", "192.0.2.1", "192.0.2.2")
	}
}

// Failure of the effective policy reload is unknown, not stale/healthy; the
// failed error body and selected values never reach Markdown.
func TestSettingsFreshnessHostScopeReloadFailureAndCancellation(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = []HostSettings{{Name: "excluded.example.test"}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
		calls++
		return SignalSettings{}, errors.New("synthetic-secret-loader-error")
	})
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil || calls != 1 {
		t.Fatalf("reload failure was not observed once: %d %v", calls, err)
	}
	requireAlertClass(t, alerts, "settings-generation-unobservable")
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "synthetic-secret-loader-error", "excluded.example.test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err = NewSettingsFreshnessSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 1 {
		t.Fatalf("canceled freshness produced coverage or loaded settings: %d %d %v", len(alerts), calls, err)
	}
}

func TestSettingsFreshnessComparisonIgnoresOnlyProcessSeams(t *testing.T) {
	settings, current := settingsFreshnessFixture(t)
	current.Now = func() time.Time { return time.Unix(1, 0) }
	current.Source = &syntheticSource{}
	current.runtime = &signalRuntime{remoteCommands: newHostCommandLimiter(1)}
	current.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
		return false, errors.New("synthetic checker must be ignored")
	}

	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("process-only seam changes returned alerts: %+v", alerts)
	}

	current.PublicDomain = "changed.example.test"
	alerts, err = NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "settings-generation-stale")
}

func TestSettingsFreshnessSignalReportsChangedSettingsWithoutRetainingValues(t *testing.T) {
	settings, current := settingsFreshnessFixture(t)
	current.PostgreSQL.Password = "synthetic-current-secret"

	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "settings-generation-stale")
	if alert.Severity != SeverityWarn || alert.PageSustain != 5 || alert.Target != "monitor-settings" {
		t.Fatalf("unexpected changed-settings identity: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"settings_generation_current=false",
		"controlled overlap/promotion",
		"startup generation",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("changed-settings Markdown omits %q:\n%s", want, markdown)
		}
	}
	requireAlertOmits(t, alert, settings.PostgreSQL.Password, current.PostgreSQL.Password)
}

func TestSettingsFreshnessSignalReportsReloadFailureAsUnobservableAndRedactsError(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	privateError := "synthetic resource contained private-marker"
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
		return SignalSettings{}, errors.New(privateError)
	})

	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "settings-generation-unobservable")
	if !strings.Contains(alert.Observed, "settings_generation_observable=false") ||
		!strings.Contains(alert.Observed, "error_class=") {
		t.Fatalf("unobservable reduction is incomplete: %q", alert.Observed)
	}
	requireAlertOmits(t, alert, privateError, "private-marker")
}

func TestSettingsFreshnessSignalPropagatesCancellationWithoutAlert(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	loaderCalled := false
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
		loaderCalled = true
		return settings, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	alerts, err := NewSettingsFreshnessSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run error = %v, want context.Canceled", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("canceled run returned alerts: %+v", alerts)
	}
	if loaderCalled {
		t.Fatal("canceled check invoked the settings loader")
	}
}

func TestSettingsFreshnessSignalNoopsForManualSettingsWithoutGenerationCheck(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("manual settings returned alerts: %+v", alerts)
	}
}
