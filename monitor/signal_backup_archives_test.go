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
	heartbeat  *time.Time
	sampleAt   time.Time
}

type backupArchiveWriterFixture struct {
	unitState          string
	mainPID            int64
	remoteUnitState    string
	remoteResult       string
	remoteRestart      string
	remoteRestartDelay string
	remoteExitStatus   int64
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

func TestBackupArchivesSignalSyntheticDetectsStaleActiveWriterProgress(t *testing.T) {
	now := time.Date(2026, 9, 1, 23, 56, 0, 0, time.UTC)
	zero := float64(0)
	one := float64(1)
	createdAt := now.Add(-time.Hour)
	staleHeartbeat := now.Add(-2 * time.Hour)
	base := []backupArchiveFixture{
		{archive: "pg", generation: "main-pg-current.sql.xz", createdAt: &createdAt, progress: &zero},
		{archive: "redis", generation: "main-redis-current", createdAt: &createdAt, progress: &zero},
		{archive: "github-urnetwork", generation: "urnetwork.tar.xz", createdAt: &createdAt, progress: &zero, heartbeat: &staleHeartbeat},
		{archive: "github-urfoundation", generation: "urfoundation.tar.xz", createdAt: &createdAt, progress: &zero, heartbeat: &staleHeartbeat},
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState: "activating", mainPID: 156738,
	}, base...)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-progress-stale", "backup-1/github")
	for _, want := range []string{
		"unit_state=activating",
		"main_pid=156738",
		"heartbeat_age=2h0m0s",
		"published_progress_total=0",
		"metrics-heartbeat-stale",
		"active-unit-progress-total-not-one",
		"Fluent Bit assigns a fresh scrape timestamp",
		"Xops commit 2733b0b",
		"already-running pre-fix shell will not gain that behavior",
		"rather than restarting this one",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stale active-writer alert missing %q:\n%s", want, alert.Markdown())
		}
	}

	healthy := append([]backupArchiveFixture(nil), base...)
	freshHeartbeat := now.Add(-30 * time.Second)
	healthy[2].progress = &one
	healthy[2].heartbeat = &freshHeartbeat
	healthy[3].heartbeat = &freshHeartbeat
	healthyAlerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState: "activating", mainPID: 156738,
	}, healthy...)
	if unexpected := findBackupArchiveAlert(healthyAlerts, "backup-archive-progress-stale", "backup-1/github"); unexpected != nil {
		t.Fatalf("fresh single-owner active progress alerted: %+v", *unexpected)
	}
}

func TestBackupArchivesSignalSyntheticRejectsMalformedWriterObservation(t *testing.T) {
	valid := backupArchiveWriterFixtureText(backupArchiveWriterFixture{})
	for _, testCase := range []struct {
		name   string
		output string
		want   string
	}{
		{name: "missing", output: "github_unit_state=activating", want: "expected 7 properties"},
		{name: "state", output: strings.Replace(valid, "github_unit_state=inactive", "github_unit_state=ACTIVE", 1), want: "invalid github_unit_state"},
		{name: "pid", output: strings.Replace(valid, "github_main_pid=0", "github_main_pid=nope", 1), want: "invalid main PID"},
		{name: "delay", output: strings.Replace(valid, "remote_restart_delay=30min", "remote_restart_delay=immediate!", 1), want: "invalid remote_restart_delay"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseBackupArchiveWriterObservation("backup-1", testCase.output)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("parse error=%v, want substring %q", err, testCase.want)
			}
		})
	}
}

func TestBackupArchivesSignalSyntheticDetectsDisabledPullRetry(t *testing.T) {
	now := time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState:          "inactive",
		remoteUnitState:    "failed",
		remoteResult:       "exit-code",
		remoteRestart:      "no",
		remoteRestartDelay: "100ms",
		remoteExitStatus:   1,
	}, fixtures...)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-retry-disabled", "backup-1/remote")
	if alert.Sustain != 1 {
		t.Fatalf("retry policy sustain=%d, want 1", alert.Sustain)
	}
	for _, want := range []string{
		"result=exit-code",
		"exit_status=1",
		"restart=no",
		"restart_delay=100ms",
		"Xops commit 2311114",
		"cannot unlock LUKS",
		"does not authorize a catch-up pull",
		"run-planetoid.sh",
		"RestartUSec=30min",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("disabled retry alert missing %q:\n%s", want, alert.Markdown())
		}
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
	return runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState: "inactive",
	}, fixtures...)
}

func runBackupArchiveFixturesWithWriter(
	t testing.TB,
	now time.Time,
	writer backupArchiveWriterFixture,
	fixtures ...backupArchiveFixture,
) Alerts {
	t.Helper()
	payload := backupArchiveFixtureJSON(t, now, fixtures...)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name == "backup-1" && strings.Contains(command, "monitor-signal-11.22-backup-archives") {
			return backupArchiveWriterFixtureText(writer), nil
		}
		if host.Name != "metrics-1" ||
			!strings.Contains(command, "urnetwork_backup_archive_%28latest_timestamp_seconds%7Cin_progress%7Cheartbeat_timestamp_seconds%29") ||
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

func backupArchiveWriterFixtureText(fixture backupArchiveWriterFixture) string {
	if fixture.unitState == "" {
		fixture.unitState = "inactive"
	}
	if fixture.remoteUnitState == "" {
		fixture.remoteUnitState = "inactive"
	}
	if fixture.remoteResult == "" {
		fixture.remoteResult = "success"
	}
	if fixture.remoteRestart == "" {
		fixture.remoteRestart = "on-failure"
	}
	if fixture.remoteRestartDelay == "" {
		fixture.remoteRestartDelay = "30min"
	}
	return fmt.Sprintf(
		"github_unit_state=%s\n"+
			"github_main_pid=%d\n"+
			"remote_unit_state=%s\n"+
			"remote_result=%s\n"+
			"remote_restart=%s\n"+
			"remote_restart_delay=%s\n"+
			"remote_exit_status=%d\n",
		fixture.unitState,
		fixture.mainPID,
		fixture.remoteUnitState,
		fixture.remoteResult,
		fixture.remoteRestart,
		fixture.remoteRestartDelay,
		fixture.remoteExitStatus,
	)
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
		if fixture.heartbeat != nil {
			metric := map[string]string{"__name__": "urnetwork_backup_archive_heartbeat_timestamp_seconds"}
			for key, value := range baseLabels {
				metric[key] = value
			}
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(sampleAt.Unix()), fmt.Sprintf("%d", fixture.heartbeat.Unix())},
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
