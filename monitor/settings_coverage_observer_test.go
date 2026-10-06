package monitor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A local output writer is attached after loading settings. It neither changes
// the desired generation nor runs while that generation is compared.
func TestSettingsFreshnessCoverageObserverIsProcessOnly(t *testing.T) {
	for _, placement := range []string{"startup", "current", "same", "different"} {
		t.Run(placement, func(t *testing.T) {
			settings, current := settingsFreshnessFixture(t)
			calls := 0
			observer := func(UrlProbeCoverageObservation) error { calls++; return nil }
			if placement != "current" {
				settings.UrlProbeCoverageObserver = observer
			}
			if placement == "current" || placement == "same" {
				current.UrlProbeCoverageObserver = observer
			} else if placement == "different" {
				current.UrlProbeCoverageObserver = func(UrlProbeCoverageObservation) error { calls++; return nil }
			}
			alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 || calls != 0 {
				t.Fatal("local coverage writer changed the effective generation or ran during comparison")
			}
			current.PostgreSQL.Password = "synthetic-current-secret"
			alerts, err = NewSettingsFreshnessSignal().Run(context.Background(), settings)
			if err != nil || calls != 0 {
				t.Fatal("credential comparison failed or invoked the coverage writer")
			}
			alert := requireAlertClass(t, alerts, "settings-generation-stale")
			requireAlertOmits(t, alert, settings.PostgreSQL.Password, current.PostgreSQL.Password)
		})
	}
}

// Use the real settings-to-probe wiring and a synthetic host transport. The
// output callback must permit a current sample while preserving both the
// durable fifteen-minute floor and the guard against a real settings change.
func TestPgQuerySampleCoverageObserverPreservesGenerationAndCadence(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	contacts, observerCalls := 0, 0
	settings := syntheticSettings(&syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
		contacts++
		return pgSampleTestEncode(t, pgSampleTestFrames(now)), nil
	}})
	settings.Now = func() time.Time { return now }
	settings.StateDir = t.TempDir()
	settings.PGQuerySampleContinuous = true
	current := settings
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	settings.UrlProbeCoverageObserver = func(UrlProbeCoverageObservation) error { observerCalls++; return nil }
	env, err := newProbeEnv(settings.withDefaults().withRuntime())
	if err != nil {
		t.Fatal(err)
	}
	probe := pgQuerySampleProbe{}
	findings, err := probe.check(context.Background(), env)
	if err != nil || contacts != 1 || observerCalls != 0 {
		t.Fatal("coverage writer prevented the current-generation sample or was invoked by it")
	}
	for _, finding := range findings {
		if finding.class == "pg-query-sample-unavailable" {
			t.Fatal("completed synthetic sample became unavailable")
		}
	}
	dir := filepath.Join(settings.StateDir, "pg-query-sample")
	state, err := pgSampleReadCadence(dir, now)
	if err != nil || state.Outcome != "complete" || !state.NextEligibleAt.Equal(now.Add(15*time.Minute)) {
		t.Fatal("coverage output changed successful sample custody or cadence")
	}
	if _, err := probe.check(context.Background(), env); err != nil || contacts != 1 {
		t.Fatal("coverage output bypassed the durable sampling floor")
	}
	before, err := os.ReadFile(filepath.Join(dir, "continuous.json"))
	if err != nil {
		t.Fatal(err)
	}
	current.PostgreSQL.Password = "synthetic-new-credential"
	now = state.NextEligibleAt
	_, err = probe.check(context.Background(), env)
	var preflight *pgSamplePreflightError
	if !errors.As(err, &preflight) || preflight.reason != pgSamplePreflightGenerationStale || contacts != 1 || observerCalls != 0 {
		t.Fatal("real settings change passed the generation guard")
	}
	after, err := os.ReadFile(filepath.Join(dir, "continuous.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stale generation changed the retained sampling cadence")
	}
}
