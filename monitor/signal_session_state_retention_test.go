// Synthetic Mimir evaluations exercise the session state retention signal
// through its public Run. No Mimir is read.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// One synthetic evaluation: each class's value, absent when not listed.
type sessionStateRetentionFixture map[string]float64

func (self sessionStateRetentionFixture) with(changes sessionStateRetentionFixture, removed ...string) sessionStateRetentionFixture {
	values := sessionStateRetentionFixture{}
	for class, value := range self {
		values[class] = value
	}
	for class, value := range changes {
		values[class] = value
	}
	for _, class := range removed {
		delete(values, class)
	}
	return values
}

// A retention that runs and makes progress on both tables.
func sessionStateRetentionHealthyFixture() sessionStateRetentionFixture {
	return sessionStateRetentionFixture{
		"runs":                                  960,
		"provider_work_session_head_error_runs": 0,
		"provider_work_session_head_progress_runs": 480,
		"audit_device_state_error_runs":            0,
		"audit_device_state_progress_runs":         480,
	}
}

var sessionStateRetentionFixtureNow = time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)

// A Mimir vector for the probe's query, its samples age old.
func sessionStateRetentionFixtureJSON(t testing.TB, values sessionStateRetentionFixture, age time.Duration) string {
	t.Helper()
	result := []map[string]any{}
	for class, value := range values {
		result = append(result, map[string]any{
			"metric": map[string]string{"monitor_metric": class},
			"value":  []any{float64(sessionStateRetentionFixtureNow.Add(-age).Unix()), fmt.Sprintf("%g", value)},
		})
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

// Runs the signal over payload, checking the query it sends.
func runSessionStateRetentionPayload(t *testing.T, payload string) []Alert {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			"urnetwork_session_state_retention_runs_total",
			"table%3D%22provider_work_session_head%22%2Cstop%3D%22error%22",
			"table%3D%22audit_device_state%22%2Cstop%3D~%22pass%7Cbudget%7Cwaiting%22",
			"%5B2h%5D",
			"%22synthetic%22",
			"monitor_metric",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("session state retention query missing %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return sessionStateRetentionFixtureNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := NewSessionStateRetentionSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func runSessionStateRetentionFixture(t *testing.T, values sessionStateRetentionFixture) []Alert {
	t.Helper()
	return runSessionStateRetentionPayload(t, sessionStateRetentionFixtureJSON(t, values, 0))
}

// The classes the alerts raise, in any order.
func sessionStateRetentionAlertClasses(alerts []Alert) map[string]bool {
	classes := map[string]bool{}
	for _, alert := range alerts {
		classes[alert.Class] = true
	}
	return classes
}

func TestSessionStateRetentionRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "session-state-retention" {
			continue
		}
		found++
		if signal.Number() != "2.4t" || signal.Cadence() != 5*time.Minute || signal.ID() != "mimir/session-state-retention" {
			t.Fatal("the session state retention signal lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4t session state retention signal is not registered")
	}
}

// A retention that runs and makes progress, one whose brake is off but still
// runs, and errors among a table's runs that make progress raise nothing.
func TestSessionStateRetentionHealthy(t *testing.T) {
	for name, values := range map[string]sessionStateRetentionFixture{
		"progress": sessionStateRetentionHealthyFixture(),
		"brake off": sessionStateRetentionHealthyFixture().with(sessionStateRetentionFixture{
			"provider_work_session_head_progress_runs": 0, "audit_device_state_progress_runs": 0,
		}),
		"errors and progress": sessionStateRetentionHealthyFixture().with(sessionStateRetentionFixture{
			"provider_work_session_head_error_runs": 40, "provider_work_session_head_progress_runs": 400,
		}),
	} {
		if alerts := runSessionStateRetentionFixture(t, values); len(alerts) != 0 {
			t.Fatalf("%s raised %v", name, sessionStateRetentionAlertClasses(alerts))
		}
	}
}

// No run in the range is a stalled chain.
func TestSessionStateRetentionStalled(t *testing.T) {
	alerts := runSessionStateRetentionFixture(t, sessionStateRetentionHealthyFixture().with(sessionStateRetentionFixture{
		"runs": 0, "provider_work_session_head_progress_runs": 0, "audit_device_state_progress_runs": 0,
	}))
	if classes := sessionStateRetentionAlertClasses(alerts); len(classes) != 1 || !classes["session-state-retention-stalled"] {
		t.Fatalf("a stalled chain raised %v", classes)
	}
	alert := requireAlertClass(t, alerts, "session-state-retention-stalled")
	for _, want := range []string{"No session state retention run ended in 2h", "remove_expired_session_state", "SIGNALS.md §2.4t"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stalled alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

// One table's errors with none of its runs making progress are a failing
// chain, though the other table's pass runs on; fewer than the threshold are
// not.
func TestSessionStateRetentionFailing(t *testing.T) {
	alerts := runSessionStateRetentionFixture(t, sessionStateRetentionHealthyFixture().with(sessionStateRetentionFixture{
		"provider_work_session_head_error_runs": 30, "provider_work_session_head_progress_runs": 0,
	}))
	if classes := sessionStateRetentionAlertClasses(alerts); len(classes) != 1 || !classes["session-state-retention-failing"] {
		t.Fatalf("a failing head pass raised %v", classes)
	}
	alert := requireAlertClass(t, alerts, "session-state-retention-failing")
	if !strings.Contains(alert.Markdown(), "runs for provider_work_session_head failed in 2h") || strings.Contains(alert.Markdown(), "provider_work_session_head and audit_device_state failed") {
		t.Fatalf("the failing alert does not name the head table alone:\n%s", alert.Markdown())
	}
	alerts = runSessionStateRetentionFixture(t, sessionStateRetentionHealthyFixture().with(sessionStateRetentionFixture{
		"audit_device_state_error_runs": 2, "audit_device_state_progress_runs": 0,
	}))
	if classes := sessionStateRetentionAlertClasses(alerts); len(classes) != 0 {
		t.Fatalf("two errors raised %v", classes)
	}
}

// Missing, stale or duplicate parts are unobservable, not zeros, and a proven
// stall still raises beside it.
func TestSessionStateRetentionUnobservable(t *testing.T) {
	alerts := runSessionStateRetentionFixture(t, sessionStateRetentionHealthyFixture().with(nil, "audit_device_state_progress_runs"))
	if classes := sessionStateRetentionAlertClasses(alerts); len(classes) != 1 || !classes["session-state-retention-unobservable"] {
		t.Fatalf("a missing part raised %v", classes)
	}
	alerts = runSessionStateRetentionPayload(t, sessionStateRetentionFixtureJSON(t, sessionStateRetentionHealthyFixture(), time.Hour))
	if classes := sessionStateRetentionAlertClasses(alerts); len(classes) != 1 || !classes["session-state-retention-unobservable"] {
		t.Fatalf("stale parts raised %v", classes)
	}
	alerts = runSessionStateRetentionFixture(t, sessionStateRetentionFixture{"runs": 0})
	if classes := sessionStateRetentionAlertClasses(alerts); len(classes) != 2 || !classes["session-state-retention-unobservable"] || !classes["session-state-retention-stalled"] {
		t.Fatalf("a stall with missing parts raised %v", classes)
	}
}
