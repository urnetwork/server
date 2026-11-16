package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Incident A replayed from recorded Mimir history: the waiting arm warns at
// 19:29Z on 10-25 (100.05 waiting, the tenth evaluation above 100) and not a
// minute earlier, the page follows at 20:03Z, the oldest-wait arm alone warns
// on 10-24 at 15:45Z, and the healthy reference of 10-20 raises nothing.
func TestPgBouncerBacklogReplaysIncidentA(t *testing.T) {
	cases := loadStormReplayFixture(t, "pgbouncer-backlog")
	for _, test := range []struct {
		name     string
		severity Severity
		observed []string
	}{
		{name: "incident-a-warn", severity: SeverityWarn, observed: []string{"instances_answering=32/32"}},
		{name: "incident-a-before-warn"},
		{name: "incident-a-page", severity: SeverityPage, observed: []string{"waiting_now=1633.3"}},
		{name: "oldest-wait-only", severity: SeverityWarn, observed: []string{"oldest_wait_now_seconds=0.837"}},
		{name: "healthy"},
	} {
		alerts := runStormReplay(t, NewPgBouncerBacklogSignal(), cases[test.name], nil)
		if test.severity == "" {
			requireStormAlerts(t, test.name, alerts, map[string][]string{})
			continue
		}
		requireStormAlerts(t, test.name, alerts, map[string][]string{"pgbouncer-backlog": {"main/pgbouncer"}})
		alert := requireAlertClass(t, alerts, "pgbouncer-backlog")
		if alert.Severity != test.severity || alert.Sustain != 1 || alert.SignalNumber != "26.1" || alert.SignalID != "mimir/pgbouncer-backlog" || alert.unobservable {
			t.Fatalf("%s: %+v", test.name, alert)
		}
		for _, want := range test.observed {
			if !strings.Contains(alert.Observed, want) {
				t.Fatalf("%s: observed %q lacks %q", test.name, alert.Observed, want)
			}
		}
	}
	oldest := requireAlertClass(t, runStormReplay(t, NewPgBouncerBacklogSignal(), cases["oldest-wait-only"], nil), "pgbouncer-backlog")
	if !strings.Contains(oldest.Symptom, "oldest wait above 0.5 s") || strings.Contains(oldest.Symptom, "waiting above") {
		t.Fatalf("the oldest-wait arm was not named alone: %q", oldest.Symptom)
	}
}

// One synthetic answer: waiting and oldest wait per step, and the instances
// both cover.
func pgbouncerBacklogResponse(t *testing.T, waiting, oldest, instances []float64) string {
	return stormSyntheticResponse(t, stormSyntheticStart(10), time.Minute, []stormSyntheticSeries{
		{part: "waiting", values: waiting},
		{part: "oldest_wait", values: oldest},
		{part: "waiting_instances", values: instances},
		{part: "oldest_wait_instances", values: instances},
	})
}

// Levels are strict, a hold needs every evaluation, a sum over fewer pools
// still proves a page, and only complete coverage clears.
func TestPgBouncerBacklogBandsHoldsAndCoverage(t *testing.T) {
	probe := pgbouncerBacklogProbe{}
	full := stormRepeat(32, 10)
	quiet := stormRepeat(0.1, 10)
	identity := "pgbouncer-backlog main/pgbouncer"
	visibility := "pgbouncer-backlog-unobservable main/pgbouncer-backlog"

	atLevel := stormRepeat(150, 10)
	atLevel[3] = 100
	pageAlone := append(stormRepeat(50, 5), stormRepeat(1001, 5)...)
	missing := stormRepeat(150, 10)
	missing[7] = math.NaN()
	for _, test := range []struct {
		name      string
		waiting   []float64
		oldest    []float64
		instances []float64
		mutate    func(*SignalSettings)
		want      map[string]string
	}{
		{name: "quiet", waiting: stormRepeat(16, 10), oldest: stormRepeat(0.37, 10), instances: full, want: map[string]string{identity: "healthy", visibility: "healthy"}},
		{name: "level is not beyond", waiting: atLevel, oldest: quiet, instances: full, want: map[string]string{identity: "hold", visibility: "healthy"}},
		{name: "page needs only its five minutes", waiting: pageAlone, oldest: quiet, instances: full, want: map[string]string{identity: "page"}},
		{name: "oldest wait at its level", waiting: stormRepeat(0, 10), oldest: stormRepeat(0.5, 10), instances: full, want: map[string]string{identity: "healthy"}},
		{name: "a missing evaluation holds", waiting: missing, oldest: quiet, instances: full, want: map[string]string{identity: "hold", visibility: "unobservable"}},
		{name: "fewer pools still prove a page", waiting: stormRepeat(1500, 10), oldest: quiet, instances: stormRepeat(31, 10), want: map[string]string{identity: "page", visibility: "unobservable"}},
		{name: "fewer pools never clear", waiting: stormRepeat(0, 10), oldest: stormRepeat(0, 10), instances: stormRepeat(31, 10), want: map[string]string{identity: "hold", visibility: "unobservable"}},
		{name: "an unconfigured census never clears", waiting: stormRepeat(0, 10), oldest: stormRepeat(0, 10), instances: full,
			mutate: func(settings *SignalSettings) { settings.Hosts[4].PgBouncerInstances = 0 },
			want:   map[string]string{identity: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, pgbouncerBacklogResponse(t, test.waiting, test.oldest, test.instances), test.mutate)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestPgBouncerBacklogDefaultsAreTheCatalogPolicy(t *testing.T) {
	settings := DefaultPgBouncerBacklogSettings()
	if settings.WarnWaiting != 100 || settings.WarnHold != 10*time.Minute || settings.PageWaiting != 1000 || settings.PageHold != 5*time.Minute ||
		settings.WarnOldestWait != 0.5 || settings.OldestWaitHold != 10*time.Minute || settings.Window != 5*time.Minute || settings.Step != time.Minute {
		t.Fatalf("§26.1 policy changed: %+v", settings)
	}
	query := pgbouncerBacklogQuery("main", settings)
	for _, want := range []string{
		`sum(avg_over_time(pgbouncer_pool_clients_waiting{env="main"}[5m]))`,
		`max(max_over_time(pgbouncer_pool_maxwait_seconds{env="main"}[5m]))`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost the catalog expression %s", want)
		}
	}
}
