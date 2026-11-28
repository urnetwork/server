// Synthetic Mimir evaluations exercise the client location reliability
// retention signal through its public Run. No Mimir is read.
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
type clientLocationReliabilityRetentionFixture map[string]float64

func (self clientLocationReliabilityRetentionFixture) with(changes clientLocationReliabilityRetentionFixture, removed ...string) clientLocationReliabilityRetentionFixture {
	values := clientLocationReliabilityRetentionFixture{}
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

// A retention whose walk runs and makes progress.
func clientLocationReliabilityRetentionHealthyFixture() clientLocationReliabilityRetentionFixture {
	return clientLocationReliabilityRetentionFixture{
		"runs":          480,
		"error_runs":    0,
		"progress_runs": 480,
	}
}

var clientLocationReliabilityRetentionFixtureNow = time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)

// A Mimir vector for the probe's query, its samples age old.
func clientLocationReliabilityRetentionFixtureJSON(t testing.TB, values clientLocationReliabilityRetentionFixture, age time.Duration) string {
	t.Helper()
	result := []map[string]any{}
	for class, value := range values {
		result = append(result, map[string]any{
			"metric": map[string]string{"monitor_metric": class},
			"value":  []any{float64(clientLocationReliabilityRetentionFixtureNow.Add(-age).Unix()), fmt.Sprintf("%g", value)},
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
func runClientLocationReliabilityRetentionPayload(t *testing.T, payload string) []Alert {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			"urnetwork_client_location_reliability_retention_runs_total",
			"stop%3D%22error%22",
			"stop%3D~%22caught_up%7Cbudget%22",
			"%5B2h%5D",
			"%22synthetic%22",
			"monitor_metric",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("client location reliability retention query missing %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return clientLocationReliabilityRetentionFixtureNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := NewClientLocationReliabilityRetentionSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func runClientLocationReliabilityRetentionFixture(t *testing.T, values clientLocationReliabilityRetentionFixture) []Alert {
	t.Helper()
	return runClientLocationReliabilityRetentionPayload(t, clientLocationReliabilityRetentionFixtureJSON(t, values, 0))
}

// The classes the alerts raise, in any order.
func clientLocationReliabilityRetentionAlertClasses(alerts []Alert) map[string]bool {
	classes := map[string]bool{}
	for _, alert := range alerts {
		classes[alert.Class] = true
	}
	return classes
}

func TestClientLocationReliabilityRetentionRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "location-reliability-retention" {
			continue
		}
		found++
		if signal.Number() != "2.4u" || signal.Cadence() != 5*time.Minute || signal.ID() != "mimir/location-reliability-retention" {
			t.Fatal("the client location reliability retention signal lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4u client location reliability retention signal is not registered")
	}
}

// A walk that runs and makes progress, one whose brake is off but still runs,
// and errors among runs that make progress raise nothing.
func TestClientLocationReliabilityRetentionHealthy(t *testing.T) {
	for name, values := range map[string]clientLocationReliabilityRetentionFixture{
		"progress":            clientLocationReliabilityRetentionHealthyFixture(),
		"brake off":           clientLocationReliabilityRetentionHealthyFixture().with(clientLocationReliabilityRetentionFixture{"progress_runs": 0}),
		"errors and progress": clientLocationReliabilityRetentionHealthyFixture().with(clientLocationReliabilityRetentionFixture{"error_runs": 40, "progress_runs": 400}),
	} {
		if alerts := runClientLocationReliabilityRetentionFixture(t, values); len(alerts) != 0 {
			t.Fatalf("%s raised %v", name, clientLocationReliabilityRetentionAlertClasses(alerts))
		}
	}
}

// No run in the range is a stalled chain.
func TestClientLocationReliabilityRetentionStalled(t *testing.T) {
	alerts := runClientLocationReliabilityRetentionFixture(t, clientLocationReliabilityRetentionHealthyFixture().with(clientLocationReliabilityRetentionFixture{
		"runs": 0, "progress_runs": 0,
	}))
	if classes := clientLocationReliabilityRetentionAlertClasses(alerts); len(classes) != 1 || !classes["location-reliability-retention-stalled"] {
		t.Fatalf("a stalled chain raised %v", classes)
	}
	alert := requireAlertClass(t, alerts, "location-reliability-retention-stalled")
	for _, want := range []string{"No client location reliability retention run ended in 2h", "remove_old_client_location_reliabilities", "SIGNALS.md §2.4u"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stalled alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

// Errors with no run making progress are a failing walk; fewer than the
// threshold are not.
func TestClientLocationReliabilityRetentionFailing(t *testing.T) {
	alerts := runClientLocationReliabilityRetentionFixture(t, clientLocationReliabilityRetentionHealthyFixture().with(clientLocationReliabilityRetentionFixture{
		"error_runs": 30, "progress_runs": 0,
	}))
	if classes := clientLocationReliabilityRetentionAlertClasses(alerts); len(classes) != 1 || !classes["location-reliability-retention-failing"] {
		t.Fatalf("a failing walk raised %v", classes)
	}
	alert := requireAlertClass(t, alerts, "location-reliability-retention-failing")
	for _, want := range []string{"walk failed in 2h", "maxRowsPerSecond", "SIGNALS.md §2.4u"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("failing alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	alerts = runClientLocationReliabilityRetentionFixture(t, clientLocationReliabilityRetentionHealthyFixture().with(clientLocationReliabilityRetentionFixture{
		"error_runs": 2, "progress_runs": 0,
	}))
	if len(alerts) != 0 {
		t.Fatalf("errors under the threshold raised %v", clientLocationReliabilityRetentionAlertClasses(alerts))
	}
}

// Each unobservable reason makes the read unknown, and a positive its own
// parts prove still raises beside it.
func TestClientLocationReliabilityRetentionUnobservable(t *testing.T) {
	for name, payload := range map[string]string{
		"missing":   clientLocationReliabilityRetentionFixtureJSON(t, clientLocationReliabilityRetentionHealthyFixture().with(nil, "error_runs"), 0),
		"stale":     clientLocationReliabilityRetentionFixtureJSON(t, clientLocationReliabilityRetentionHealthyFixture(), time.Hour),
		"unknown":   clientLocationReliabilityRetentionFixtureJSON(t, clientLocationReliabilityRetentionHealthyFixture().with(clientLocationReliabilityRetentionFixture{"surprise": 1}), 0),
		"duplicate": strings.Replace(clientLocationReliabilityRetentionFixtureJSON(t, clientLocationReliabilityRetentionFixture{"runs": 480, "error_runs": 0, "progress_runs": 480, "progress_runs_copy": 480}, 0), "progress_runs_copy", "progress_runs", 1),
		"negative":  clientLocationReliabilityRetentionFixtureJSON(t, clientLocationReliabilityRetentionHealthyFixture().with(clientLocationReliabilityRetentionFixture{"runs": -1}), 0),
	} {
		alerts := runClientLocationReliabilityRetentionPayload(t, payload)
		if classes := clientLocationReliabilityRetentionAlertClasses(alerts); len(classes) != 1 || !classes["location-reliability-retention-unobservable"] {
			t.Fatalf("%s raised %v", name, classes)
		}
	}
	alerts := runClientLocationReliabilityRetentionFixture(t, clientLocationReliabilityRetentionHealthyFixture().with(clientLocationReliabilityRetentionFixture{"runs": 0}, "error_runs"))
	if classes := clientLocationReliabilityRetentionAlertClasses(alerts); len(classes) != 2 || !classes["location-reliability-retention-unobservable"] || !classes["location-reliability-retention-stalled"] {
		t.Fatalf("a partial read with no runs raised %v", classes)
	}
}
