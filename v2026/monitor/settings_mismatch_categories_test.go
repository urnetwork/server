package monitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// Exercise the actual scheduled comparison and rendered Alert with secret-like
// values, map keys and paths; only the fixed categories may cross the boundary.
func TestSettingsMismatchCategoriesActualSignalPreservesPrivacy(t *testing.T) {
	settings, current := settingsFreshnessFixture(t)
	secret := strings.Repeat("synthetic-private-value-", 4096)
	current.PostgreSQL.Password = secret
	current.LogServiceHosts = map[string][]string{"synthetic-private-map-key": {"private-placement.example"}}
	current.SSHKeyPaths = []string{"/synthetic/private-key"}
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "settings-generation-stale")
	if alert.Observed != "settings_generation_current=false mismatch_categories=log_inventory,ssh_policy,postgresql" {
		t.Fatalf("fixed mismatch categories lost: %s", alert.Observed)
	}
	if alert.Severity != SeverityWarn || alert.PageSustain != 5 || alert.Target != "monitor-settings" {
		t.Fatal("diagnostic changed the stale policy")
	}
	for _, private := range []string{secret, "synthetic-private-map-key", "private-placement.example", "/synthetic/private-key", settings.PostgreSQL.Password} {
		if strings.Contains(alert.Markdown(), private) {
			t.Fatal("mismatch diagnostic exposed a private value")
		}
	}
	if len(alert.Observed) > 512 {
		t.Fatal("private payload expanded the fixed diagnostic")
	}
}

// Equality still ignores process-only seams and uses exactly one current load.
func TestSettingsMismatchCategoriesHealthyAndDirectGuardParity(t *testing.T) {
	settings, current := settingsFreshnessFixture(t)
	calls := 0
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
		calls++
		return *current, nil
	})
	current.Source = &syntheticSource{}
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || calls != 1 {
		t.Fatalf("healthy comparison changed: alerts=%d loads=%d err=%v", len(alerts), calls, err)
	}
	current.PostgreSQL.Password = "synthetic-rotated-secret"
	matches, err := settings.SettingsGenerationCheck(context.Background(), settings)
	if err != nil || matches || calls != 2 {
		t.Fatal("request-local Boolean guard changed")
	}
}

// A custom check has no typed comparison evidence and cannot invent categories.
func TestSettingsMismatchCategoriesCustomAndAmbiguousChecksStayUnknown(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		settings, current := settingsFreshnessFixture(t)
		current.PostgreSQL.Password = "synthetic-current-secret"
		check := settings.SettingsGenerationCheck
		settings.SettingsGenerationCheck = func(ctx context.Context, startup SignalSettings) (bool, error) {
			if multiple {
				if _, err := check(ctx, startup); err != nil {
					return false, err
				}
				if _, err := check(ctx, startup); err != nil {
					return false, err
				}
			}
			return false, nil
		}
		alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "settings-generation-stale")
		if alert.Observed != "settings_generation_current=false mismatch_categories=unknown" {
			t.Fatal("custom or ambiguous comparison gained field authority")
		}
	}
}

// Loader failure and cancellation keep their existing authority and perform no
// diagnostic reload, value rendering or stale comparison.
func TestSettingsMismatchCategoriesUnavailableAndCanceledKeepAuthority(t *testing.T) {
	settings, _ := settingsFreshnessFixture(t)
	calls := 0
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
		calls++
		return SignalSettings{}, errors.New("synthetic-private-resource-error")
	})
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil || calls != 1 {
		t.Fatal("loader error was retried or replaced")
	}
	alert := requireAlertClass(t, alerts, "settings-generation-unobservable")
	if strings.Contains(alert.Markdown(), "mismatch_categories") || strings.Contains(alert.Markdown(), "synthetic-private-resource-error") {
		t.Fatal("failed load gained mismatch authority or exposed its value")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err = NewSettingsFreshnessSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 1 {
		t.Fatal("cancellation changed source or load count")
	}
}

// Per-invocation state cannot cross independent concurrent freshness callbacks.
func TestSettingsMismatchCategoriesConcurrentSignalsRemainSeparate(t *testing.T) {
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, category := range []string{"public_scope", "postgresql"} {
		settings, current := settingsFreshnessFixture(t)
		if category == "public_scope" {
			current.PublicDomain = "changed.example"
		} else {
			current.PostgreSQL.Password = "synthetic-other-secret"
		}
		workers.Add(1)
		go func(settings SignalSettings, category string) {
			defer workers.Done()
			<-start
			alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
			if err != nil {
				results <- err
				return
			}
			for _, alert := range alerts {
				if alert.Class == "settings-generation-stale" && alert.Observed == "settings_generation_current=false mismatch_categories="+category {
					results <- nil
					return
				}
			}
			results <- errors.New("separate mismatch category lost or crossed")
		}(settings, category)
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Even a composed callback using parallel checks cannot race the private
// diagnostic into falsely authoritative single-comparison categories.
func TestSettingsMismatchCategoriesParallelComposedChecksStayUnknown(t *testing.T) {
	settings, current := settingsFreshnessFixture(t)
	current.PostgreSQL.Password = "synthetic-parallel-secret"
	check := settings.SettingsGenerationCheck
	settings.SettingsGenerationCheck = func(ctx context.Context, startup SignalSettings) (bool, error) {
		start := make(chan struct{})
		results := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() {
				<-start
				_, err := check(ctx, startup)
				results <- err
			}()
		}
		close(start)
		for i := 0; i < 2; i++ {
			if err := <-results; err != nil {
				return false, err
			}
		}
		return false, nil
	}
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "settings-generation-stale")
	if alert.Observed != "settings_generation_current=false mismatch_categories=unknown" {
		t.Fatal("parallel comparisons gained single-check authority")
	}
}

// Count every actual check entry, even when a composed callback discards a
// second failure/cancellation or returns while a second loader is still active.
func TestSettingsMismatchCategoriesIgnoredFailedCanceledAndInflightChecksStayUnknown(t *testing.T) {
	for _, mode := range []string{"failed", "canceled", "inflight"} {
		settings, current := settingsFreshnessFixture(t)
		current.PostgreSQL.Password = "synthetic-current-mismatch"
		calls := 0
		entered := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
			calls++
			if calls == 2 && mode == "failed" {
				return SignalSettings{}, errors.New("synthetic-ignored-load-error")
			}
			if calls == 2 && mode == "inflight" {
				close(entered)
				<-release
			}
			return *current, nil
		})
		check := settings.SettingsGenerationCheck
		settings.SettingsGenerationCheck = func(ctx context.Context, startup SignalSettings) (bool, error) {
			if _, err := check(ctx, startup); err != nil {
				return false, err
			}
			switch mode {
			case "failed":
				_, _ = check(ctx, startup)
			case "canceled":
				canceledCtx, cancel := context.WithCancel(ctx)
				cancel()
				_, _ = check(canceledCtx, startup)
			case "inflight":
				go func() {
					defer close(done)
					_, _ = check(ctx, startup)
				}()
				<-entered
			}
			return false, nil
		}
		alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
		if mode == "inflight" {
			close(release)
			<-done
		}
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "settings-generation-stale")
		if alert.Observed != "settings_generation_current=false mismatch_categories=unknown" {
			t.Errorf("%s attempt gained single-comparison category authority", mode)
		}
		wantCalls := 2
		if mode == "canceled" {
			wantCalls = 1
		}
		if calls != wantCalls {
			t.Errorf("%s comparison load count changed: got %d want %d", mode, calls, wantCalls)
		}
		if strings.Contains(alert.Markdown(), "synthetic-ignored-load-error") {
			t.Fatal("ignored error entered mismatch output")
		}
	}
}
