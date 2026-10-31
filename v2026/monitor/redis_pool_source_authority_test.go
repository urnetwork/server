// Independent coverage and lifecycle controls for the existing calibrated
// pool signal; no socket, native helper, database or backend query runs here.
package monitor

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fixed desired placement preserves the existing proxy host split assertion.
func redisPoolFixtureHosts(service string) []string {
	if service == "proxy" {
		return []string{"crisp", "fireside"}
	}
	return []string{"fixture-" + service}
}

// A complete synthetic fifteen-minute source span with eleven discrete hold
// points. It does not assert independent pool-generation or recovery authority.
func redisPoolFixtureProof(at time.Time) map[string]float64 {
	partValues := map[string]float64{"source/start": float64(at.Add(-time.Hour).Unix()), "source/prior_start": float64(at.Add(-time.Hour).Unix()),
		"source/start_clock": float64(at.Add(-15 * time.Second).Unix()), "source/prior_start_clock": float64(at.Add(-15*time.Minute - 15*time.Second).Unix())}
	for _, counter := range []string{"timeouts", "misses"} {
		prefix := "source/" + counter + "/"
		partValues[prefix+"points"] = 11
		partValues[prefix+"clock_points"] = 11
		partValues[prefix+"max_age"] = 15
		partValues[prefix+"min_age"] = 15
		partValues[prefix+"resets"] = 0
		partValues[prefix+"prior_clock"] = float64(at.Add(-15*time.Minute - 15*time.Second).Unix())
	}
	return partValues
}

// Safely mutates fixed proof fields in a private synthetic vector; no raw
// production response is accepted or rendered by these test helpers.
func redisPoolMutateProof(t testing.TB, payload string, mutate func(map[string]any)) string {
	t.Helper()
	var envelope map[string]any
	if json.Unmarshal([]byte(payload), &envelope) != nil {
		t.Fatal("synthetic vector invalid")
	}
	data := envelope["data"].(map[string]any)
	for _, row := range data["result"].([]any) {
		mutate(row.(map[string]any))
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal("synthetic vector encoding failed")
	}
	return string(encoded)
}

// The fake backend returns only projection parts requested by the actual query.
// This permits an old-source counterfactual without feeding its legacy parser
// unsupported proof rows or changing any assertion by source version.
func redisPoolFixtureResponseForQuery(t testing.TB, payload, command string) string {
	t.Helper()
	if strings.Contains(command, url.QueryEscape("source/expected")) {
		return payload
	}
	var envelope map[string]any
	if json.Unmarshal([]byte(payload), &envelope) != nil {
		return payload
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		return payload
	}
	sourceRows, ok := data["result"].([]any)
	if !ok {
		return payload
	}
	resultRows := []any{}
	for _, row := range sourceRows {
		value, ok := row.(map[string]any)
		if !ok {
			resultRows = append(resultRows, row)
			continue
		}
		labels, ok := value["metric"].(map[string]any)
		if !ok {
			resultRows = append(resultRows, row)
			continue
		}
		part, _ := labels["monitor_metric"].(string)
		if !strings.HasPrefix(part, "source/") {
			resultRows = append(resultRows, row)
		}
	}
	data["result"] = resultRows
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal("synthetic query projection failed")
	}
	return string(encoded)
}

// A whole expected service missing from a nonempty response is unknown, even
// though every returned service has complete healthy-looking rate rows.
func TestRedisPoolMissingDesiredServiceRemainsUnknown(t *testing.T) {
	payload := redisPoolFixtureJSON(t, redisPoolFixtureNow, []redisPoolFixtureService{{service: "api", missesFloor: 2, missesNow: 3}})
	alerts := runRedisPoolFixtureOutputForServices(t, payload, []string{"api", "proxy"})
	alert := requireAlertClass(t, alerts, "redis-pool-unobservable")
	if !alert.unobservable || !strings.Contains(alert.Observed, "source_coverage_incomplete") {
		t.Fatal("missing desired service appeared complete")
	}
}

// Fresh derived evaluation timestamps cannot conceal stale original source
// clocks, a sparse floor, changed process generation or a visible pool reset.
func TestRedisPoolOriginalSourceAndHoldAuthorityRequired(t *testing.T) {
	for _, test := range []struct {
		name, part string
		value      float64
	}{
		{name: "stale-original", part: "source/timeouts/max_age", value: 900},
		{name: "sparse-rate-hold", part: "source/timeouts/points", value: 1},
		{name: "sparse-clock-hold", part: "source/misses/clock_points", value: 1},
		{name: "counter-reset", part: "source/timeouts/resets", value: 1},
		{name: "restarted-process", part: "source/prior_start", value: 1},
	} {
		payload := redisPoolFixtureJSON(t, redisPoolFixtureNow, []redisPoolFixtureService{{service: "api", timeoutsFloor: 2000, timeoutsNow: 2000, missesFloor: 1, missesNow: 1}})
		payload = redisPoolMutateProof(t, payload, func(row map[string]any) {
			labels := row["metric"].(map[string]any)
			if labels["monitor_metric"] == test.part {
				row["value"] = []any{redisPoolFixtureNow.Unix(), strconv.FormatFloat(test.value, 'f', -1, 64)}
			}
		})
		alerts := runRedisPoolFixtureOutputForServices(t, payload, []string{"api"})
		requireAlertClass(t, alerts, "redis-pool-unobservable")
		for _, alert := range alerts {
			if alert.Class == "redis-pool-distress-critical" || alert.Class == "redis-pool-distress" {
				t.Fatalf("case %s: unqualified sustained pressure accepted", test.name)
			}
		}
	}
}

// Source failure on one desired service cannot erase an independently
// qualified service's existing critical threshold crossing.
func TestRedisPoolQualifiedPressureSurvivesMissingSibling(t *testing.T) {
	payload := redisPoolFixtureJSON(t, redisPoolFixtureNow, []redisPoolFixtureService{{service: "proxy", timeoutsFloor: 2000, timeoutsNow: 2000, missesFloor: 1, missesNow: 1}})
	alerts := runRedisPoolFixtureOutputForServices(t, payload, []string{"api", "proxy"})
	if requireAlertClass(t, alerts, "redis-pool-distress-critical").unobservable {
		t.Fatal("qualified pressure misclassified")
	}
	requireAlertClass(t, alerts, "redis-pool-unobservable")
}

// Flat and masked-reset-looking totals supply no interval-health authority.
// The production hold identities preserve both pressure bands in the notifier.
func TestRedisPoolBelowBandCannotClearUnknownPoolLifetimes(t *testing.T) {
	notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
	alert := Alert{SignalID: "mimir/redis-pool", Class: "redis-pool-distress-critical", Target: "synthetic/api", Environment: "synthetic", Severity: SeverityPage}
	now := redisPoolFixtureNow
	notifier.observeWithLock(now, "redis-pool", SignalExecution{Alerts: Alerts{alert}})
	calls := 0
	payload := redisPoolFixtureJSON(t, now, []redisPoolFixtureService{{service: "api", missesFloor: 2, missesNow: 3}})
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		calls++
		if host.Name != "metrics-1" {
			t.Fatal("owning query gateway changed")
		}
		return redisPoolFixtureResponseForQuery(t, payload, command), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}}, HostSettings{Name: "fixture-api", Roles: []string{"edge"}})
	settings.LogServices = []string{"api"}
	settings.LogServiceHosts = map[string][]string{"api": {"fixture-api"}}
	settings.LogServiceBlocks = map[string][]string{"api": {"g1"}}
	if err := settings.Validate(); err != nil {
		t.Fatal("synthetic source inventory invalid")
	}
	for range 3 {
		observation := &signalClearHoldObservation{}
		ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observation)
		alerts, err := NewRedisPoolSignal().Run(ctx, settings)
		if err != nil || len(alerts) != 0 {
			t.Fatal("complete below-band accounting created an incident")
		}
		holds := observation.snapshot(false)
		if len(holds) != 2 {
			t.Fatal("public source observation omitted unknown-lifetime pressure holds")
		}
		if transitions := notifier.observeWithLock(now, "redis-pool", SignalExecution{ClearHolds: holds}); len(transitions) != 0 {
			t.Fatal("unknown pool lifetimes cleared pressure")
		}
	}
	if calls != 3 {
		t.Fatal("observation replayed or omitted a scheduled source request")
	}
	if _, present := notifier.keyIncidents[slackIncidentKey(alert)]; !present {
		t.Fatal("critical incident disappeared")
	}
}

// The actual injected request carries one exact evaluation clock and stays
// one scheduled read. This control is separate from closure-unknown checks.
func TestRedisPoolQueryBindsOneEvaluationClock(t *testing.T) {
	now := redisPoolFixtureNow.Add(789 * time.Millisecond)
	calls := 0
	payload := redisPoolFixtureJSON(t, redisPoolFixtureNow, []redisPoolFixtureService{{service: "api", missesFloor: 2, missesNow: 3}})
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		calls++
		if host.Name != "metrics-1" || !strings.Contains(command, "&time="+strconv.FormatInt(now.Unix(), 10)) {
			t.Fatal("exact actual query clock missing")
		}
		return redisPoolFixtureResponseForQuery(t, payload, command), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}}, HostSettings{Name: "fixture-api", Roles: []string{"edge"}})
	settings.LogServices = []string{"api"}
	settings.LogServiceHosts = map[string][]string{"api": {"fixture-api"}}
	settings.LogServiceBlocks = map[string][]string{"api": {"g1"}}
	if err := settings.Validate(); err != nil {
		t.Fatal("synthetic source inventory invalid")
	}
	alerts, err := NewRedisPoolSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || calls != 1 {
		t.Fatal("single bounded evaluation changed")
	}
}

// Complete proof rows do not qualify an omitted derived service. A healthy
// sibling cannot clear source visibility; a pressured sibling still reports.
func TestRedisPoolProofWithoutDerivedRatesRemainsUnknown(t *testing.T) {
	for _, test := range []struct {
		name             string
		siblingFloor     float64
		expectedPressure int
	}{
		{name: "below-band-sibling", siblingFloor: 0, expectedPressure: 0},
		{name: "critical-sibling", siblingFloor: 2000, expectedPressure: 1},
	} {
		payload := redisPoolFixtureJSON(t, redisPoolFixtureNow, []redisPoolFixtureService{
			{service: "api", timeoutsFloor: -1, timeoutsNow: -1, missesFloor: -1, missesNow: -1},
			{service: "proxy", timeoutsFloor: test.siblingFloor, timeoutsNow: test.siblingFloor, missesFloor: 1, missesNow: 1},
		})
		alerts := runRedisPoolFixtureOutputForServices(t, payload, []string{"api", "proxy"})
		unknown := requireAlertClass(t, alerts, "redis-pool-unobservable")
		if !unknown.unobservable || !strings.Contains(unknown.Observed, "source_coverage_incomplete") {
			t.Fatalf("case %s: proof-only service appeared observable", test.name)
		}
		pressure := 0
		for _, alert := range alerts {
			if alert.Class == "redis-pool-distress-critical" {
				pressure++
				if alert.Target != "synthetic/proxy" || alert.unobservable {
					t.Fatalf("case %s: independent sibling pressure changed", test.name)
				}
			}
		}
		if pressure != test.expectedPressure {
			t.Fatalf("case %s: independent sibling pressure lost or manufactured", test.name)
		}
	}
}
