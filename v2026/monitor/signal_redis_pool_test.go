package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// One service's pool rates: the ten-minute floors and current values of the
// fleet-wide five-minute rates, and per-host current rates. A negative value
// omits that series.
type redisPoolFixtureService struct {
	service       string
	timeoutsFloor float64
	timeoutsNow   float64
	missesFloor   float64
	missesNow     float64
	hosts         map[string][2]float64 // host -> {timeouts, misses}
}

func redisPoolFixtureJSON(t testing.TB, now time.Time, services []redisPoolFixtureService) string {
	t.Helper()
	result := []map[string]any{}
	sample := func(labels map[string]string, value float64) {
		text := fmt.Sprintf("%g", value)
		if math.IsNaN(value) {
			text = "NaN"
		}
		result = append(result, map[string]any{
			"metric": labels,
			"value":  []any{float64(now.Unix()), text},
		})
	}
	for _, service := range services {
		for class, value := range map[string]float64{
			"timeouts_floor": service.timeoutsFloor,
			"timeouts_now":   service.timeoutsNow,
			"misses_floor":   service.missesFloor,
			"misses_now":     service.missesNow,
		} {
			if value < 0 {
				continue
			}
			sample(map[string]string{"service": service.service, "monitor_metric": class}, value)
		}
		for host, values := range service.hosts {
			sample(map[string]string{"service": service.service, "host": host, "monitor_metric": "host_timeouts"}, values[0])
			sample(map[string]string{"service": service.service, "host": host, "monitor_metric": "host_misses"}, values[1])
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

var redisPoolFixtureNow = time.Date(2026, 10, 25, 15, 20, 0, 0, time.UTC)

func runRedisPoolFixtureOutput(t *testing.T, payload string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			"urnetwork_redis_pool_timeouts_total",
			"urnetwork_redis_pool_misses_total",
			"%22synthetic%22",
			"%5B5m%5D",
			"%5B10m%3A1m%5D",
			"min_over_time",
			"monitor_metric",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("pool query missing %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return redisPoolFixtureNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := NewRedisPoolSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func runRedisPoolFixture(t *testing.T, services []redisPoolFixtureService) Alerts {
	t.Helper()
	return runRedisPoolFixtureOutput(t, redisPoolFixtureJSON(t, redisPoolFixtureNow, services))
}

// The pre-incident week's medians: every service at zero timeouts and a few
// misses a second, connect a little busier.
func redisPoolHealthyFleet() []redisPoolFixtureService {
	return []redisPoolFixtureService{
		{service: "api", missesFloor: 2.4, missesNow: 3},
		{service: "connect", missesFloor: 15.5, missesNow: 20},
		{service: "proxy", missesFloor: 2.4, missesNow: 4},
		{service: "taskworker", missesFloor: 5.9, missesNow: 7},
	}
}

func TestRedisPoolSignalHealthyFleetIsQuiet(t *testing.T) {
	if alerts := runRedisPoolFixture(t, redisPoolHealthyFleet()); len(alerts) != 0 {
		t.Fatalf("healthy pools alerted: %+v", alerts)
	}
}

// 2026-10-25 15:20Z: the proxies' pools timed out about 8,000 times a second
// and dialed about 14,000 times a second, sustained. That pages.
func TestRedisPoolSignalPagesOnSustainedTimeoutStorm(t *testing.T) {
	alerts := runRedisPoolFixture(t, []redisPoolFixtureService{
		{service: "api", missesFloor: 2.4, missesNow: 3},
		{service: "connect", missesFloor: 15.5, missesNow: 20},
		{service: "taskworker", missesFloor: 5.9, missesNow: 7},
		{
			service: "proxy", timeoutsFloor: 8139, timeoutsNow: 8840, missesFloor: 13727, missesNow: 13727,
			hosts: map[string][2]float64{"crisp": {5100, 8800}, "fireside": {3700, 4900}},
		},
	})
	if len(alerts) != 1 {
		t.Fatalf("want one critical alert, got %+v", alerts)
	}
	alert := requireAlertClass(t, alerts, "redis-pool-distress-critical")
	if alert.Severity != SeverityPage || alert.Target != "synthetic/proxy" || alert.Sustain != 1 || alert.SignalNumber != "3.5b" || alert.SignalID != "mimir/redis-pool" {
		t.Fatalf("critical pool alert contract changed: %+v", alert)
	}
	for _, want := range []string{"timeouts_floor_per_second=8139", "misses_floor_per_second=13727", "crisp", "fireside"} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("observed values lack %q: %s", want, alert.Observed)
		}
	}
}

// 2026-10-22 05:00Z: Connect's pools timed out about 190 times a second while
// its edges were CPU-saturated. That warns; it does not page.
func TestRedisPoolSignalWarnsOnSustainedTimeouts(t *testing.T) {
	alerts := runRedisPoolFixture(t, []redisPoolFixtureService{
		{service: "api", missesFloor: 5, missesNow: 6},
		{service: "connect", timeoutsFloor: 192, timeoutsNow: 396, missesFloor: 890, missesNow: 1137},
	})
	if len(alerts) != 1 {
		t.Fatalf("want one warning, got %+v", alerts)
	}
	alert := requireAlertClass(t, alerts, "redis-pool-distress")
	if alert.Severity != SeverityWarn || alert.Target != "synthetic/connect" {
		t.Fatalf("warning contract changed: %+v", alert)
	}
}

// Sustained dial churn without timeouts warns, and pages at storm rates.
func TestRedisPoolSignalMissesAloneWarnAndPage(t *testing.T) {
	warn := runRedisPoolFixture(t, []redisPoolFixtureService{{service: "api", missesFloor: 1500, missesNow: 1700}})
	if alert := requireAlertClass(t, warn, "redis-pool-distress"); alert.Target != "synthetic/api" || len(warn) != 1 {
		t.Fatalf("sustained misses did not warn: %+v", warn)
	}
	page := runRedisPoolFixture(t, []redisPoolFixtureService{{service: "proxy", missesFloor: 23000, missesNow: 31178}})
	if alert := requireAlertClass(t, page, "redis-pool-distress-critical"); alert.Severity != SeverityPage || len(page) != 1 {
		t.Fatalf("storm misses did not page: %+v", page)
	}
}

// A deploy's pools fill in a burst: a high current rate with a low ten-minute
// floor is not distress.
func TestRedisPoolSignalBriefBurstIsHealthy(t *testing.T) {
	alerts := runRedisPoolFixture(t, []redisPoolFixtureService{
		{service: "api", timeoutsFloor: 0, timeoutsNow: 1384, missesFloor: 34, missesNow: 7763},
	})
	if len(alerts) != 0 {
		t.Fatalf("a brief burst alerted: %+v", alerts)
	}
}

// The thresholds sit between the healthy week and both incidents.
func TestRedisPoolSignalThresholdsSeparateHealthyFromIncidents(t *testing.T) {
	if !(0 < redisPoolTimeoutsWarnPerSecond && redisPoolTimeoutsWarnPerSecond < redisPoolTimeoutsPagePerSecond) ||
		!(15.5 < redisPoolMissesWarnPerSecond && redisPoolMissesWarnPerSecond < redisPoolMissesPagePerSecond) {
		t.Fatal("pool thresholds are not ordered above the healthy medians")
	}
	if redisPoolTimeoutsPagePerSecond > 1780 || redisPoolTimeoutsWarnPerSecond > 192 {
		t.Fatal("pool thresholds no longer catch the 2026-10-25 and 2026-10-22 floors")
	}
}

// Missing, partial or invalid evidence is unknown, never a measured zero,
// while independently proven distress still reports.
func TestRedisPoolSignalIncompleteEvidenceIsUnknown(t *testing.T) {
	empty := runRedisPoolFixture(t, nil)
	unknown := requireAlertClass(t, empty, "redis-pool-unobservable")
	if unknown.Target != "synthetic/redis-pool" || !strings.Contains(unknown.Observed, "no_pool_series") {
		t.Fatalf("empty response not reported as unknown: %+v", unknown)
	}

	partial := runRedisPoolFixture(t, []redisPoolFixtureService{
		{service: "api", timeoutsFloor: -1, timeoutsNow: 0, missesFloor: 2, missesNow: 3},
		{service: "proxy", timeoutsFloor: 8139, timeoutsNow: 8840, missesFloor: 13727, missesNow: 13727},
	})
	if alert := requireAlertClass(t, partial, "redis-pool-unobservable"); !strings.Contains(alert.Observed, "incomplete_service_metrics") || !strings.Contains(alert.Observed, "api") {
		t.Fatalf("partial service not reported: %+v", alert)
	}
	requireAlertClass(t, partial, "redis-pool-distress-critical")

	invalid := runRedisPoolFixture(t, []redisPoolFixtureService{{service: "api", timeoutsFloor: math.NaN(), timeoutsNow: 0, missesFloor: 2, missesNow: 3}})
	if alert := requireAlertClass(t, invalid, "redis-pool-unobservable"); !strings.Contains(alert.Observed, "invalid_or_stale_metric") {
		t.Fatalf("invalid value not reported: %+v", invalid)
	}

	stale := redisPoolFixtureJSON(t, redisPoolFixtureNow.Add(-20*time.Minute), redisPoolHealthyFleet())
	if alert := requireAlertClass(t, runRedisPoolFixtureOutput(t, stale), "redis-pool-unobservable"); !strings.Contains(alert.Observed, "invalid_or_stale_metric") {
		t.Fatalf("stale samples not reported: %+v", alert)
	}
}

// A refused or malformed Mimir response fails the probe, not a quiet run.
func TestRedisPoolSignalRejectsUnsuccessfulResponses(t *testing.T) {
	for name, payload := range map[string]string{
		"error":  `{"status":"error","error":"synthetic"}`,
		"matrix": `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		"text":   `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }}
			settings := syntheticSettings(source)
			settings.Now = func() time.Time { return redisPoolFixtureNow }
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
			if _, err := NewRedisPoolSignal().Run(context.Background(), settings); err == nil {
				t.Fatal("unsuccessful response did not fail the probe")
			}
		})
	}
}

// Overlay this test alone to reproduce the absent probe at its registry
// boundary.
func TestRedisPoolSignalIsRegistered(t *testing.T) {
	matches := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "redis-pool" {
			continue
		}
		matches++
		if signal.Number() != "3.5b" || signal.ID() != "mimir/redis-pool" || signal.Cadence() != 5*time.Minute {
			t.Fatalf("registered pool signal does not match its catalog contract: %s %s %s", signal.Number(), signal.ID(), signal.Cadence())
		}
	}
	if matches != 1 {
		t.Fatal("missing unique registered SIGNALS.md section 3.5b pool signal")
	}
}
