// Independent erasure-set write-health evidence cannot be vetoed by drive caches.
package monitor

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Change only fresh tolerance partitions in the existing complete synthetic frame.
func minioTolerancePayload(t *testing.T, now time.Time, store minioStoreFixture, change func(map[string]any) []any) string {
	t.Helper()
	result := []any{}
	for _, item := range store.series(now) {
		row := item.(map[string]any)
		if row["metric"].(map[string]string)["monitor_check"] == "set_write_tolerance" && change != nil {
			result = append(result, change(row)...)
		} else {
			result = append(result, row)
		}
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal("invalid synthetic tolerance frame")
	}
	return string(payload)
}

// Negative producer evidence is affirmative with online, healing or offline drives.
func TestMinioToleranceIndependentNegativePublicRun(t *testing.T) {
	for _, testCase := range []struct {
		healing bool
		offline bool
	}{{}, {healing: true}, {offline: true}} {
		store := healthyMinioStore()
		if testCase.healing {
			store.drives[0].health = 2
		}
		if testCase.offline {
			store.drives[0].health = 0
			store.offline = 1
		}
		store.tolerance = -1
		alerts := runMinioStorage(t, store)
		page := requireAlertClass(t, alerts, "minio-drive-offline")
		if page.Severity != SeverityPage || page.Sustain != 1 || !strings.Contains(page.Observed, "write_tolerance=-1") || !strings.Contains(page.Context, "separate evidence") {
			t.Fatal("independent negative did not retain immediate cached-health PAGE")
		}
	}
}

// Scrape freshness cannot join two independent producer caches into one generation.
func TestMinioToleranceDifferentFreshClocksDoNotVetoNegative(t *testing.T) {
	now := time.Date(2037, 1, 2, 3, 4, 0, 0, time.UTC)
	store := healthyMinioStore()
	store.tolerance = -1
	payload := minioTolerancePayload(t, now, store, func(row map[string]any) []any {
		row["value"].([]any)[0] = float64(now.Add(-time.Minute).Unix())
		return []any{row}
	})
	alerts, err := NewMinioStorageSignal().Run(t.Context(), minioStorageSettings(t, now, payload))
	if err != nil {
		t.Fatal("fresh independent cache fixture failed")
	}
	if page := requireAlertClass(t, alerts, "minio-drive-offline"); page.Severity != SeverityPage || page.Sustain != 1 {
		t.Fatal("drive cache vetoed older but fresh write-health evidence")
	}
}

// Missing, stale, malformed, duplicate or no-reserve tolerance cannot grant clears.
func TestMinioToleranceUnavailableDoesNotQualifyHealthy(t *testing.T) {
	now := time.Date(2037, 1, 2, 3, 4, 0, 0, time.UTC)
	for _, mode := range []string{"missing", "stale", "fraction", "nan", "duplicate", "unlabelled", "zero"} {
		store := healthyMinioStore()
		payload := minioTolerancePayload(t, now, store, func(row map[string]any) []any {
			switch mode {
			case "missing":
				return nil
			case "stale":
				row["value"].([]any)[0] = float64(now.Add(-minioStorageFreshness - time.Second).Unix())
			case "fraction":
				row["value"].([]any)[1] = "-0.5"
			case "nan":
				row["value"].([]any)[1] = "NaN"
			case "duplicate":
				return []any{row, row}
			case "unlabelled":
				delete(row["metric"].(map[string]string), "set_id")
			case "zero":
				row["value"].([]any)[1] = "0"
			}
			return []any{row}
		})
		settings := minioStorageSettings(t, now, payload)
		signal := NewMinioStorageSignal()
		alerts, err := signal.Run(t.Context(), settings)
		if err != nil {
			t.Fatal("unavailable tolerance fixture failed")
		}
		requireNoMinioAlert(t, alerts, "minio-drive-offline")
		unknown := requireAlertClass(t, alerts, "minio-metrics-unobservable")
		if unknown.Frame != "write-tolerance" || !unknown.unobservable || unknown.Severity != SeverityWarn || unknown.Sustain != 2 {
			t.Fatal("unavailable tolerance gained healthy or outage authority")
		}
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal("invalid synthetic settings")
		}
		findings, err := (minioStorageProbe{}).check(t.Context(), env)
		if err != nil {
			t.Fatal("unavailable reducer failed")
		}
		for _, f := range findings {
			if f.class == "minio-drive-offline" && f.healthy {
				t.Fatal("unavailable tolerance emitted internal healthy sentinel")
			}
		}
	}
}

// A qualified negative survives missing independent drive-source evidence.
func TestMinioToleranceNegativeSurvivesDriveGap(t *testing.T) {
	store := healthyMinioStore()
	store.tolerance = -1
	store.omit = map[string]bool{"drive_health": true}
	alerts := runMinioStorage(t, store)
	page := requireAlertClass(t, alerts, "minio-drive-offline")
	if page.Severity != SeverityPage || page.Sustain != 1 || !strings.Contains(page.Observed, "offline=0") {
		t.Fatal("missing drive cache hid health failure or invented offline drives")
	}
	if unknown := requireAlertClass(t, alerts, "minio-metrics-unobservable"); unknown.Frame != "drives" {
		t.Fatal("negative erased independent drive observation gap")
	}
}

// Distinct observed sets use the weakest value without inventing a set census.
func TestMinioToleranceWeakestDistinctSet(t *testing.T) {
	now := time.Date(2037, 1, 2, 3, 4, 0, 0, time.UTC)
	payload := minioTolerancePayload(t, now, healthyMinioStore(), func(row map[string]any) []any {
		second := map[string]any{"metric": map[string]string{"host": minioTestStore, "monitor_check": "set_write_tolerance", "pool_id": "0", "set_id": "1"}, "value": []any{float64(now.Unix()), "-1"}}
		return []any{row, second}
	})
	alerts, err := NewMinioStorageSignal().Run(t.Context(), minioStorageSettings(t, now, payload))
	if err != nil {
		t.Fatal("distinct set fixture failed")
	}
	if page := requireAlertClass(t, alerts, "minio-drive-offline"); page.Sustain != 1 || !strings.Contains(page.Observed, "write_tolerance=-1") {
		t.Fatal("weakest observed set lost independent failure")
	}
}

// Tolerance visibility does not suppress independently measured sibling failures.
func TestMinioToleranceGapPreservesIndependentFailures(t *testing.T) {
	store := healthyMinioStore()
	store.omit = map[string]bool{"set_write_tolerance": true}
	store.drives[0].freePct = 3
	store.buckets[2].usage = store.buckets[2].quota
	store.requests["write_5xx"] = 500
	alerts := runMinioStorage(t, store)
	for _, class := range []string{"minio-metrics-unobservable", "minio-drive-capacity", "minio-bucket-quota", "minio-write-errors"} {
		requireAlertClass(t, alerts, class)
	}
}

// Existing healthy, healing and ordinary offline bands retain their identities.
func TestMinioToleranceHealthyAndOrdinaryDriveControls(t *testing.T) {
	if alerts := runMinioStorage(t, healthyMinioStore()); len(alerts) != 0 {
		t.Fatal("complete healthy store changed")
	}
	store := healthyMinioStore()
	store.drives[0].health = 2
	if alert := requireAlertClass(t, runMinioStorage(t, store), "minio-drive-offline"); alert.Severity != SeverityWarn || alert.Sustain != 2 {
		t.Fatal("ordinary healing control changed")
	}
	store = healthyMinioStore()
	store.drives[0].health = 0
	store.offline = 1
	store.tolerance = 0
	if alert := requireAlertClass(t, runMinioStorage(t, store), "minio-drive-offline"); alert.Severity != SeverityPage || alert.Sustain != 2 {
		t.Fatal("ordinary offline control changed")
	}
}

// Public executions pass the real sustain gate and notifier; unknown runs cannot clear.
func TestMinioToleranceNotifierRequiresCompleteRecovery(t *testing.T) {
	now := time.Date(2037, 1, 2, 3, 4, 0, 0, time.UTC)
	broken := healthyMinioStore()
	broken.tolerance = -1
	missing := healthyMinioStore()
	missing.omit = map[string]bool{"set_write_tolerance": true}
	healthy := healthyMinioStore()
	states := []minioStoreFixture{broken, missing, missing, healthy, missing, healthy, healthy}
	var run atomic.Int32
	settings := minioStorageSettings(t, now, "unused")
	settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return minioStorageFixtureJSON(t, now, states[int(run.Load())]), nil
	}}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	signal := NewMinioStorageSignal()
	key := slackIncidentKey(Alert{SignalID: signal.ID(), Class: "minio-drive-offline", Target: minioTestStore})
	postSignalRunsThroughTheGate(t, signal, settings, len(states), func(next int) {
		if next > 0 {
			notifier.stateLock.Lock()
			_, present := notifier.keyIncidents[key]
			notifier.stateLock.Unlock()
			if !present {
				t.Fatal("unknown tolerance cleared incident before two complete observations")
			}
			harness.advance(time.Minute)
		}
		run.Store(int32(next))
	}, notifier)
	notifier.stateLock.Lock()
	_, present := notifier.keyIncidents[key]
	notifier.stateLock.Unlock()
	if present {
		t.Fatal("second final complete healthy observation did not clear incident")
	}
	closeSlackNotifier(t, notifier)
	posts := webhook.posted()
	identity := "minio-drive-offline · " + minioTestStore
	if countSlackPosts(posts, "[PAGE] raised", identity) != 1 || countSlackPosts(posts, "[CLEARED]", identity) != 1 {
		t.Fatal("tolerance recovery gate notification count changed")
	}
}
