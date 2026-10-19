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

// The registered signal for the PgBouncer pool exporter. Before §2.11a had a
// probe nothing in the monitor read it, so the 2026-10-12 and 2026-10-19 pool
// livelocks paged no one.
func pgbouncerPoolStarvationSignalForTest(t *testing.T) Signal {
	t.Helper()
	for _, signal := range NewSignals() {
		if signal.Key() == "pgbouncer-pool-starvation" {
			return signal
		}
	}
	t.Fatal("no registered signal reads pgbouncer_pool_servers or pgbouncer_pool_clients_waiting; a PgBouncer pool livelock pages no one")
	return nil
}

// One synthetic PgBouncer instance as the exporter last read it. A nil pool
// gauge is absent from the Mimir vector.
type pgbouncerStarvationFixture struct {
	index          int
	up             float64
	sampleAge      time.Duration
	noPool         bool
	waiting        float64
	servers        float64
	logins         float64
	maxWait        *float64
	starvedSeconds float64
}

func pgbouncerStarvationSeconds(value float64) *float64 {
	return &value
}

// A healthy pool with its 20 servers.
func pgbouncerStarvationHealthy(index int) pgbouncerStarvationFixture {
	return pgbouncerStarvationFixture{index: index, up: 1, servers: 20, maxWait: pgbouncerStarvationSeconds(0)}
}

// A pool in the livelock: queued clients, no server, one login at a time,
// clients expiring at the 10 s queue deadline, starved for an hour.
func pgbouncerStarvationLivelocked(index int) pgbouncerStarvationFixture {
	return pgbouncerStarvationFixture{
		index: index, up: 1, waiting: 205, servers: 0, logins: 1,
		maxWait: pgbouncerStarvationSeconds(10), starvedSeconds: 3600,
	}
}

// A Mimir vector for the probe's query, evaluated at now.
func pgbouncerStarvationFixtureJson(t testing.TB, now time.Time, fixtures []pgbouncerStarvationFixture) string {
	t.Helper()
	results := []map[string]any{}
	sample := func(fixture pgbouncerStarvationFixture, metric string, database string, value float64) {
		labels := map[string]string{
			"env": "synthetic", "host": "db-1", "job": "pgbouncer",
			"pgbouncer": fmt.Sprintf("%d", fixture.index), "monitor_metric": metric,
		}
		if database != "" {
			labels["database"] = database
		}
		results = append(results, map[string]any{
			"metric": labels,
			"value":  []any{float64(now.Unix()), fmt.Sprintf("%g", value)},
		})
	}
	for _, fixture := range fixtures {
		sample(fixture, "up", "", fixture.up)
		sample(fixture, "sample_time", "", float64(now.Add(-fixture.sampleAge).Add(-12*time.Second).UnixMilli())/1000)
		if fixture.noPool || fixture.up != 1 {
			continue
		}
		sample(fixture, "waiting", "app", fixture.waiting)
		sample(fixture, "servers", "app", fixture.servers)
		sample(fixture, "logins", "app", fixture.logins)
		if fixture.maxWait != nil {
			sample(fixture, "max_wait", "app", *fixture.maxWait)
		}
		sample(fixture, "starved_seconds", "app", fixture.starvedSeconds)
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": results},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// Runs the signal over a Mimir payload, checking the query it sends.
func runPgBouncerStarvationPayload(t *testing.T, now time.Time, payload string) Alerts {
	t.Helper()
	signal := pgbouncerPoolStarvationSignalForTest(t)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" {
			return "", fmt.Errorf("pgbouncer pool starvation queried %s, not the services host", host.Name)
		}
		for _, want := range []string{
			"pgbouncer_up",
			"timestamp%28pgbouncer_up",
			"pgbouncer_pool_clients_waiting",
			"pgbouncer_pool_servers",
			"pgbouncer_pool_server_logins",
			"pgbouncer_pool_maxwait_seconds",
			"pgbouncer_pool_starved_seconds",
			"job%3D%22pgbouncer%22",
			"env%3D%22synthetic%22",
			"monitor_metric",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("pgbouncer pool starvation query missing %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func runPgBouncerStarvationFixture(t *testing.T, fixtures []pgbouncerStarvationFixture) Alerts {
	t.Helper()
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	return runPgBouncerStarvationPayload(t, now, pgbouncerStarvationFixtureJson(t, now, fixtures))
}

func requireNoPgBouncerStarvationAlerts(t *testing.T, alerts Alerts) {
	t.Helper()
	if len(alerts) != 0 {
		classes := []string{}
		for _, alert := range alerts {
			classes = append(classes, alert.Class)
		}
		t.Fatalf("unexpected alerts %v:\n%s", classes, alerts[0].Markdown())
	}
}

// 32 pools with their servers are quiet.
func TestPgBouncerPoolStarvationSignalHealthyFleetIsQuiet(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{}
	for index := 1; index <= 32; index++ {
		fixtures = append(fixtures, pgbouncerStarvationHealthy(index))
	}
	requireNoPgBouncerStarvationAlerts(t, runPgBouncerStarvationFixture(t, fixtures))
}

// The 2026-10-19 shape: pgbouncer@1 (the patched build) kept 20 servers while
// the other 31 pools livelocked. The healthy sibling does not clear the page.
func TestPgBouncerPoolStarvationSignalPagesWhenAHealthySiblingRemains(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1)}
	for index := 2; index <= 32; index++ {
		fixtures = append(fixtures, pgbouncerStarvationLivelocked(index))
	}
	alerts := runPgBouncerStarvationFixture(t, fixtures)
	alert := requireAlertClass(t, alerts, "pgbouncer-pools-starved")
	if alert.SignalNumber != "2.11a" || alert.SignalKey != "pgbouncer-pool-starvation" ||
		alert.Severity != SeverityPage || alert.Sustain != 1 || alert.Target != "synthetic/pgbouncer-pools" || alert.Frame != "" {
		t.Fatalf("wrong starvation page identity: %+v", alert)
	}
	for _, want := range []string{
		"31 of 32 read PgBouncer pools have queued clients and no server",
		"starved_pools=31 old_starved_pools=31",
		"clients_waiting=6355 usable_servers=20 logins=31 max_wait_s=10 max_starved_s=3600",
		"old_starved=db-1/pgbouncer@2,db-1/pgbouncer@3,db-1/pgbouncer@4,db-1/pgbouncer@5,db-1/pgbouncer@6,db-1/pgbouncer@7,db-1/pgbouncer@8,db-1/pgbouncer@9,and 23 more",
		"with_servers=db-1/pgbouncer@1",
		"metrics_gateway=metrics-1",
		"SET query_wait_timeout = 4",
		"Do not restart PostgreSQL",
		"SIGNALS.md §5.12 and §2.11a",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("starvation page missing %q:\n%s", want, alert.Markdown())
		}
	}
	if count := requireAlertClassCount(alerts, "pgbouncer-pool-starved-shard"); count != 0 {
		t.Fatalf("fleet starvation also warned per shard: %d", count)
	}
	if count := requireAlertClassCount(alerts, "pgbouncer-pools-unobservable"); count != 0 {
		t.Fatalf("a fully read fleet reported unknown pools: %d", count)
	}
}

// At the 09:57Z onset the pools had been starved for one exporter reading,
// but their clients had already waited past the 5 s deadline: that is old.
func TestPgBouncerPoolStarvationSignalOnsetPagesOnTheClientDeadline(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{}
	for index := 1; index <= 32; index++ {
		if index <= 8 {
			fixtures = append(fixtures, pgbouncerStarvationHealthy(index))
			continue
		}
		fixtures = append(fixtures, pgbouncerStarvationFixture{
			index: index, up: 1, waiting: 268, servers: 0, logins: 1,
			maxWait: pgbouncerStarvationSeconds(5), starvedSeconds: 15,
		})
	}
	alert := requireAlertClass(t, runPgBouncerStarvationFixture(t, fixtures), "pgbouncer-pools-starved")
	if !strings.Contains(alert.Markdown(), "24 of 32 read PgBouncer pools") {
		t.Fatalf("onset page has the wrong count:\n%s", alert.Markdown())
	}
}

// A pool is empty while its first login completes after a start or reload,
// and a young starvation below the deadline is not yet old: neither alerts.
func TestPgBouncerPoolStarvationSignalYoungStarvationDoesNotAlert(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{}
	for index := 1; index <= 32; index++ {
		fixtures = append(fixtures, pgbouncerStarvationFixture{
			index: index, up: 1, waiting: 3, servers: 0, logins: 1, maxWait: pgbouncerStarvationSeconds(0),
		})
	}
	fixtures[5] = pgbouncerStarvationFixture{
		index: 6, up: 1, waiting: 40, servers: 0, logins: 1, maxWait: pgbouncerStarvationSeconds(4.9), starvedSeconds: 29,
	}
	requireNoPgBouncerStarvationAlerts(t, runPgBouncerStarvationFixture(t, fixtures))
}

// Queued clients behind busy servers are §2.11's busy-server pressure, not
// starvation.
func TestPgBouncerPoolStarvationSignalBusyServersAreNotStarvation(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{}
	for index := 1; index <= 32; index++ {
		fixtures = append(fixtures, pgbouncerStarvationFixture{
			index: index, up: 1, waiting: 60, servers: 20, maxWait: pgbouncerStarvationSeconds(8),
		})
	}
	requireNoPgBouncerStarvationAlerts(t, runPgBouncerStarvationFixture(t, fixtures))
}

// One old starved pool warns, held for two minutes, and pages after five.
func TestPgBouncerPoolStarvationSignalOneOldPoolWarnsThenPages(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{}
	for index := 1; index <= 32; index++ {
		fixtures = append(fixtures, pgbouncerStarvationHealthy(index))
	}
	fixtures[6] = pgbouncerStarvationLivelocked(7)
	alerts := runPgBouncerStarvationFixture(t, fixtures)
	alert := requireAlertClass(t, alerts, "pgbouncer-pool-starved-shard")
	if alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.PageSustain != 5 || alert.Target != "synthetic/db-1/pgbouncer@7" {
		t.Fatalf("wrong shard starvation identity: %+v", alert)
	}
	if !strings.Contains(alert.Markdown(), "PgBouncer pool db-1/pgbouncer@7 (app) has had 205 queued clients and no server for 3600 s; its oldest client has waited 10 s") {
		t.Fatalf("shard starvation has the wrong symptom:\n%s", alert.Markdown())
	}
	if count := requireAlertClassCount(alerts, "pgbouncer-pools-starved"); count != 0 {
		t.Fatalf("one starved pool paged as a fleet: %d", count)
	}
}

// Consoles the exporter could not read are unknown, reported beside the page
// that the read pools prove.
func TestPgBouncerPoolStarvationSignalUnansweredConsolesAreUnknownBesideThePage(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{}
	for index := 1; index <= 32; index++ {
		if index <= 3 {
			fixtures = append(fixtures, pgbouncerStarvationFixture{index: index, up: 0})
			continue
		}
		fixtures = append(fixtures, pgbouncerStarvationLivelocked(index))
	}
	alerts := runPgBouncerStarvationFixture(t, fixtures)
	page := requireAlertClass(t, alerts, "pgbouncer-pools-starved")
	if !strings.Contains(page.Markdown(), "29 of 29 read PgBouncer pools") {
		t.Fatalf("page counts unread pools:\n%s", page.Markdown())
	}
	unknown := requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
	if !unknown.unobservable || unknown.Severity != SeverityWarn || unknown.Sustain != 2 {
		t.Fatalf("unanswered consoles are not an unknown warning: %+v", unknown)
	}
	if !strings.Contains(unknown.Markdown(), "visibility_reason=console_unanswered=db-1/pgbouncer@1,db-1/pgbouncer@2,db-1/pgbouncer@3") {
		t.Fatalf("unknown warning does not name the unread consoles:\n%s", unknown.Markdown())
	}
}

// Every console unanswered (the exporter's reads failing, or PgBouncer frozen
// on a storage stall) is unknown, never healthy and never a measured page.
func TestPgBouncerPoolStarvationSignalEveryConsoleUnansweredIsUnknown(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{}
	for index := 1; index <= 32; index++ {
		fixtures = append(fixtures, pgbouncerStarvationFixture{index: index, up: 0})
	}
	alerts := runPgBouncerStarvationFixture(t, fixtures)
	if len(alerts) != 1 {
		t.Fatalf("every console unanswered produced %d alerts", len(alerts))
	}
	unknown := requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
	if !unknown.unobservable || !strings.Contains(unknown.Markdown(), "frozen on its log writes") {
		t.Fatalf("every console unanswered is not an unknown warning with the frozen-PgBouncer context: %+v", unknown)
	}
}

// No exporter series at all is unknown.
func TestPgBouncerPoolStarvationSignalMissingSeriesIsUnobservable(t *testing.T) {
	alert := requireAlertClass(t, runPgBouncerStarvationFixture(t, nil), "pgbouncer-pools-unobservable")
	if !strings.Contains(alert.Markdown(), "visibility_reason=exporter_series_missing") {
		t.Fatalf("missing series has no structural reason:\n%s", alert.Markdown())
	}
}

// Series the exporter or Fluent Bit stopped refreshing cannot prove
// starvation or health.
func TestPgBouncerPoolStarvationSignalStaleSeriesIsUnobservable(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{}
	for index := 1; index <= 32; index++ {
		fixture := pgbouncerStarvationLivelocked(index)
		fixture.sampleAge = 10 * time.Minute
		fixtures = append(fixtures, fixture)
	}
	alerts := runPgBouncerStarvationFixture(t, fixtures)
	if count := requireAlertClassCount(alerts, "pgbouncer-pools-starved"); count != 0 {
		t.Fatalf("stale series paged: %d", count)
	}
	alert := requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
	if !strings.Contains(alert.Markdown(), "visibility_reason=stale_series=db-1/pgbouncer@1") {
		t.Fatalf("stale series has no structural reason:\n%s", alert.Markdown())
	}
}

// A read console whose pool lacks a gauge is unknown, not healthy.
func TestPgBouncerPoolStarvationSignalIncompletePoolIsUnknown(t *testing.T) {
	fixtures := []pgbouncerStarvationFixture{}
	for index := 1; index <= 32; index++ {
		fixtures = append(fixtures, pgbouncerStarvationHealthy(index))
	}
	fixtures[3].maxWait = nil
	fixtures[4].noPool = true
	alert := requireAlertClass(t, runPgBouncerStarvationFixture(t, fixtures), "pgbouncer-pools-unobservable")
	if !strings.Contains(alert.Markdown(), "visibility_reason=incomplete_series=db-1/pgbouncer@4,db-1/pgbouncer@5") {
		t.Fatalf("incomplete pools have no structural reason:\n%s", alert.Markdown())
	}
}

// Malformed samples make the whole frame unknown rather than a measured zero.
func TestPgBouncerPoolStarvationSignalMalformedSamplesAreUnobservable(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	vector := func(labels map[string]string, value string) string {
		payload, err := json.Marshal(map[string]any{
			"status": "success",
			"data": map[string]any{"resultType": "vector", "result": []map[string]any{{
				"metric": labels, "value": []any{float64(now.Unix()), value},
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	for _, c := range []struct {
		labels map[string]string
		value  string
		reason string
	}{
		{labels: map[string]string{"host": "db-1", "pgbouncer": "1", "monitor_metric": "up"}, value: "-1", reason: "invalid_metric"},
		{labels: map[string]string{"host": "db-1", "pgbouncer": "1", "monitor_metric": "up"}, value: "NaN", reason: "invalid_metric"},
		{labels: map[string]string{"pgbouncer": "1", "monitor_metric": "up"}, value: "1", reason: "unlabeled_series"},
		{labels: map[string]string{"host": "db-1", "pgbouncer": "1", "monitor_metric": "servers"}, value: "1", reason: "unlabeled_series"},
		{labels: map[string]string{"host": "db-1", "pgbouncer": "1", "monitor_metric": "cl_cancel"}, value: "1", reason: "unknown_metric_class"},
	} {
		alerts := runPgBouncerStarvationPayload(t, now, vector(c.labels, c.value))
		alert := requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
		if !strings.Contains(alert.Markdown(), "visibility_reason="+c.reason) {
			t.Errorf("%v %s: unobservable reason is not %s:\n%s", c.labels, c.value, c.reason, alert.Markdown())
		}
	}
}

// Instances order by host, then numerically, and long lists are capped.
func TestPgBouncerPoolStarvationInstancesOrderNumerically(t *testing.T) {
	names := []string{"db-1/pgbouncer@10", "db-1/pgbouncer@9", "db-0/pgbouncer@12", "db-1/pgbouncer@2"}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if pgbouncerStarvationInstanceLess(names[j], names[i]) {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	if got := strings.Join(names, " "); got != "db-0/pgbouncer@12 db-1/pgbouncer@2 db-1/pgbouncer@9 db-1/pgbouncer@10" {
		t.Fatalf("instance order = %s", got)
	}
	many := []string{}
	for index := 1; index <= 10; index++ {
		many = append(many, fmt.Sprintf("p%d", index))
	}
	if got := pgbouncerStarvationNames(many); got != "p1,p2,p3,p4,p5,p6,p7,p8,and 2 more" {
		t.Fatalf("capped names = %s", got)
	}
}

// The old-starvation rule's two arms and its boundaries.
func TestPgBouncerPoolStarvationOldStarvationBoundaries(t *testing.T) {
	for _, c := range []struct {
		waiting, servers, maxWait, starvedSeconds float64
		old                                       bool
	}{
		{waiting: 1, servers: 0, maxWait: 5, starvedSeconds: 0, old: true},
		{waiting: 1, servers: 0, maxWait: 4.999, starvedSeconds: 30, old: true},
		{waiting: 1, servers: 0, maxWait: 4.999, starvedSeconds: 29.9, old: false},
		{waiting: 0, servers: 0, maxWait: 10, starvedSeconds: 600, old: false},
		{waiting: 9, servers: 1, maxWait: 10, starvedSeconds: 0, old: false},
		{waiting: 9, servers: 0, maxWait: math.Inf(1), starvedSeconds: 0, old: true},
	} {
		pool := &pgbouncerStarvationPool{values: map[string]float64{
			"waiting": c.waiting, "servers": c.servers, "max_wait": c.maxWait, "starved_seconds": c.starvedSeconds,
		}}
		if pool.old() != c.old {
			t.Errorf("waiting=%g servers=%g max_wait=%g starved_seconds=%g: old=%v, want %v",
				c.waiting, c.servers, c.maxWait, c.starvedSeconds, pool.old(), c.old)
		}
	}
}
