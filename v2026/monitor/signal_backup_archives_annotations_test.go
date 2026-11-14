package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Adds only envelope metadata; the data object is retained byte for byte.
func backupArchiveAnnotatedPayload(t testing.TB, payload string, warnings []string) string {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		t.Fatal("invalid synthetic envelope")
	}
	encoded, err := json.Marshal(warnings)
	if err != nil {
		t.Fatal("cannot encode synthetic warning metadata")
	}
	envelope["warnings"] = encoded
	output, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal("cannot encode synthetic envelope")
	}
	var roundTrip map[string]json.RawMessage
	if err := json.Unmarshal(output, &roundTrip); err != nil || string(roundTrip["data"]) != string(envelope["data"]) {
		t.Fatal("annotation perturbation changed the metric data")
	}
	return string(output)
}

// Uses the public signal transport with only visibly synthetic configured hosts.
func backupArchiveAnnotationSettings(now time.Time, payload func() string, writer func() (string, error), metricCalls, writerCalls *atomic.Int32) SignalSettings {
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name == "backup-1" && strings.Contains(command, "monitor-signal-11.22-backup-archives") {
			writerCalls.Add(1)
			return writer()
		}
		if host.Name == "metrics-1" && strings.Contains(command, "urnetwork_backup_archive_%28latest_timestamp_seconds%7Cin_progress%7Cheartbeat_timestamp_seconds%7Cintegrity_checked_timestamp_seconds%29") &&
			strings.Contains(command, "host%3D~%22backup-1%22") && strings.Contains(command, "env%3D%22synthetic%22") {
			metricCalls.Add(1)
			return payload(), nil
		}
		return "", fmt.Errorf("unexpected synthetic observation route")
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "backup-1", Roles: []string{"backup"}, Backup: &BackupHostSettings{
			PGSource: "by@203.0.113.10", PGPort: 8022,
			RedisSource: "by@203.0.113.11", RedisPort: 8023,
		}},
		HostSettings{Name: "metrics-1", Roles: []string{"services"}},
	)
	return settings
}

// Complete health becomes unknown when only warning metadata changes.
func TestBackupArchivesAnnotationsPublicCompleteThenWarnings(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	clean := backupArchiveFixtureJSON(t, now, healthyBackupArchiveFixtures(now)...)
	const canary = "synthetic-private-backup-warning-canary"
	for _, warnings := range [][]string{nil, {}, {canary}, {""}} {
		payload := backupArchiveAnnotatedPayload(t, clean, warnings)
		var metricCalls, writerCalls atomic.Int32
		settings := backupArchiveAnnotationSettings(now, func() string { return payload }, func() (string, error) {
			return backupArchiveWriterFixtureText(backupArchiveWriterFixture{}), nil
		}, &metricCalls, &writerCalls)
		alerts, err := NewBackupArchivesSignal().Run(context.Background(), settings)
		if err != nil || metricCalls.Load() != 1 || writerCalls.Load() != 1 {
			t.Fatal("valid public metric and writer route did not complete exactly once")
		}
		if len(warnings) == 0 {
			if len(alerts) != 0 {
				t.Fatal("empty warning list changed complete health")
			}
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || alerts[0].Target != "backup-archives/mimir" ||
			alerts[0].Frame != "response-warnings" || alerts[0].Sustain != 2 || !isVisibilityAlert(alerts[0]) || alerts[0].Severity != SeverityWarn {
			t.Fatal("warning-only perturbation incorrectly attested complete archive health")
		}
		encoded, err := json.Marshal(alerts)
		if err != nil || strings.Contains(string(encoded), canary) || strings.Contains(alerts[0].Markdown(), canary) {
			t.Fatal("query warning text escaped public output")
		}
	}
}

// Omitted newer artifacts and corrective checks can reverse current-state claims.
func TestBackupArchivesAnnotationsOmittedCorrectiveRowsRemainUnknown(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	old := now.Add(-6 * 24 * time.Hour)
	oldCheck := now.Add(-time.Hour)
	partial := healthyBackupArchiveFixtures(now)
	partial[0].createdAt = &old
	partial[0].generation = "pg-old-synthetic"
	partial[0].integrityResult = "invalid"
	partial[0].integrityCheckedAt = &oldCheck
	corrective := healthyBackupArchiveFixtures(now)[0]
	complete := append(append([]backupArchiveFixture(nil), partial...), corrective)
	// The corrective generation must not duplicate the Boolean gauge.
	complete[len(complete)-1].progress = nil
	for index, payload := range []string{
		backupArchiveFixtureJSON(t, now, complete...),
		backupArchiveFixtureJSON(t, now, partial...),
		backupArchiveAnnotatedPayload(t, backupArchiveFixtureJSON(t, now, partial...), []string{"synthetic-omitted-corrective-row"}),
	} {
		var metricCalls, writerCalls atomic.Int32
		settings := backupArchiveAnnotationSettings(now, func() string { return payload }, func() (string, error) {
			return backupArchiveWriterFixtureText(backupArchiveWriterFixture{}), nil
		}, &metricCalls, &writerCalls)
		alerts, err := NewBackupArchivesSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal("synthetic corrective-row control failed to execute")
		}
		if index == 0 && len(alerts) != 0 {
			t.Fatal("newest corrected complete vector was not healthy")
		}
		stale := findBackupArchiveAlert(alerts, "backup-archive-stale", "backup-1/pg")
		invalid := findBackupArchiveAlert(alerts, "backup-archive-integrity-invalid", "backup-1/pg")
		if index == 1 {
			if stale == nil || invalid == nil {
				t.Fatal("clean partial-vector control did not exercise both reversible claims")
			}
		} else if stale != nil || invalid != nil {
			t.Fatal("omitted corrective rows were incorrectly treated as authoritative current state")
		}
		if index == 2 && (len(alerts) != 1 || !isVisibilityAlert(alerts[0])) {
			t.Fatal("warned partial subset did not retain an explicit unknown boundary")
		}
	}
}

// Returned contract violations and direct writer failures survive incomplete queries.
func TestBackupArchivesAnnotationsPreserveIndependentFailures(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	invalidProgress := float64(2)
	fixtures[1].progress = &invalidProgress
	payload := backupArchiveAnnotatedPayload(t, backupArchiveFixtureJSON(t, now, fixtures...), []string{"synthetic-warning"})
	var metricCalls, writerCalls atomic.Int32
	settings := backupArchiveAnnotationSettings(now, func() string { return payload }, func() (string, error) {
		return backupArchiveWriterFixtureText(backupArchiveWriterFixture{
			remoteUnitState: "failed", remoteResult: "exit-code", remoteExitStatus: 1,
		}), nil
	}, &metricCalls, &writerCalls)
	alerts, err := NewBackupArchivesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("independent synthetic failure control did not execute")
	}
	for _, expected := range []struct{ class, target string }{
		{class: "cannot-observe", target: "backup-archives/mimir"},
		{class: "backup-archive-metrics-invalid", target: "backup-1/redis"},
		{class: "backup-archive-run-failed", target: "backup-1/remote"},
	} {
		requireBackupArchiveAlert(t, alerts, expected.class, expected.target)
	}
	if len(alerts) != 3 {
		t.Fatal("incomplete query invented additional current-state failures")
	}
}

// Warned metrics cannot supply heartbeat/progress reasons or internal health.
func TestBackupArchivesAnnotationsInternalHealthAndDirectProgressFailure(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	payload := backupArchiveAnnotatedPayload(t, backupArchiveFixtureJSON(t, now, healthyBackupArchiveFixtures(now)...), []string{"synthetic-warning"})
	var metricCalls, writerCalls atomic.Int32
	settings := backupArchiveAnnotationSettings(now, func() string { return payload }, func() (string, error) {
		return backupArchiveWriterFixtureText(backupArchiveWriterFixture{unitState: "active"}), nil
	}, &metricCalls, &writerCalls)
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal("invalid synthetic probe settings")
	}
	findings, err := (backupArchivesProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal("valid synthetic probe did not complete")
	}
	directProgressFailure := false
	for _, value := range findings {
		if value.healthy && (strings.Contains(value.target, "/pg") || strings.Contains(value.target, "/redis") ||
			strings.Contains(value.target, "/github-") || value.class == "backup-archive-progress-stale" || value.class == "backup-archive-recovery-idle") {
			t.Fatal("incomplete metric response emitted an internal healthy predicate")
		}
		if value.class == "backup-archive-progress-stale" && !value.healthy {
			directProgressFailure = strings.Contains(value.observed, "reasons=active-unit-without-main-pid") &&
				!strings.Contains(value.observed, "github-heartbeat-incomplete") && !strings.Contains(value.observed, "active-unit-progress-total-not-one")
		}
	}
	if !directProgressFailure {
		t.Fatal("warning erased a directly proven writer identity failure or invented joined reasons")
	}
}

// Source failure cannot turn positive-looking stdout into successful authority.
func TestBackupArchivesAnnotationsWriterErrorCannotAttestHealth(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	payload := backupArchiveFixtureJSON(t, now, healthyBackupArchiveFixtures(now)...)
	var metricCalls, writerCalls atomic.Int32
	settings := backupArchiveAnnotationSettings(now, func() string { return payload }, func() (string, error) {
		return backupArchiveWriterFixtureText(backupArchiveWriterFixture{}), errors.New("synthetic source failure")
	}, &metricCalls, &writerCalls)
	alerts, err := NewBackupArchivesSignal().Run(context.Background(), settings)
	if err == nil || len(alerts) != 0 || metricCalls.Load() != 1 || writerCalls.Load() != 1 {
		t.Fatal("failed writer transport accepted positive-looking stdout")
	}
}

// Drives the public signal, actual cadence gate and notifier across warned runs.
func TestBackupArchivesAnnotationsNotifierRequiresCompleteRecovery(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	old := now.Add(-6 * 24 * time.Hour)
	stale := healthyBackupArchiveFixtures(now)
	stale[0].createdAt = &old
	clean := backupArchiveFixtureJSON(t, now, healthyBackupArchiveFixtures(now)...)
	const canary = "synthetic-private-backup-notifier-canary"
	warning := backupArchiveAnnotatedPayload(t, clean, []string{canary})
	payloads := []string{backupArchiveFixtureJSON(t, now, stale...), warning, warning, warning, clean, clean}
	var run, metricCalls, writerCalls atomic.Int32
	settings := backupArchiveAnnotationSettings(now, func() string { return payloads[run.Load()] }, func() (string, error) {
		return backupArchiveWriterFixtureText(backupArchiveWriterFixture{}), nil
	}, &metricCalls, &writerCalls)
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifierSettings := harness.settings(webhookUrl)
	notifierSettings.ClearAfterCompleteRuns = 2
	notifier := NewSlackAlertNotifier(notifierSettings)
	const incidentKey = "observability/backup-archives|backup-archive-stale|backup-1/pg"
	postSignalRunsThroughTheGate(t, NewBackupArchivesSignal(), settings, len(payloads), func(index int) {
		if 1 <= index {
			notifier.stateLock.Lock()
			incident := notifier.keyIncidents[incidentKey]
			absentRuns := -1
			if incident != nil {
				absentRuns = incident.signalKeyAbsentRuns["backup-archives"]
			}
			notifier.stateLock.Unlock()
			expectedAbsent := 0
			if index == 5 {
				expectedAbsent = 1
			}
			if absentRuns != expectedAbsent {
				t.Fatal("warned ready or held visibility incorrectly counted toward archive recovery")
			}
		}
		run.Store(int32(index))
	}, notifier)
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	const identity = "observability/backup-archives · backup-archive-stale · backup-1/pg"
	if countSlackPosts(texts, "[PAGE] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 1 {
		t.Fatal("clean recovery control did not clear the single original archive incident")
	}
	if metricCalls.Load() != int32(len(payloads)) || writerCalls.Load() != int32(len(payloads)) {
		t.Fatal("notifier control bypassed a required public source observation")
	}
	if strings.Contains(strings.Join(texts, "\n"), canary) || strings.Contains(strings.Join(harness.logLines(), "\n"), canary) {
		t.Fatal("private warning text escaped through notifier output")
	}
}

// An empty warned subset cannot prove missing archives or idle recovery work.
func TestBackupArchivesAnnotationsEmptySubsetIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	payload := backupArchiveAnnotatedPayload(t, backupArchiveFixtureJSON(t, now), []string{""})
	var metricCalls, writerCalls atomic.Int32
	settings := backupArchiveAnnotationSettings(now, func() string { return payload }, func() (string, error) {
		return backupArchiveWriterFixtureText(backupArchiveWriterFixture{
			remoteInvocationID: "none", remoteExecStart: 0,
		}), nil
	}, &metricCalls, &writerCalls)
	alerts, err := NewBackupArchivesSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !isVisibilityAlert(alerts[0]) {
		t.Fatal("empty warned subset became missing-archive or recovery-idle authority")
	}
}
