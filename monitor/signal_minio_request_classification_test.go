package monitor

import (
	"sync/atomic"
	"testing"
	"time"
)

// A finite request partition is an incident, even though its class is copied from a table.
func TestMinioRequestErrorClassesRemainIncidents(t *testing.T) {
	cases := []struct {
		class    string
		requests map[string]float64
		severity Severity
	}{
		{class: "minio-write-errors", requests: map[string]float64{"write_requests": 100, "write_5xx": 50}, severity: SeverityPage},
		{class: "minio-read-errors", requests: map[string]float64{"read_requests": 100, "read_5xx": 50}, severity: SeverityPage},
		{class: "minio-write-rejected", requests: map[string]float64{"write_requests": 100, "write_4xx": 50}, severity: SeverityWarn},
	}
	for _, c := range cases {
		store := healthyMinioStore()
		store.requests = c.requests
		alert := requireAlertClass(t, runMinioStorage(t, store), c.class)
		if alert.unobservable || isVisibilityAlert(alert) {
			t.Fatalf("%s request incident became visibility", c.class)
		}
		if alert.Severity != c.severity || alert.Sustain != 2 || alert.Target != minioTestStore {
			t.Fatalf("%s incident policy changed: %+v", c.class, alert)
		}
	}
}

// Real request incidents must not make an independently recovered capacity identity unknown.
func TestMinioRequestErrorsDoNotHoldRecoveredCapacity(t *testing.T) {
	cases := []struct {
		class    string
		requests map[string]float64
	}{
		{class: "minio-write-errors", requests: map[string]float64{"write_requests": 100, "write_5xx": 50}},
		{class: "minio-read-errors", requests: map[string]float64{"read_requests": 100, "read_5xx": 50}},
		{class: "minio-write-rejected", requests: map[string]float64{"write_requests": 100, "write_4xx": 50}},
	}
	for _, c := range cases {
		now := time.Date(2037, 2, 3, 4, 5, 0, 0, time.UTC)
		capacity := healthyMinioStore()
		for i := range capacity.drives {
			capacity.drives[i].freePct = 0.5
		}
		requests := healthyMinioStore()
		requests.requests = c.requests
		healthy := healthyMinioStore()
		states := []minioStoreFixture{capacity, requests, requests, healthy, healthy}
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
		capacityKey := slackIncidentKey(Alert{SignalID: signal.ID(), Class: "minio-drive-capacity", Target: minioTestStore})
		requestKey := slackIncidentKey(Alert{SignalID: signal.ID(), Class: c.class, Target: minioTestStore})
		postSignalRunsThroughTheGate(t, signal, settings, len(states), func(next int) {
			notifier.stateLock.Lock()
			_, capacityPresent := notifier.keyIncidents[capacityKey]
			_, requestPresent := notifier.keyIncidents[requestKey]
			notifier.stateLock.Unlock()
			if (next == 1 || next == 2) && !capacityPresent {
				t.Fatalf("%s cleared capacity before two complete recovered observations", c.class)
			}
			if next == 2 && requestPresent {
				t.Fatalf("%s raised before its sustain gate", c.class)
			}
			if next == 3 || next == 4 {
				if capacityPresent {
					t.Fatalf("%s incident held independently recovered capacity", c.class)
				}
				if !requestPresent {
					t.Fatalf("%s request incident missing before two healthy observations", c.class)
				}
			}
			run.Store(int32(next))
			harness.advance(time.Minute)
		}, notifier)
		notifier.stateLock.Lock()
		_, requestPresent := notifier.keyIncidents[requestKey]
		notifier.stateLock.Unlock()
		if requestPresent {
			t.Fatalf("%s did not clear after two complete healthy observations", c.class)
		}
		closeSlackNotifier(t, notifier)
		posts := webhook.posted()
		if countSlackPosts(posts, "[PAGE] raised", "minio-drive-capacity · "+minioTestStore) != 1 ||
			countSlackPosts(posts, "[CLEARED]", "minio-drive-capacity · "+minioTestStore) != 1 ||
			countSlackPosts(posts, "[CLEARED]", c.class+" · "+minioTestStore) != 1 {
			t.Fatalf("%s composed transition counts changed", c.class)
		}
	}
}

// A genuine source gap still holds every request incident through held and ready visibility.
func TestMinioRequestErrorIncidentsSurviveUnavailableSource(t *testing.T) {
	cases := []struct {
		class    string
		requests map[string]float64
	}{
		{class: "minio-write-errors", requests: map[string]float64{"write_requests": 100, "write_5xx": 50}},
		{class: "minio-read-errors", requests: map[string]float64{"read_requests": 100, "read_5xx": 50}},
		{class: "minio-write-rejected", requests: map[string]float64{"write_requests": 100, "write_4xx": 50}},
	}
	for _, c := range cases {
		now := time.Date(2037, 2, 3, 4, 5, 0, 0, time.UTC)
		requests := healthyMinioStore()
		requests.requests = c.requests
		unknown := healthyMinioStore()
		unknown.omit = map[string]bool{"drive_health": true}
		healthy := healthyMinioStore()
		states := []minioStoreFixture{requests, requests, unknown, unknown, healthy, healthy}
		var run atomic.Int32
		settings := minioStorageSettings(t, now, "unused")
		settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			return minioStorageFixtureJSON(t, now, states[int(run.Load())]), nil
		}}
		_, webhookUrl := newFakeSlackWebhook(t)
		harness := newSlackTestHarness()
		notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
		t.Cleanup(func() { closeSlackNotifier(t, notifier) })
		signal := NewMinioStorageSignal()
		key := slackIncidentKey(Alert{SignalID: signal.ID(), Class: c.class, Target: minioTestStore})
		postSignalRunsThroughTheGate(t, signal, settings, len(states), func(next int) {
			if next >= 2 {
				notifier.stateLock.Lock()
				_, present := notifier.keyIncidents[key]
				notifier.stateLock.Unlock()
				if !present {
					t.Fatalf("%s source gap or first healthy run cleared the incident", c.class)
				}
			}
			run.Store(int32(next))
			harness.advance(time.Minute)
		}, notifier)
		notifier.stateLock.Lock()
		_, present := notifier.keyIncidents[key]
		notifier.stateLock.Unlock()
		if present {
			t.Fatalf("%s did not clear after two complete healthy observations", c.class)
		}
	}
}

// Finite incident declarations do not weaken visibility for source failures or unknown classes.
func TestMinioRequestClassificationKeepsUnknownFallback(t *testing.T) {
	store := healthyMinioStore()
	store.omit = map[string]bool{"drive_health": true}
	alert := requireAlertClass(t, runMinioStorage(t, store), "minio-metrics-unobservable")
	if !alert.unobservable || !isVisibilityAlert(alert) {
		t.Fatal("missing drive authority stopped reporting visibility")
	}
	if !isVisibilityAlertClass("synthetic-unregistered-minio-partition") {
		t.Fatal("unregistered class lost conservative visibility fallback")
	}
}
