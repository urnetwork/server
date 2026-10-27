package monitor

import (
	"strings"
	"testing"
	"time"
)

// Incident B replayed from recorded Mimir history: the proxy's pool timeouts
// pass 10/s at 14:48Z on 10-26 (14.7/s) and warn at 14:52Z, about eight
// minutes before §3.5b's 50/s floor; the healthy reference raises nothing.
func TestRedisPoolTimeoutsReplaysIncidentB(t *testing.T) {
	cases := loadStormReplayFixture(t, "redis-pool-timeouts")
	for _, test := range []struct {
		name    string
		targets []string
	}{
		{name: "incident-b", targets: []string{"main/proxy"}},
		{name: "incident-b-before"},
		{name: "healthy"},
	} {
		alerts := runStormReplay(t, NewRedisPoolTimeoutsSignal(), cases[test.name], nil)
		want := map[string][]string{}
		if 0 < len(test.targets) {
			want["redis-pool-timeouts"] = test.targets
		}
		requireStormAlerts(t, test.name, alerts, want)
	}
	alert := requireAlertClass(t, runStormReplay(t, NewRedisPoolTimeoutsSignal(), cases["incident-b"], nil), "redis-pool-timeouts")
	if alert.Severity != SeverityWarn || alert.SignalNumber != "26.7" || !strings.Contains(alert.Observed, "timeouts_per_second_now=224.8") || !strings.Contains(alert.Observed, "misses_per_second_now=3141.7") {
		t.Fatalf("%+v", alert)
	}
}

func redisPoolTimeoutsResponse(t *testing.T, timeouts map[string][]float64) string {
	series := []stormSyntheticSeries{}
	for service, values := range timeouts {
		series = append(series,
			stormSyntheticSeries{part: "timeouts", labels: map[string]string{"service": service}, values: values},
			stormSyntheticSeries{part: "misses", labels: map[string]string{"service": service}, values: stormRepeat(4, len(values))},
		)
	}
	return stormSyntheticResponse(t, stormSyntheticStart(5), time.Minute, series)
}

// The band is strict and needs all five minutes; a placed service that stops
// publishing its registered counters is unknown.
func TestRedisPoolTimeoutsBandsAndServices(t *testing.T) {
	probe := redisPoolTimeoutsProbe{}
	quiet := func() map[string][]float64 {
		timeouts := map[string][]float64{}
		for _, service := range []string{"alt", "api", "connect", "gossip", "mcp", "proxy", "taskworker"} {
			timeouts[service] = stormRepeat(0, 5)
		}
		return timeouts
	}
	proxy, connect := "redis-pool-timeouts main/proxy", "redis-pool-timeouts main/connect"
	visibility := "redis-pool-timeouts-unobservable main/redis-pool-timeouts"
	atLevel, above, brief, silent := quiet(), quiet(), quiet(), quiet()
	atLevel["proxy"] = stormRepeat(10, 5)
	above["proxy"] = stormRepeat(14.7, 5)
	brief["proxy"] = []float64{6, 14.7, 19, 41, 186}
	delete(silent, "connect")
	for _, test := range []struct {
		name     string
		timeouts map[string][]float64
		want     map[string]string
	}{
		{name: "healthy", timeouts: quiet(), want: map[string]string{proxy: "healthy", connect: "healthy", visibility: "healthy"}},
		{name: "at the level", timeouts: atLevel, want: map[string]string{proxy: "healthy"}},
		{name: "above the level", timeouts: above, want: map[string]string{proxy: "warn", connect: "healthy"}},
		{name: "four minutes", timeouts: brief, want: map[string]string{proxy: "hold"}},
		{name: "a placed service that stops publishing", timeouts: silent, want: map[string]string{connect: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, redisPoolTimeoutsResponse(t, test.timeouts), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestRedisPoolTimeoutsQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultRedisPoolTimeoutsSettings()
	if settings.WarnTimeouts != 10 || settings.Hold != 5*time.Minute || settings.Window != 5*time.Minute {
		t.Fatalf("§26.7 policy changed: %+v", settings)
	}
	want := `sum by (service) (rate(urnetwork_redis_pool_timeouts_total{env="main"}[5m]))`
	if !strings.Contains(redisPoolTimeoutsQuery("main", settings), want) {
		t.Fatalf("query lost the catalog expression:\n%s", redisPoolTimeoutsQuery("main", settings))
	}
}
