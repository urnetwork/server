package monitor

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Incident B replayed from recorded Mimir history: node 6394's HELLO rate
// passes 500/s at 14:48Z on 10-26 and churn warns at 14:52Z, while its main
// thread (above 0.85 from 14:49Z) has held only four evaluations; the main
// thread warns at 14:58Z. The healthy reference raises nothing across all 48
// nodes.
func TestRedisMainThreadReplaysIncidentB(t *testing.T) {
	cases := loadStormReplayFixture(t, "redis-main-thread")
	node := "by-us-fmt-5-edge-6:6394"
	for _, test := range []struct {
		name string
		want map[string][]string
	}{
		{name: "incident-b-hello", want: map[string][]string{"redis-connection-churn": {node}}},
		{name: "incident-b-main-thread", want: map[string][]string{"redis-connection-churn": {node}, "redis-main-thread-saturated": {node}}},
		{name: "healthy", want: map[string][]string{}},
	} {
		alerts := runStormReplay(t, NewRedisMainThreadSignal(), cases[test.name], nil)
		requireStormAlerts(t, test.name, alerts, test.want)
		for _, alert := range alerts {
			if alert.Severity != SeverityWarn || alert.SignalNumber != "26.8" || alert.unobservable {
				t.Fatalf("%s: %+v", test.name, alert)
			}
		}
	}
	saturated := requireAlertClass(t, runStormReplay(t, NewRedisMainThreadSignal(), cases["incident-b-main-thread"], nil), "redis-main-thread-saturated")
	if !strings.Contains(saturated.Observed, "main_thread_cores_now=0.925") || !strings.Contains(saturated.Observed, "hello_per_second_now=3504.0") {
		t.Fatalf("observed %q", saturated.Observed)
	}
}

// A synthetic answer for every configured node: main-thread cores and HELLO
// rate per step; overrides replace one node's values, and a nil part leaves
// it out.
func redisMainThreadResponse(t *testing.T, overrides map[string][2][]float64) string {
	series := []stormSyntheticSeries{}
	for _, node := range append(stormPortRange(6380, 6411), stormPortRange(6412, 6427)...) {
		host := "by-us-fmt-5-edge-6"
		if 6412 <= node {
			host = "by-us-fmt-5-edge-5"
		}
		port := strconv.Itoa(node)
		values := [2][]float64{stormRepeat(0.4, 10), stormRepeat(3, 10)}
		if override, present := overrides[port]; present {
			values = override
		}
		labels := map[string]string{"env": "main", "job": "redis", "host": host, "node": port}
		if values[0] != nil {
			series = append(series, stormSyntheticSeries{part: "main_thread", labels: labels, values: values[0]})
		}
		if values[1] != nil {
			series = append(series, stormSyntheticSeries{part: "hello", labels: map[string]string{"env": "main", "job": "redis", "host": host, "node": port, "cmd": "hello"}, values: values[1]})
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(10), time.Minute, series)
}

// Each node's two bands are strict and independent; a scraped node without a
// HELLO count reads zero; an unscraped configured node is unknown.
func TestRedisMainThreadBandsAndNodes(t *testing.T) {
	probe := redisMainThreadProbe{}
	saturated, churn := "redis-main-thread-saturated by-us-fmt-5-edge-6:6394", "redis-connection-churn by-us-fmt-5-edge-6:6394"
	visibility := "redis-main-thread-unobservable main/redis-main-thread"
	for _, test := range []struct {
		name      string
		overrides map[string][2][]float64
		want      map[string]string
	}{
		{name: "healthy", want: map[string]string{saturated: "healthy", churn: "healthy", visibility: "healthy"}},
		{name: "main thread at its level", overrides: map[string][2][]float64{"6394": {stormRepeat(0.85, 10), stormRepeat(3, 10)}}, want: map[string]string{saturated: "healthy"}},
		{name: "main thread above", overrides: map[string][2][]float64{"6394": {stormRepeat(0.95, 10), stormRepeat(3, 10)}}, want: map[string]string{saturated: "warn", churn: "healthy"}},
		{name: "commands, not reconnects", overrides: map[string][2][]float64{"6421": {stormRepeat(0.947, 10), stormRepeat(40, 10)}}, want: map[string]string{"redis-main-thread-saturated by-us-fmt-5-edge-5:6421": "warn", "redis-connection-churn by-us-fmt-5-edge-5:6421": "healthy"}},
		{name: "churn at its level", overrides: map[string][2][]float64{"6394": {stormRepeat(0.4, 10), stormRepeat(500, 10)}}, want: map[string]string{churn: "healthy"}},
		{name: "churn above for five minutes", overrides: map[string][2][]float64{"6394": {stormRepeat(0.4, 10), append(stormRepeat(3, 5), stormRepeat(733, 5)...)}}, want: map[string]string{churn: "warn", saturated: "healthy"}},
		{name: "no HELLO since start", overrides: map[string][2][]float64{"6394": {stormRepeat(0.4, 10), nil}}, want: map[string]string{churn: "healthy", visibility: "healthy"}},
		{name: "an unscraped node", overrides: map[string][2][]float64{"6394": {nil, nil}}, want: map[string]string{saturated: "hold", churn: "hold", visibility: "unobservable"}},
		{name: "a scrape gap", overrides: map[string][2][]float64{"6394": {append(stormRepeat(0.95, 9), math.NaN()), stormRepeat(3, 10)}}, want: map[string]string{saturated: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, redisMainThreadResponse(t, test.overrides), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestRedisMainThreadQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultRedisMainThreadSettings()
	if settings.MainThreadBusy != 0.85 || settings.MainThreadHold != 10*time.Minute || settings.HelloRate != 500 || settings.HelloHold != 5*time.Minute {
		t.Fatalf("§26.8 policy changed: %+v", settings)
	}
	query := redisMainThreadQuery("main", settings)
	for _, want := range []string{
		`rate(redis_cpu_user_main_thread_seconds_total{env="main"}[5m]) + rate(redis_cpu_sys_main_thread_seconds_total{env="main"}[5m])`,
		`rate(redis_commands_total{env="main",cmd="hello"}[5m])`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost the catalog expression %s", want)
		}
	}
}
