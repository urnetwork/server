package monitor

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

var identityChurnNow = time.Date(2026, 10, 22, 6, 32, 0, 0, time.UTC)

var identityChurnSlots = []fleetSlot{
	{service: "proxy", host: "proxy-a.example", block: "g1", allowed: true},
	{service: "proxy", host: "proxy-b.example", block: "g1", allowed: true},
}

// One evaluation of the proxy fleet plus the PostgreSQL lifetime row.
type identityChurnFixture struct {
	scalars   map[string]float64
	discovery map[string]float64
	fresh     map[string]time.Time
	lifetime  Row
}

// The 2026-10-15 baseline: identities lived and nothing was refused.
func healthyIdentityChurnFixture() *identityChurnFixture {
	at := fleetEvaluationTime(identityChurnNow, DefaultIdentityChurnSettings().EvaluationLag)
	fixture := &identityChurnFixture{
		scalars: map[string]float64{
			"mints": 57, "retires": 8, "refusals": 0, "devices": 6800, "per_device": 0.5,
			"per_device_hold": 0.5, "per_device_hold_points": 6, "per_device_hold_expected": 6,
			"per_device_median": 0.5, "per_device_median_points": 168, "pgbouncer_waiting": 0, "contract_mean": 0.05,
		},
		discovery: map[string]float64{"quota": 2.5, "exhausted": 0.2, "unavailable": 0.2},
		fresh:     map[string]time.Time{},
		lifetime:  Row{"3420", "3000", "200", "400", "30"},
	}
	for _, family := range []string{"devices", "mints", "refusals", "discovery"} {
		for _, slot := range identityChurnSlots {
			fixture.fresh[family+"/"+slot.host] = at.Add(-20 * time.Second)
		}
	}
	return fixture
}

// The 2026-10-22 06:20Z loop: every criterion and the database side.
func incidentIdentityChurnFixture() *identityChurnFixture {
	fixture := healthyIdentityChurnFixture()
	for part, value := range map[string]float64{
		"mints": 300, "retires": 260, "refusals": 600, "devices": 10000, "per_device": 1.8,
		"per_device_hold": 1.6, "pgbouncer_waiting": 900, "contract_mean": 1.35,
	} {
		fixture.scalars[part] = value
	}
	fixture.discovery = map[string]float64{"quota": 30, "exhausted": 16, "unavailable": 44}
	fixture.lifetime = Row{"18676", "1494", "12139", "13073", "8"}
	return fixture
}

func (self *identityChurnFixture) json(t testing.TB) string {
	t.Helper()
	at := fleetEvaluationTime(identityChurnNow, DefaultIdentityChurnSettings().EvaluationLag)
	fixture := newFleetFixture(at)
	for part, value := range self.scalars {
		fixture.scalar(part, value)
	}
	for outcome, value := range self.discovery {
		fixture.labeled("discovery", map[string]string{"outcome": outcome}, value)
	}
	for key, sourceTime := range self.fresh {
		family, hostName, _ := strings.Cut(key, "/")
		fixture.fresh(family, fleetSlot{service: "proxy", host: hostName, block: "g1"}, sourceTime)
	}
	return fixture.json(t)
}

func identityChurnSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return identityChurnNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	hosts := []string{}
	for _, slot := range identityChurnSlots {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: slot.host, Roles: []string{"edge"}})
		hosts = append(hosts, slot.host)
	}
	settings.LogServices = []string{"proxy"}
	settings.LogServiceHosts = map[string][]string{"proxy": hosts}
	settings.LogServiceBlocks = map[string][]string{"proxy": {"g1"}}
	return settings
}

type identityChurnRun struct {
	alerts   Alerts
	commands []string
	queries  []string
}

func runIdentityChurnFixture(t *testing.T, fixture *identityChurnFixture, mutate func(*syntheticSource, *SignalSettings)) identityChurnRun {
	t.Helper()
	run := identityChurnRun{}
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" {
				t.Fatalf("fleet read contacted a non-gateway host")
			}
			run.commands = append(run.commands, command)
			return fixture.json(t), nil
		},
		postgresFn: func(query string) ([]Row, error) {
			run.queries = append(run.queries, query)
			return []Row{fixture.lifetime}, nil
		},
	}
	settings := identityChurnSettings(source)
	if mutate != nil {
		mutate(source, &settings)
	}
	alerts, err := NewIdentityChurnSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	run.alerts = alerts
	return run
}

func TestIdentityChurnLoopPagesOnTheOctoberIncident(t *testing.T) {
	run := runIdentityChurnFixture(t, incidentIdentityChurnFixture(), nil)
	churn := requireAlertClass(t, run.alerts, "identity-churn")
	loop := requireAlertClass(t, run.alerts, "identity-churn-loop")
	if len(run.alerts) != 2 || churn.Severity != SeverityWarn || loop.Severity != SeverityPage || churn.Sustain != 2 || loop.Sustain != 2 ||
		churn.Target != "synthetic/hosted-identities" || loop.Target != churn.Target {
		t.Fatalf("incident alerts = %+v", run.alerts)
	}
	for _, want := range []string{
		"criteria=per_device_mints,retire_ratio,mint_refusals,discovery_unresolved,short_lived",
		"mints_per_s=300.0", "retire_mint_ratio=0.87", "refusals_per_s=600.0", "held_min_30m0s=1.60", "weekly_median=0.50",
		"unresolved_share=0.67", "pgbouncer_clients_waiting=900", "contract_mean_ms=1350.0",
		"lifetime_identities=18676", "alive_after_10m0s=8.0%", "retired_never_connected=65.0%", "retired_median_life=8s",
		"expected_slots=2 complete_slots=2", "Do not restart proxies fleet-wide",
	} {
		if !strings.Contains(loop.Markdown(), want) {
			t.Fatalf("loop alert lacks %q:\n%s", want, loop.Markdown())
		}
	}
	for _, alert := range run.alerts {
		requireAlertOmits(t, alert, "proxy_device_config", "client_id")
	}
}

func TestIdentityChurnHealthyBaselineIsQuiet(t *testing.T) {
	if run := runIdentityChurnFixture(t, healthyIdentityChurnFixture(), nil); len(run.alerts) != 0 {
		t.Fatalf("baseline alerted: %+v", run.alerts)
	}
}

// Each criterion trips alone, and its boundary or qualifier does not.
func TestIdentityChurnCriteriaAndQualifiers(t *testing.T) {
	for _, c := range []struct {
		name      string
		mutate    func(*identityChurnFixture)
		criterion string
	}{
		{name: "per-device over twice the median", mutate: func(f *identityChurnFixture) { f.scalars["per_device_hold"] = 1.01 }, criterion: "per_device_mints"},
		{name: "per-device at twice the median", mutate: func(f *identityChurnFixture) { f.scalars["per_device_hold"] = 1.0 }},
		{name: "growing fleet", mutate: func(f *identityChurnFixture) {
			f.scalars["mints"], f.scalars["retires"], f.scalars["devices"] = 400, 40, 48000
		}},
		{name: "retire ratio over band", mutate: func(f *identityChurnFixture) { f.scalars["mints"], f.scalars["retires"] = 200, 170 }, criterion: "retire_ratio"},
		{name: "retire ratio at low mints", mutate: func(f *identityChurnFixture) { f.scalars["mints"], f.scalars["retires"] = 150, 140 }},
		{name: "refusals over band", mutate: func(f *identityChurnFixture) { f.scalars["refusals"] = 100.5 }, criterion: "mint_refusals"},
		{name: "refusals at band", mutate: func(f *identityChurnFixture) { f.scalars["refusals"] = 100 }},
		{name: "discovery unresolved", mutate: func(f *identityChurnFixture) {
			f.discovery = map[string]float64{"quota": 6.9, "exhausted": 1.1, "unavailable": 2}
		}, criterion: "discovery_unresolved"},
		{name: "discovery too few outcomes", mutate: func(f *identityChurnFixture) {
			f.discovery = map[string]float64{"quota": 0.1, "exhausted": 0.4, "unavailable": 0.4}
		}},
		{name: "short-lived identities", mutate: func(f *identityChurnFixture) { f.lifetime = Row{"5000", "1999", "2500", "2600", "9"} }, criterion: "short_lived"},
		{name: "short-lived but few identities", mutate: func(f *identityChurnFixture) { f.lifetime = Row{"2999", "100", "2800", "2850", "9"} }},
		{name: "short-lived but connected", mutate: func(f *identityChurnFixture) { f.lifetime = Row{"5000", "1999", "2499", "2600", "9"} }},
	} {
		fixture := healthyIdentityChurnFixture()
		c.mutate(fixture)
		run := runIdentityChurnFixture(t, fixture, nil)
		if c.criterion == "" {
			if len(run.alerts) != 0 {
				t.Fatalf("%s alerted: %+v", c.name, run.alerts)
			}
			continue
		}
		alert := requireAlertClass(t, run.alerts, "identity-churn")
		if len(run.alerts) != 1 || alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "criteria="+c.criterion+" ") {
			t.Fatalf("%s: alerts = %+v", c.name, run.alerts)
		}
	}
}

// The loop needs the database side; each side engages it on its own evidence,
// and unread database evidence beside proven churn stays unknown.
func TestIdentityChurnLoopRequiresDatabaseSide(t *testing.T) {
	for _, c := range []struct {
		name    string
		waiting float64
		mean    float64
		loop    bool
		unknown bool
	}{
		{name: "quiet database", waiting: 400, mean: 0.9},
		{name: "queued clients", waiting: 501, mean: 0.05, loop: true},
		{name: "slow contracts", waiting: 0, mean: 1.01, loop: true},
		{name: "no contract replies", waiting: 0, mean: math.Inf(1), loop: true},
		{name: "queued clients unread", waiting: math.NaN(), mean: 0.5, unknown: true},
		{name: "queued clients unread but slow contracts", waiting: math.NaN(), mean: 2, loop: true},
	} {
		fixture := healthyIdentityChurnFixture()
		fixture.scalars["refusals"] = 600
		fixture.scalars["pgbouncer_waiting"], fixture.scalars["contract_mean"] = c.waiting, c.mean
		if math.IsNaN(c.waiting) {
			delete(fixture.scalars, "pgbouncer_waiting")
		}
		run := runIdentityChurnFixture(t, fixture, nil)
		requireAlertClass(t, run.alerts, "identity-churn")
		loops := requireAlertClassCount(run.alerts, "identity-churn-loop")
		unknowns := requireAlertClassCount(run.alerts, "identity-churn-unobservable")
		if (loops == 1) != c.loop || (unknowns == 1) != c.unknown || len(run.alerts) != 1+loops+unknowns {
			t.Fatalf("%s: alerts = %+v", c.name, run.alerts)
		}
		if c.unknown && !strings.Contains(requireAlertClass(t, run.alerts, "identity-churn-unobservable").Observed, "loop_context_unknown") {
			t.Fatalf("%s: unknown loop reason lost", c.name)
		}
	}
}

func TestIdentityChurnUnknownControls(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*identityChurnFixture)
		reason string
	}{
		{name: "missing slot", mutate: func(f *identityChurnFixture) { delete(f.fresh, "refusals/proxy-b.example") }, reason: "slot_coverage_incomplete"},
		{name: "stale slot", mutate: func(f *identityChurnFixture) {
			f.fresh["devices/proxy-a.example"] = identityChurnNow.Add(-8 * time.Minute)
		}, reason: "slot_coverage_incomplete"},
		{name: "hold incomplete", mutate: func(f *identityChurnFixture) { f.scalars["per_device_hold_points"] = 5 }, reason: "per_device_hold_incomplete"},
		{name: "hold unbounded", mutate: func(f *identityChurnFixture) { f.scalars["per_device_hold"] = math.Inf(1) }, reason: "per_device_hold_incomplete"},
		{name: "baseline short", mutate: func(f *identityChurnFixture) { f.scalars["per_device_median_points"] = 83 }, reason: "per_device_baseline_short"},
		{name: "baseline zero", mutate: func(f *identityChurnFixture) { f.scalars["per_device_median"] = 0 }, reason: "per_device_baseline_short"},
		{name: "mints missing", mutate: func(f *identityChurnFixture) { delete(f.scalars, "mints") }, reason: "mint_rate_unknown"},
		{name: "refusals missing", mutate: func(f *identityChurnFixture) { delete(f.scalars, "refusals") }, reason: "refusal_rate_unknown"},
		{name: "unknown outcome", mutate: func(f *identityChurnFixture) { f.discovery["Hostile"] = 1 }, reason: "unrecognized_discovery_outcome"},
		{name: "inconsistent lifetime", mutate: func(f *identityChurnFixture) { f.lifetime = Row{"10", "11", "0", "0", "-1"} }, reason: "lifetime_unavailable"},
		{name: "malformed lifetime", mutate: func(f *identityChurnFixture) { f.lifetime = Row{"10", "one", "0", "0", "-1"} }, reason: "lifetime_unavailable"},
	} {
		fixture := healthyIdentityChurnFixture()
		c.mutate(fixture)
		run := runIdentityChurnFixture(t, fixture, nil)
		unknown := requireAlertClass(t, run.alerts, "identity-churn-unobservable")
		if len(run.alerts) != 1 || !unknown.unobservable || !strings.Contains(unknown.Observed, c.reason) {
			t.Fatalf("%s: alerts = %+v", c.name, run.alerts)
		}
	}
}

// Each source failure leaves only its own evidence unknown: churn proven by
// the other source still alerts.
func TestIdentityChurnSourcesFailIndependently(t *testing.T) {
	run := runIdentityChurnFixture(t, incidentIdentityChurnFixture(), func(source *syntheticSource, _ *SignalSettings) {
		source.hostFn = func(HostSettings, string) (string, error) { return "", errors.New("synthetic gateway refused") }
	})
	churn := requireAlertClass(t, run.alerts, "identity-churn")
	unknown := requireAlertClass(t, run.alerts, "identity-churn-unobservable")
	if len(run.alerts) != 2 || !strings.Contains(churn.Observed, "criteria=short_lived ") || !strings.Contains(unknown.Observed, "metrics_unavailable") {
		t.Fatalf("lifetime churn lost with metrics down: %+v", run.alerts)
	}
	requireAlertOmits(t, unknown, "synthetic gateway refused")

	run = runIdentityChurnFixture(t, incidentIdentityChurnFixture(), func(source *syntheticSource, _ *SignalSettings) {
		source.postgresFn = func(string) ([]Row, error) { return nil, errors.New("synthetic database refused") }
	})
	churn = requireAlertClass(t, run.alerts, "identity-churn")
	requireAlertClass(t, run.alerts, "identity-churn-loop")
	unknown = requireAlertClass(t, run.alerts, "identity-churn-unobservable")
	if len(run.alerts) != 3 || strings.Contains(churn.Observed, "short_lived") || !strings.Contains(unknown.Observed, "lifetime_unavailable") {
		t.Fatalf("metric churn lost with the database down: %+v", run.alerts)
	}
	requireAlertOmits(t, unknown, "synthetic database refused")

	run = runIdentityChurnFixture(t, healthyIdentityChurnFixture(), func(_ *syntheticSource, settings *SignalSettings) {
		settings.ExcludedHosts = []string{"proxy-b.example"}
	})
	unknown = requireAlertClass(t, run.alerts, "identity-churn-unobservable")
	if len(run.alerts) != 1 || !strings.Contains(unknown.Observed, "proxy/proxy-b.example/g1[excluded]") {
		t.Fatalf("excluded proxy slot was certified: %+v", run.alerts)
	}
}

// Both reads are bounded, aggregate-only and take their history from Mimir.
func TestIdentityChurnReadShapes(t *testing.T) {
	run := runIdentityChurnFixture(t, healthyIdentityChurnFixture(), nil)
	if len(run.commands) != 1 || len(run.queries) != 1 {
		t.Fatalf("reads = %d commands, %d queries", len(run.commands), len(run.queries))
	}
	at := identityChurnNow.Add(-2 * time.Minute).Unix()
	mint := `urnetwork_jwt_state_queries_total{env="synthetic",job="proxy",caller="hosted",operation="mint"}`
	for _, want := range []string{
		"--max-time 15", "--max-filesize 1048576", "time=" + strconv.FormatInt(at, 10),
		"min_over_time((60 * sum(rate(" + mint + "[1800s])) / sum(urnetwork_proxy_devices_active{env=\"synthetic\",job=\"proxy\"}))[1800s:300s])",
		"quantile_over_time(0.5, (60 * sum(rate(" + mint + "[1800s])) / sum(urnetwork_proxy_devices_active{env=\"synthetic\",job=\"proxy\"}))[604800s:3600s])",
		"sum(rate(urnetwork_hosted_mint_admission_refusals_total{env=\"synthetic\",job=\"proxy\"}[900s]))",
		"sum by (outcome) (rate(urnetwork_findproviders2_native_source_outcomes_total{env=\"synthetic\",job=\"proxy\",source=\"primary\"}[1800s]))",
		"pgbouncer_pool_clients_waiting",
	} {
		if !strings.Contains(run.commands[0], want) {
			t.Fatalf("fleet query lacks %q:\n%s", want, run.commands[0])
		}
	}
	query := run.queries[0]
	for _, want := range []string{
		"now() - interval '660 seconds'", "now() - interval '600 seconds'", "LIMIT 200001",
		"EXISTS (SELECT 1 FROM proxy_device_config p WHERE p.client_id = n.source_client_id)",
		"NOT EXISTS (SELECT 1 FROM network_client_connection cc WHERE cc.client_id = n.client_id)",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("lifetime query lacks %q:\n%s", want, query)
		}
	}
	selectList := query[strings.LastIndex(query, "SELECT\n"):]
	if strings.Contains(selectList, "client_id") || strings.Contains(selectList, "network_id") {
		t.Fatalf("lifetime query returns identities:\n%s", selectList)
	}
}

func TestIdentityChurnCanceledParentPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		cancel()
		return "", context.Canceled
	}}
	alerts, err := NewIdentityChurnSignal().Run(ctx, identityChurnSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("canceled run = %+v, %v", alerts, err)
	}
}
