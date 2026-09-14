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
