package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type backupArchiveFixture struct {
	archive    string
	generation string
	createdAt  *time.Time
	progress   *float64
	sampleAt   time.Time
}

func TestBackupArchivesSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for index, archive := range backupArchiveNames {
		createdAt := now.Add(-time.Duration(index+1) * 12 * time.Hour)
		progress := float64(0)
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &progress,
		})
	}
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	if len(alerts) != 0 {
		t.Fatalf("healthy backup archives alerted: %+v", alerts)
	}
}

func TestBackupArchivesSignalSyntheticQuotedCollectorOmission(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 1, 0, 0, time.UTC)
	alerts := runBackupArchiveFixtures(t, now)
	if len(alerts) != len(backupArchiveNames)*2 {
		t.Fatalf("collector omission alerts=%d, want %d: %+v", len(alerts), len(backupArchiveNames)*2, alerts)
	}
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-metrics-missing", "backup-1/pg")
	if alert.SignalNumber != "11.22" || alert.SignalKey != "backup-archives" || alert.Sustain != 2 {
		t.Fatalf("wrong backup metrics signal identity: %+v", alert)
	}
	for _, want := range []string{
		"classic-config quotes",
		"textfile\"",
		"node_uname_info",
		"stdout-only textfile collector",
		"wrapping quotes must not be present",
		"SIGNALS.md §11.22",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("backup metrics alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestBackupArchivesSignalSyntheticStaleAndMissingGenerations(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 2, 0, 0, time.UTC)
	zero := float64(0)
	stale := now.Add(-12 * 24 * time.Hour)
	fresh := now.Add(-24 * time.Hour)
	alerts := runBackupArchiveFixtures(t, now,
		backupArchiveFixture{archive: "pg", generation: "main-pg-old.sql.xz", createdAt: &stale, progress: &zero},
		backupArchiveFixture{archive: "redis", generation: "main-redis-current", createdAt: &fresh, progress: &zero},
		backupArchiveFixture{archive: "github-urnetwork", progress: &zero},
		backupArchiveFixture{archive: "github-urfoundation", progress: &zero},
	)
	staleAlert := requireBackupArchiveAlert(t, alerts, "backup-archive-stale", "backup-1/pg")
	for _, want := range []string{
		"generation=main-pg-old.sql.xz",
		"12 days",
		"absent udisks mount",
		"Start a catch-up run only with operator authorization",
		"cannot create a new recovery point",
	} {
		if !strings.Contains(staleAlert.Markdown(), want) {
			t.Fatalf("stale archive alert missing %q:\n%s", want, staleAlert.Markdown())
		}
	}
	missing := requireBackupArchiveAlert(t, alerts, "backup-archive-missing", "backup-1/github-urnetwork")
	if !strings.Contains(missing.Markdown(), "in_progress=0") ||
		!strings.Contains(missing.Markdown(), "no recoverable generation") {
		t.Fatalf("missing archive alert lacks completion semantics:\n%s", missing.Markdown())
	}
	if findBackupArchiveAlert(alerts, "backup-archive-stale", "backup-1/redis") != nil {
		t.Fatalf("fresh Redis archive was marked stale: %+v", alerts)
	}
}

func TestBackupArchivesSignalSyntheticStaleScrapeIsObservationLoss(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 3, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt,
			progress: &zero, sampleAt: now.Add(-2 * time.Minute),
		})
	}
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	missing := requireBackupArchiveAlert(t, alerts, "backup-archive-metrics-missing", "backup-1/pg")
	if !strings.Contains(missing.Observed, "stale_scrape_samples=2") {
		t.Fatalf("stale samples were not kept as visibility evidence: %s", missing.Observed)
	}
	if findBackupArchiveAlert(alerts, "backup-archive-stale", "backup-1/pg") != nil {
		t.Fatalf("stale scrape was misclassified as stale archive: %+v", alerts)
	}
}

func TestBackupArchivesSignalSyntheticRejectsInvalidMetrics(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 4, 0, 0, time.UTC)
	invalidProgress := float64(2)
	future := now.Add(10 * time.Minute)
	alerts := runBackupArchiveFixtures(t, now,
		backupArchiveFixture{archive: "pg", generation: "future", createdAt: &future, progress: &invalidProgress},
	)
	invalid := requireBackupArchiveAlert(t, alerts, "backup-archive-metrics-invalid", "backup-1/pg")
	for _, want := range []string{"future_timestamp=", "value=2", "single-writer exposition"} {
		if !strings.Contains(invalid.Markdown(), want) {
			t.Fatalf("invalid metric alert missing %q:\n%s", want, invalid.Markdown())
		}
	}
}

func runBackupArchiveFixtures(t testing.TB, now time.Time, fixtures ...backupArchiveFixture) Alerts {
	t.Helper()
	payload := backupArchiveFixtureJSON(t, now, fixtures...)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" ||
			!strings.Contains(command, "urnetwork_backup_archive_%28latest_timestamp_seconds%7Cin_progress%29") ||
			!strings.Contains(command, "host%3D~%22backup-1%22") ||
			!strings.Contains(command, "env%3D%22synthetic%22") {
			return "", fmt.Errorf("unexpected backup Mimir command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "backup-1", Roles: []string{"backup"}},
		HostSettings{Name: "metrics-1", Roles: []string{"services"}},
	)
	alerts, err := NewBackupArchivesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func backupArchiveFixtureJSON(t testing.TB, now time.Time, fixtures ...backupArchiveFixture) string {
	t.Helper()
	result := []map[string]any{}
	for _, fixture := range fixtures {
		sampleAt := fixture.sampleAt
		if sampleAt.IsZero() {
			sampleAt = now
		}
		baseLabels := map[string]string{
			"env": "synthetic", "host": "backup-1", "archive": fixture.archive,
		}
		if fixture.createdAt != nil {
			metric := map[string]string{"__name__": "urnetwork_backup_archive_latest_timestamp_seconds"}
			for key, value := range baseLabels {
				metric[key] = value
			}
			metric["generation"] = fixture.generation
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(sampleAt.Unix()), fmt.Sprintf("%d", fixture.createdAt.Unix())},
			})
		}
		if fixture.progress != nil {
			metric := map[string]string{"__name__": "urnetwork_backup_archive_in_progress"}
			for key, value := range baseLabels {
				metric[key] = value
			}
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(sampleAt.Unix()), fmt.Sprintf("%.0f", *fixture.progress)},
			})
		}
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func requireBackupArchiveAlert(t testing.TB, alerts Alerts, class, target string) Alert {
	t.Helper()
	alert := findBackupArchiveAlert(alerts, class, target)
	if alert == nil {
		t.Fatalf("no %s alert for %s in %+v", class, target, alerts)
	}
	return *alert
}

func findBackupArchiveAlert(alerts Alerts, class, target string) *Alert {
	for index := range alerts {
		if alerts[index].Class == class && alerts[index].Target == target {
			return &alerts[index]
		}
	}
	return nil
}
