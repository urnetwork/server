// Tests for SIGNALS.md §2.19b, the egress site pool: every condition from a
// synthetic source, the unobservable and malformed cases, and the production
// queries against a seeded database.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The pool settings a synthetic deployment judges by: a pool of one or two
// sites per class, and a candidate listed for every class.
func testEgressSitePoolContext() *egressSitePoolContext {
	siteSettings := model.DefaultProviderEgressSiteSettings()
	siteSettings.SitePoolSize = map[string]int{"dns": 1, "connectivity": 1, "cdn": 1, "site": 2}
	return &egressSitePoolContext{
		siteSettings: siteSettings,
		rules:        model.DefaultProviderEgressRules(),
		candidateNameClasses: map[string]string{
			"dns-candidate":          "dns",
			"connectivity-candidate": "connectivity",
			"cdn-candidate":          "cdn",
			"site-candidate":         "site",
		},
	}
}

// The signal over poolContext rather than the deployment's configuration.
func testEgressSitePoolSignal(poolContext *egressSitePoolContext) Signal {
	signal := NewEgressSitePoolSignal().(*signalAdapter)
	signal.probe = egressSitePoolProbe{
		settings: DefaultEgressSitePoolSettings(),
		loadPoolSettings: func() (*egressSitePoolContext, error) {
			return poolContext, nil
		},
	}
	return signal
}

// One cadence's answers, query by query, with Mimir's; every part starts
// healthy and a test changes the part it is about.
type egressSitePoolFixture struct {
	destinations [][]string
	regional     [][]string
	places       [][]string
	countries    [][]string
	task         []string
	retries      []string
	classLoads   [][]string
	borrowed     map[string]float64
	answered     map[string]float64
	guardTrips   map[string]float64
	// mimirDown makes the services host refuse the query
	mimirDown bool
}

// A destination row: name, class, active, probation, retired age, retire
// count, age above the retire line, failure share, samples.
func testEgressSitePoolDestinationRow(name string, class string, active bool, aboveRetireAge int64, failureShare float64) []string {
	return []string{
		name, class, strconv.FormatBool(active), "false", "-1", "0",
		strconv.FormatInt(aboveRetireAge, 10), strconv.FormatFloat(failureShare, 'f', -1, 64), "250",
	}
}

// A deployment where every condition holds.
func healthyEgressSitePoolFixture() *egressSitePoolFixture {
	return &egressSitePoolFixture{
		destinations: [][]string{
			testEgressSitePoolDestinationRow("resolver-a", "dns", true, -1, 0.01),
			testEgressSitePoolDestinationRow("portal-a", "connectivity", true, -1, 0),
			testEgressSitePoolDestinationRow("edge-a", "cdn", true, -1, 0.02),
			testEgressSitePoolDestinationRow("news-a", "site", true, -1, 0.1),
			testEgressSitePoolDestinationRow("news-b", "site", true, -1, 0.1),
		},
		task:    []string{"1", "0", "0", "-3600"},
		retries: []string{"0", "0", "0"},
		classLoads: [][]string{
			{"cdn", "990", "1000"},
			{"connectivity", "1000", "1000"},
			{"dns", "995", "1000"},
			{"site", "2400", "2600"},
		},
		borrowed:   map[string]float64{"quality": 5, "speed": 1},
		answered:   map[string]float64{"quality": 100, "speed": 80},
		guardTrips: map[string]float64{"full": 0, "blackhole": 0},
	}
}

// The Mimir answer: one vector with every part tagged as the query tags it.
func (self *egressSitePoolFixture) mimirResponse() string {
	// one series of an instant vector
	type series struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	result := []series{}
	add := func(part string, label string, values map[string]float64) {
		keys := []string{}
		for key := range values {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			result = append(result, series{
				Metric: map[string]string{"monitor_egress_part": part, label: key},
				Value:  []any{1788000000, strconv.FormatFloat(values[key], 'f', -1, 64)},
			})
		}
	}
	add("borrowed", "rank_mode", self.borrowed)
	add("answered", "rank_mode", self.answered)
	add("guard_trips", "schedule", self.guardTrips)
	response, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		panic(err)
	}
	return string(response)
}

// A source that answers the signal's queries from the fixture.
func (self *egressSitePoolFixture) source(t *testing.T) *syntheticSource {
	rows := func(values [][]string) []Row {
		out := []Row{}
		for _, value := range values {
			out = append(out, Row(value))
		}
		return out
	}
	return &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "monitor-signal-2.19b-egress-site-pool-destinations"):
				return rows(self.destinations), nil
			case strings.Contains(query, "monitor-signal-2.19b-egress-site-pool-regional"):
				return rows(self.regional), nil
			case strings.Contains(query, "monitor-signal-2.19b-egress-site-pool-places"):
				return rows(self.places), nil
			case strings.Contains(query, "monitor-signal-2.19b-egress-site-pool-countries"):
				return rows(self.countries), nil
			case strings.Contains(query, "monitor-signal-2.19b-egress-site-pool-task"):
				return []Row{Row(self.task)}, nil
			case strings.Contains(query, "monitor-signal-2.19b-egress-site-pool-retries"):
				return []Row{Row(self.retries)}, nil
			case strings.Contains(query, "monitor-signal-2.19b-egress-site-pool-fleet"):
				return rows(self.classLoads), nil
			default:
				t.Fatalf("unexpected egress site pool query: %s", query)
				return nil, nil
			}
		},
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" || !strings.Contains(command, "/prometheus/api/v1/query") {
				t.Fatalf("unexpected Mimir command on %s: %s", host.Name, command)
			}
			if self.mimirDown {
				return "", fmt.Errorf("synthetic Mimir refused")
			}
			return sitePoolCoverageTestResponse(t, self, command, nil), nil
		},
	}
}

// Settings with a services host for Mimir beside the synthetic database.
func testEgressSitePoolSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "api-a.example"}, HostSettings{Name: "api-b.example"}, HostSettings{Name: "worker-a.example"})
	settings.LogServices = []string{"api", "taskworker"}
	settings.LogServiceHosts = map[string][]string{"api": {"api-a.example", "api-b.example"}, "taskworker": {"worker-a.example"}}
	settings.LogServiceBlocks = map[string][]string{"api": {"blue"}, "taskworker": {"blue"}}
	return settings
}

// The class/frame of every alert, in order.
func testEgressSitePoolAlertKeys(alerts Alerts) []string {
	keys := []string{}
	for _, alert := range alerts {
		keys = append(keys, alert.Class+"/"+alert.Frame)
	}
	return keys
}

// Runs the signal over one fixture and returns its alert keys.
func runEgressSitePoolFixture(t *testing.T, fixture *egressSitePoolFixture, poolContext *egressSitePoolContext) (Alerts, []string) {
	t.Helper()
	alerts, err := testEgressSitePoolSignal(poolContext).Run(context.Background(), testEgressSitePoolSettings(fixture.source(t)))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, alert := range alerts {
		requireAlertClass(t, alerts, alert.Class)
	}
	return alerts, testEgressSitePoolAlertKeys(alerts)
}

// A pool that holds on every condition raises nothing.
func TestEgressSitePoolSignalSyntheticHealthy(t *testing.T) {
	_, keys := runEgressSitePoolFixture(t, healthyEgressSitePoolFixture(), testEgressSitePoolContext())
	if 0 < len(keys) {
		t.Fatalf("a healthy pool raised %v", keys)
	}
}

// An active scored site above the retire line for more than a day is named;
// one on probation, one not yet a day over, and a retired one are not.
func TestEgressSitePoolSignalSyntheticNeedsRefresh(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	probation := testEgressSitePoolDestinationRow("news-probation", "site", true, 200000, 0.8)
	probation[3] = "true"
	fixture.destinations = append(fixture.destinations,
		testEgressSitePoolDestinationRow("news-stuck", "site", true, 100000, 0.7),
		testEgressSitePoolDestinationRow("news-recent", "site", true, 3600, 0.7),
		testEgressSitePoolDestinationRow("news-retired", "site", false, 200000, 0.7),
		probation,
	)
	alerts, keys := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
	if strings.Join(keys, ",") != "egress-site-pool-needs-refresh/news-stuck" {
		t.Fatalf("alerts = %v, want news-stuck alone", keys)
	}
	if !strings.Contains(alerts[0].Observed, "failure_share=0.700 samples=250 above_retire_seconds=100000") {
		t.Errorf("observed = %q", alerts[0].Observed)
	}
}

// The refresh chain lost, parked on errors, or not claimed for more than a
// cadence each have their frame.
func TestEgressSitePoolSignalSyntheticRefreshNotRunning(t *testing.T) {
	for _, test := range []struct {
		task  []string
		frame string
	}{
		{task: []string{"0", "0", "0", "0"}, frame: "task-missing"},
		{task: []string{"1", "1", "4", "-60"}, frame: "task-parked"},
		{task: []string{"1", "0", "0", "90000"}, frame: "stale-run"},
		{task: []string{"1", "0", "0", "80000"}, frame: ""},
	} {
		fixture := healthyEgressSitePoolFixture()
		fixture.task = test.task
		_, keys := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
		want := []string{}
		if test.frame != "" {
			want = append(want, "egress-site-refresh-not-running/"+test.frame)
		}
		if !slices.Equal(keys, want) {
			t.Errorf("task %v: alerts = %v, want %v", test.task, keys, want)
		}
	}
}

// A class under its pool size, or at it with nothing left to promote, is
// thin; a candidate egress-sites.yml lists and the refresh has not synced yet
// counts.
func TestEgressSitePoolSignalSyntheticPoolThin(t *testing.T) {
	poolContext := testEgressSitePoolContext()
	poolContext.siteSettings.SitePoolSize["dns"] = 2
	delete(poolContext.candidateNameClasses, "cdn-candidate")
	fixture := healthyEgressSitePoolFixture()
	// a site retired for good and one cooling down are not candidates
	dropped := testEgressSitePoolDestinationRow("edge-dropped", "cdn", false, -1, 0)
	dropped[4], dropped[5] = "99999999", "3"
	cooling := testEgressSitePoolDestinationRow("edge-cooling", "cdn", false, -1, 0)
	cooling[4], cooling[5] = "3600", "1"
	fixture.destinations = append(fixture.destinations, dropped, cooling)

	_, keys := runEgressSitePoolFixture(t, fixture, poolContext)
	want := []string{"egress-site-pool-thin/dns/below-pool-size", "egress-site-pool-thin/cdn/candidates-exhausted"}
	if !slices.Equal(keys, want) {
		t.Fatalf("alerts = %v, want %v", keys, want)
	}
}

// A site blocked in a place and not marked, a class too thin at a place, and
// a country whose exits nothing reaches are each named by place.
func TestEgressSitePoolSignalSyntheticPlaces(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.regional = [][]string{{"news-a", "site", "cn", "", "30", "29"}}
	fixture.places = [][]string{{"dns", "us", "California", "4", "120", "6"}}
	fixture.countries = [][]string{{"ir", "5", "150", "150", "40", "38"}}
	alerts, keys := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
	want := []string{
		"egress-site-regional-failure-unmarked/news-a@cn",
		"egress-site-place-pool-thin/dns@us/California",
		"egress-country-unreachable/ir",
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("alerts = %v, want %v", keys, want)
	}
	if !strings.Contains(alerts[2].Observed, "country=ir sites=5 loads=150 failed=150 runs=40 echo_failures=38") {
		t.Errorf("observed = %q", alerts[2].Observed)
	}
}

// More than half of one rank mode's answered providers borrowed is named; a
// label that is not a rank mode never becomes a frame.
func TestEgressSitePoolSignalSyntheticBackfillSustained(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.borrowed = map[string]float64{"quality": 60, "speed": 40, "other": 90}
	fixture.answered = map[string]float64{"quality": 100, "speed": 80, "other": 100}
	alerts, keys := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
	if strings.Join(keys, ",") != "egress-backfill-sustained/quality" {
		t.Fatalf("alerts = %v, want quality alone", keys)
	}
	if !strings.Contains(alerts[0].Observed, "borrowed=60 answered=100 share=0.600") {
		t.Errorf("observed = %q", alerts[0].Observed)
	}
}

// Without Mimir the backfill and guard halves say they cannot be read, and
// never that they are healthy; the database conditions still run.
func TestEgressSitePoolSignalSyntheticUnobservableMetrics(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.mimirDown = true
	fixture.task = []string{"0", "0", "0", "0"}
	alerts, keys := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
	want := []string{"egress-site-refresh-not-running/task-missing", "egress-site-pool-unobservable/metrics"}
	if !slices.Equal(keys, want) {
		t.Fatalf("alerts = %v, want %v", keys, want)
	}
	if !strings.Contains(alerts[1].Observed, "reason=bounded-source-unavailable") {
		t.Errorf("observed = %q", alerts[1].Observed)
	}

	findings, err := egressSitePoolProbe{settings: DefaultEgressSitePoolSettings(), loadPoolSettings: func() (*egressSitePoolContext, error) {
		return testEgressSitePoolContext(), nil
	}}.check(context.Background(), mustEgressSitePoolProbeEnv(t, testEgressSitePoolSettings(fixture.source(t))))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.class == "egress-backfill-sustained" {
			t.Fatalf("an unread backfill produced a finding: %+v", f)
		}
	}
}

// The probe environment a signal run would build from settings.
func mustEgressSitePoolProbeEnv(t *testing.T, settings SignalSettings) *probeEnv {
	t.Helper()
	settings = settings.withDefaults()
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// Retries more than a backoff step past their due time are named.
func TestEgressSitePoolSignalSyntheticRetryQueueStarved(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.retries = []string{"12", "3", "5400"}
	alerts, keys := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
	if strings.Join(keys, ",") != "egress-retry-queue-starved/" {
		t.Fatalf("alerts = %v, want the starved queue", keys)
	}
	if !strings.Contains(alerts[0].Observed, "retry_rows=12 overdue_rows=3 max_overdue_seconds=5400") {
		t.Errorf("observed = %q", alerts[0].Observed)
	}
}

// Every class failing above the prober-fault line at once is the prober;
// one class under it, or with too few loads to judge, is not; a guard trip
// is named by its schedule.
func TestEgressSitePoolSignalSyntheticProberFault(t *testing.T) {
	failingAll := [][]string{{"cdn", "700", "1000"}, {"connectivity", "600", "1000"}, {"dns", "700", "1000"}, {"site", "1500", "2600"}}
	oneUnder := [][]string{{"cdn", "700", "1000"}, {"connectivity", "900", "1000"}, {"dns", "700", "1000"}, {"site", "1500", "2600"}}
	oneThin := [][]string{{"cdn", "700", "1000"}, {"connectivity", "10", "50"}, {"dns", "700", "1000"}, {"site", "1500", "2600"}}
	for _, test := range []struct {
		name       string
		classLoads [][]string
		guardTrips map[string]float64
		want       []string
	}{
		{name: "every class", classLoads: failingAll, want: []string{"egress-prober-fault/failure-share"}},
		{name: "one class under", classLoads: oneUnder, want: []string{}},
		{name: "one class thin", classLoads: oneThin, want: []string{}},
		{name: "a guard trip", guardTrips: map[string]float64{"blackhole": 2, "full": 0, "other": 5}, want: []string{"egress-prober-fault/guard-blackhole"}},
	} {
		fixture := healthyEgressSitePoolFixture()
		if test.classLoads != nil {
			fixture.classLoads = test.classLoads
		}
		if test.guardTrips != nil {
			fixture.guardTrips = test.guardTrips
		}
		_, keys := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
		if !slices.Equal(keys, test.want) {
			t.Errorf("%s: alerts = %v, want %v", test.name, keys, test.want)
		}
	}
}

// A malformed answer fails the probe rather than being judged.
func TestEgressSitePoolSignalRejectsMalformedAnswers(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*egressSitePoolFixture)
	}{
		{name: "short destination row", mutate: func(f *egressSitePoolFixture) { f.destinations[0] = f.destinations[0][:8] }},
		{name: "bad active flag", mutate: func(f *egressSitePoolFixture) { f.destinations[0][2] = "maybe" }},
		{name: "bad share", mutate: func(f *egressSitePoolFixture) { f.destinations[0][7] = "NaN" }},
		{name: "bad regional count", mutate: func(f *egressSitePoolFixture) { f.regional = [][]string{{"news-a", "site", "cn", "", "x", "1"}} }},
		{name: "short task row", mutate: func(f *egressSitePoolFixture) { f.task = f.task[:3] }},
		{name: "fleet total under its ok", mutate: func(f *egressSitePoolFixture) { f.classLoads[0] = []string{"cdn", "10", "5"} }},
	} {
		fixture := healthyEgressSitePoolFixture()
		test.mutate(fixture)
		if _, err := testEgressSitePoolSignal(testEgressSitePoolContext()).Run(context.Background(), testEgressSitePoolSettings(fixture.source(t))); err == nil {
			t.Errorf("%s: a malformed answer was judged", test.name)
		}
	}
}

// The signal's queries select no provider identity: every row they return is
// a site, a class, a place or a count.
func TestEgressSitePoolQueriesReturnNoProviderIdentity(t *testing.T) {
	poolContext := testEgressSitePoolContext()
	settings := DefaultEgressSitePoolSettings()
	for _, query := range []string{
		egressSitePoolDestinationsQuery(),
		egressSitePoolRegionalQuery(settings, poolContext.siteSettings),
		egressSitePoolPlaceThinQuery(settings, poolContext.siteSettings),
		egressSitePoolUnreachableQuery(settings, poolContext.siteSettings),
		egressSitePoolTaskQuery(),
		egressSitePoolRetriesQuery(poolContext.rules),
		egressSitePoolClassLoadsQuery(poolContext.siteSettings),
	} {
		final := query[strings.LastIndex(query, "\nSELECT"):]
		if strings.Contains(final, "client_id") {
			t.Errorf("a query returns a provider identity: %s", final)
		}
	}
}

// A source that runs the signal's queries against the test's own database.
type egressSitePoolDatabaseSource struct {
	syntheticSource
}

// Implements the source's PostgreSQL read, against the test's database, with
// values rendered as psql renders text.
func (self *egressSitePoolDatabaseSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	rows := []Row{}
	var queryErr error
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, query)
		if err != nil {
			queryErr = err
			return
		}
		defer result.Close()
		for result.Next() {
			values, err := result.Values()
			if err != nil {
				queryErr = err
				return
			}
			row := make(Row, len(values))
			for i, value := range values {
				if value != nil {
					row[i] = fmt.Sprint(value)
				}
			}
			rows = append(rows, row)
		}
		queryErr = result.Err()
	})
	return rows, queryErr
}

// The production queries on a seeded database: the built-in pool with one
// site long over the retire line, one blocked in a place where the others
// pass, a place too thin for its resolvers, a country no site reaches, no
// refresh task, and no Mimir.
func TestEgressSitePoolSignalOnASeededDatabase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		if _, err := controller.EnsureProviderEgressDestinationsSeeded(ctx); err != nil {
			tb.Fatal(err)
		}
		now := server.NowUtc()
		overSince := now.Add(-30 * time.Hour)
		share := 0.7
		resolvers := 0
		for _, d := range model.GetProviderEgressDestinations(ctx) {
			switch {
			case d.Name == "cnn":
				d.FailureShare = &share
				d.SampleCount = 250
				d.AboveRetireSince = &overSince
				model.SetProviderEgressDestination(ctx, d)
			case d.Class == "dns" && resolvers < 3:
				resolvers++
				d.Incompatible = []model.ProviderEgressDestinationPlace{{Country: "ir"}}
				model.SetProviderEgressDestination(ctx, d)
			}
		}

		day := now.UTC().Truncate(24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			siteTally := func(name string, country string, loads int, failures int) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO provider_egress_site_tally (
						tally_day, name, country_code, region,
						load_count, failure_count, healthy_load_count, healthy_failure_count, update_time
					)
					VALUES ($1, $2, $3, '', $4, $5, $4, $5, $6)
					`,
					day, name, country, loads, failures, now,
				))
			}
			placeTally := func(country string, runs int, echoFailures int) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO provider_egress_place_tally (
						tally_day, country_code, region, run_count, healthy_run_count, echo_failure_count, update_time
					)
					VALUES ($1, $2, '', $3, $3, $4, $5)
					`,
					day, country, runs, echoFailures, now,
				))
			}
			siteTally("bbc", "de", 30, 29)
			for _, name := range []string{"reuters", "ap-news", "the-guardian"} {
				siteTally(name, "de", 30, 0)
			}
			for _, name := range []string{"reuters", "ap-news"} {
				siteTally(name, "kp", 40, 40)
			}
			placeTally("de", 30, 0)
			placeTally("ir", 30, 0)
			placeTally("kp", 40, 40)
		})

		poolContext := &egressSitePoolContext{
			siteSettings:         model.DefaultProviderEgressSiteSettings(),
			rules:                model.DefaultProviderEgressRules(),
			candidateNameClasses: map[string]string{"dns-candidate": "dns", "connectivity-candidate": "connectivity", "cdn-candidate": "cdn", "site-candidate": "site"},
		}
		settings := syntheticSettings(&egressSitePoolDatabaseSource{})
		settings.Now = func() time.Time { return now }
		alerts, err := testEgressSitePoolSignal(poolContext).Run(ctx, settings)
		if err != nil {
			tb.Fatal(err)
		}
		want := []string{
			"egress-site-pool-needs-refresh/cnn",
			"egress-site-refresh-not-running/task-missing",
			"egress-site-regional-failure-unmarked/bbc@de",
			"egress-site-place-pool-thin/dns@ir",
			"egress-country-unreachable/kp",
			"egress-site-pool-unobservable/metrics",
		}
		if got := testEgressSitePoolAlertKeys(alerts); strings.Join(got, ",") != strings.Join(want, ",") {
			tb.Fatalf("alerts = %v, want %v", got, want)
		}
	})
}
