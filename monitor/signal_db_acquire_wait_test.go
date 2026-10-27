package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Incident A replayed from recorded Mimir history: the proxy's mean acquire
// wait passes 100 ms at 20:04Z on 10-25 (92.6 ms at 20:03Z), so it warns at
// 20:08Z and not a minute earlier; connect at 35-49 ms and the healthy
// reference of 10-20 raise nothing.
func TestDbAcquireWaitReplaysIncidentA(t *testing.T) {
	cases := loadStormReplayFixture(t, "db-acquire-wait")
	for _, test := range []struct {
		name    string
		targets []string
	}{
		{name: "incident-a-proxy", targets: []string{"main/proxy"}},
		{name: "incident-a-before-proxy"},
		{name: "healthy"},
	} {
		alerts := runStormReplay(t, NewDbAcquireWaitSignal(), cases[test.name], nil)
		want := map[string][]string{}
		if 0 < len(test.targets) {
			want["db-acquire-wait"] = test.targets
		}
		requireStormAlerts(t, test.name, alerts, want)
	}
	alert := requireAlertClass(t, runStormReplay(t, NewDbAcquireWaitSignal(), cases["incident-a-proxy"], nil), "db-acquire-wait")
	if alert.Severity != SeverityWarn || alert.SignalNumber != "26.3" || !strings.Contains(alert.Observed, "mean_ms_now=205.5") || !strings.Contains(alert.Observed, "canceled_per_second=26.2") {
		t.Fatalf("%+v", alert)
	}
}

// A synthetic answer for the services given: mean, acquired and canceled.
func dbAcquireWaitResponse(t *testing.T, means map[string][]float64, acquired map[string][]float64) string {
	series := []stormSyntheticSeries{}
	for service, values := range means {
		series = append(series, stormSyntheticSeries{part: "mean", labels: map[string]string{"service": service}, values: values})
	}
	for service, values := range acquired {
		series = append(series, stormSyntheticSeries{part: "acquired", labels: map[string]string{"service": service}, values: values})
		series = append(series, stormSyntheticSeries{part: "canceled", labels: map[string]string{"service": service}, values: stormRepeat(0, len(values))})
	}
	return stormSyntheticResponse(t, stormSyntheticStart(5), time.Minute, series)
}

// Every placed service is required; zero acquisitions read as no wait; the
// level is strict.
func TestDbAcquireWaitServicesAndBands(t *testing.T) {
	probe := dbAcquireWaitProbe{}
	services := []string{"alt", "api", "connect", "gossip", "mcp", "proxy", "taskworker"}
	everyService := func(mean float64) (map[string][]float64, map[string][]float64) {
		means, acquired := map[string][]float64{}, map[string][]float64{}
		for _, service := range services {
			means[service] = stormRepeat(mean, 5)
			acquired[service] = stormRepeat(100, 5)
		}
		return means, acquired
	}
	proxy, gossip := "db-acquire-wait main/proxy", "db-acquire-wait main/gossip"
	visibility := "db-acquire-wait-unobservable main/db-acquire-wait"

	healthyMeans, healthyAcquired := everyService(0.004)
	atLevelMeans, atLevelAcquired := everyService(0.004)
	atLevelMeans["proxy"] = stormRepeat(0.1, 5)
	slowMeans, slowAcquired := everyService(0.004)
	slowMeans["proxy"] = stormRepeat(0.23, 5)
	idleMeans, idleAcquired := everyService(0.004)
	idleMeans["gossip"] = stormRepeat(math.NaN(), 5)
	idleAcquired["gossip"] = stormRepeat(0, 5)
	silentMeans, silentAcquired := everyService(0.004)
	delete(silentMeans, "gossip")
	delete(silentAcquired, "gossip")
	for _, test := range []struct {
		name     string
		means    map[string][]float64
		acquired map[string][]float64
		want     map[string]string
	}{
		{name: "healthy", means: healthyMeans, acquired: healthyAcquired, want: map[string]string{proxy: "healthy", visibility: "healthy"}},
		{name: "at the level", means: atLevelMeans, acquired: atLevelAcquired, want: map[string]string{proxy: "healthy"}},
		{name: "above the level", means: slowMeans, acquired: slowAcquired, want: map[string]string{proxy: "warn", gossip: "healthy"}},
		{name: "no acquisitions is no wait", means: idleMeans, acquired: idleAcquired, want: map[string]string{gossip: "healthy", visibility: "healthy"}},
		{name: "a placed service that stops publishing", means: silentMeans, acquired: silentAcquired, want: map[string]string{gossip: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, dbAcquireWaitResponse(t, test.means, test.acquired), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestDbAcquireWaitQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultDbAcquireWaitSettings()
	if settings.WarnMean != 100*time.Millisecond || settings.Hold != 5*time.Minute || settings.Pool != "default" || settings.Window != 5*time.Minute {
		t.Fatalf("§26.3 policy changed: %+v", settings)
	}
	want := `sum by (service) (rate(urnetwork_pg_pool_acquire_duration_seconds_total{env="main",pool="default"}[5m])) / sum by (service) (rate(urnetwork_pg_pool_acquires_total{env="main",pool="default",outcome="acquired"}[5m]))`
	if !strings.Contains(dbAcquireWaitQuery("main", settings), want) {
		t.Fatalf("query lost the catalog expression:\n%s", dbAcquireWaitQuery("main", settings))
	}
}
