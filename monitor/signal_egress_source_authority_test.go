// Evidence-boundary controls for site-pool, derive readiness and no-exit-IP
// findings. The fixtures contain only invented, count-only observations.
package monitor

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Supplies a due-age column only when the production query requests it. This
// lets the old query reach the old reducer in the causal RED control.
func derivedSourceWithTaskOverdue(t *testing.T, fixture *derivedLocationsFixture, overdue string) *syntheticSource {
	t.Helper()
	source := fixture.source(t)
	postgres := source.postgresFn
	source.postgresFn = func(query string) ([]Row, error) {
		rows, err := postgres(query)
		if err == nil && strings.Contains(query, "monitor-signal-2.19c-derived-locations-state") && strings.Contains(query, "task_overdue_seconds") {
			if len(rows) != 1 || (len(rows[0]) != 5 && len(rows[0]) != 6) {
				t.Fatal("unexpected synthetic derive state shape")
			}
			if len(rows[0]) == 5 {
				rows[0] = append(rows[0], overdue)
			} else {
				rows[0][5] = overdue
			}
		}
		return rows, err
	}
	return source
}

// Runs the real probe, retaining healthy sentinels as well as violations.
func derivedAuthorityFindings(t *testing.T, fixture *derivedLocationsFixture, overdue string) []finding {
	t.Helper()
	settings := syntheticSettings(derivedSourceWithTaskOverdue(t, fixture, overdue)).withDefaults()
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (derivedLocationsProbe{settings: DefaultDerivedLocationsSettings()}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// No visible completion plus a task far past run_at is not indefinite health.
func TestDerivedLocationsAuthorityOverdueFirstRun(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.runs = nil
	fixture.table = derivedLocationsTable{medianResidualKm: -1, medianReputation: -1, lastUpdateAgeSeconds: -1}
	settings := DefaultDerivedLocationsSettings()
	overdue := strconv.FormatInt(int64(settings.DeriveMaxRunAge/time.Second)+1, 10)
	alerts, err := NewDerivedLocationsSignalWithSettings(settings).Run(context.Background(), syntheticSettings(derivedSourceWithTaskOverdue(t, fixture, overdue)))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "derive-not-running")
	if alert.Frame != "first-run-overdue" || !strings.Contains(alert.Observed, "task_overdue_seconds="+overdue) {
		t.Fatalf("missing bounded first-completion deadline: %+v", alert)
	}
	for _, phrase := range []string{"run_at", "rescheduling", "does not prove", "claim"} {
		if !strings.Contains(alert.Markdown(), phrase) {
			t.Fatalf("overdue finding omitted authority limit %q", phrase)
		}
	}
}

// A future first run is expected warming, not a failure and not a healthy
// sentinel capable of clearing an older not-running ticket.
func TestDerivedLocationsAuthorityFirstRunWarming(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.runs = nil
	fixture.table = derivedLocationsTable{medianResidualKm: -1, medianReputation: -1, lastUpdateAgeSeconds: -1}
	for _, f := range derivedAuthorityFindings(t, fixture, "-3600") {
		if f.class == "derive-not-running" {
			t.Fatalf("uncompleted first run was judged instead of warming: healthy=%t frame=%s", f.healthy, f.frame)
		}
	}
}

// Unknown first-run completion cannot resolve an old incident; a later
// recorded completion can, through the unchanged ticket manager.
func TestDerivedLocationsAuthorityWarmingPreservesTicket(t *testing.T) {
	ctx := context.Background()
	manager := newTicketManager("synthetic", &ticketEscalationEmitter{})
	manager.resolveTicks = 3
	stale := healthyDerivedLocationsFixture()
	stale.runs[0] = derivedLocationsRun(17*time.Hour, 400)
	stale.runs = stale.runs[:1]
	for range 2 {
		manager.ingest(ctx, derivedAuthorityFindings(t, stale, "3600"))
	}
	if manager.openCount() != 1 {
		t.Fatalf("setup opened %d tickets, want one stale derivation", manager.openCount())
	}
	warming := healthyDerivedLocationsFixture()
	warming.runs = nil
	warming.table = derivedLocationsTable{medianResidualKm: -1, medianReputation: -1, lastUpdateAgeSeconds: -1}
	for range manager.resolveTicks {
		manager.ingest(ctx, derivedAuthorityFindings(t, warming, "-3600"))
	}
	if manager.openCount() != 1 {
		t.Fatal("an uncompleted first run falsely resolved a prior not-running ticket")
	}
	for range manager.resolveTicks {
		manager.ingest(ctx, derivedAuthorityFindings(t, healthyDerivedLocationsFixture(), "-3600"))
	}
	if manager.openCount() != 0 {
		t.Fatal("a recorded fresh derivation did not resolve the ticket")
	}
}

// The exact threshold is not a violation. Without a completion it still does
// not certify health; the next second is covered by the overdue control.
func TestDerivedLocationsAuthorityFirstRunThresholdBoundary(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.runs = nil
	fixture.table = derivedLocationsTable{medianResidualKm: -1, medianReputation: -1, lastUpdateAgeSeconds: -1}
	overdue := strconv.FormatInt(int64(DefaultDerivedLocationsSettings().DeriveMaxRunAge/time.Second), 10)
	for _, f := range derivedAuthorityFindings(t, fixture, overdue) {
		if f.class == "derive-not-running" {
			t.Fatalf("threshold equality was judged without a completed run: healthy=%t frame=%s", f.healthy, f.frame)
		}
	}
}

// A readable fresh table remains the existing activity control if the run
// history is unavailable; it does not erase that separate visibility warning.
func TestDerivedLocationsAuthorityFreshTableControl(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.redisErr = errors.New("synthetic history unavailable")
	healthy, visibility := false, false
	for _, f := range derivedAuthorityFindings(t, fixture, "90000") {
		if f.class == "derive-not-running" && f.healthy {
			healthy = true
		}
		if f.class == "derive-run-history-unobservable" && !f.healthy {
			visibility = true
		}
	}
	if !healthy || !visibility {
		t.Fatal("fresh table activity or the independent history gap was lost")
	}
}

// A malformed new age is an observation error, never a zero or healthy run.
func TestDerivedLocationsAuthorityMalformedDueAge(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	_, err := NewDerivedLocationsSignal().Run(context.Background(), syntheticSettings(derivedSourceWithTaskOverdue(t, fixture, "not-an-age")))
	if err == nil {
		t.Fatal("malformed task due age was not rejected")
	}
}

// The extra age comes from the exact function's current run_at in the same
// database statement, not a manufactured creation time or another task.
func TestDerivedLocationsAuthorityCurrentScheduleSql(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		for _, overdueSeconds := range []int{61200, -3600} {
			query := derivedLocationsStateQuery(DefaultDerivedLocationsSettings())
			prefix := `WITH pending_task(function_name, reschedule_error_count, run_at) AS (
 VALUES ('` + deriveLocationsTaskFunction + `', 0,
         (now() AT TIME ZONE 'utc') - interval '` + strconv.Itoa(overdueSeconds) + ` seconds'),
        ('synthetic.unrelated.task', 99,
         (now() AT TIME ZONE 'utc') - interval '100 hours')
)
SELECT
 (`
			query = strings.Replace(query, "SELECT\n (", prefix, 1)
			rows, err := (&egressSitePoolDatabaseSource{}).PostgreSQL(context.Background(), query)
			if err != nil {
				tb.Fatal(err)
			}
			if len(rows) != 1 || len(rows[0]) != 6 || rows[0][0] != "1" || rows[0][1] != "0" || rows[0][5] != strconv.Itoa(overdueSeconds) {
				tb.Fatalf("current schedule age was not isolated: rows=%v due_age=%d", rows, overdueSeconds)
			}
		}
	})
}

// A stored error has no heartbeat or task-phase authority in this query.
func TestDerivedLocationsAuthorityRetryIsNotParked(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.state.failingTaskRows = 1
	fixture.state.maxRescheduleCount = 4
	alert := requireAlertClass(t, fixture.run(t, nil), "derive-not-running")
	if alert.Frame != "task-parked" {
		t.Fatal("legacy ticket identity changed")
	}
	if strings.Contains(alert.Symptom, "parked") || !strings.Contains(alert.Observed, "execution_state=unobserved") || !strings.Contains(alert.Context, "RunPost") {
		t.Fatalf("retry finding overstates execution state: %s", alert.Markdown())
	}
}

// Site refresh retry state has the same authority limit as derive retry state.
func TestEgressSitePoolAuthorityRetryIsNotParked(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.task = []string{"1", "1", "4", "-60"}
	alerts, _ := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
	alert := requireAlertClass(t, alerts, "egress-site-refresh-not-running")
	if alert.Frame != "task-parked" || strings.Contains(alert.Symptom, "parked") || !strings.Contains(alert.Observed, "execution_state=unobserved") || !strings.Contains(alert.Context, "RunPost") {
		t.Fatalf("refresh retry finding overstates execution state: %s", alert.Markdown())
	}
}

// A late run_at is not an observed last completion or evidence that no worker
// currently holds the task. Preserve the schedule warning, not those causes.
func TestEgressSitePoolAuthorityOverdueIsNotLastRun(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	poolContext := testEgressSitePoolContext()
	fixture.task = []string{"1", "0", "0", strconv.Itoa(poolContext.siteSettings.SiteRefreshIntervalSeconds + 1)}
	alerts, _ := runEgressSitePoolFixture(t, fixture, poolContext)
	alert := requireAlertClass(t, alerts, "egress-site-refresh-not-running")
	if alert.Frame != "stale-run" || !strings.Contains(alert.Observed, "schedule_anchor=run_at") || !strings.Contains(alert.Observed, "execution_state=unobserved") {
		t.Fatalf("schedule warning lost its exact anchor: %s", alert.Markdown())
	}
	for _, rejected := range []string{"last ran more than", "last run is more than", "task is not being claimed"} {
		if strings.Contains(alert.Markdown(), rejected) {
			t.Fatalf("schedule age was promoted to an execution claim: %q", rejected)
		}
	}
}

// A failed observed subset must remain visible without being called the full
// active pool or proving which shared component caused the failures.
func TestEgressSitePoolAuthorityObservedCountrySubset(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.countries = [][]string{{"zz", "1", "40", "40", "40", "0"}}
	alerts, _ := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
	alert := requireAlertClass(t, alerts, "egress-country-unreachable")
	for _, phrase := range []string{"observed active", "coverage=observed-sites-only", "unobserved", "does not isolate"} {
		if !strings.Contains(alert.Markdown(), phrase) {
			t.Fatalf("country subset omitted %q: %s", phrase, alert.Markdown())
		}
	}
	for _, rejected := range []string{"Every active site", "it is not the sites", "never answer this by marking"} {
		if strings.Contains(alert.Markdown(), rejected) {
			t.Fatalf("country subset retains unsupported claim %q", rejected)
		}
	}
}

// The actual SQL returns one failing site even when a second active site has
// no country sample. The public finding must preserve that exact limitation.
func TestEgressSitePoolAuthorityCountrySqlSubset(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		settings := DefaultEgressSitePoolSettings()
		poolContext := testEgressSitePoolContext()
		query := egressSitePoolUnreachableQuery(settings, poolContext.siteSettings)
		query = strings.Replace(query, "WITH site_country AS (", `WITH
provider_egress_destination(name, active, probation) AS (
 VALUES ('observed.example', true, false), ('unobserved.example', true, false)
), provider_egress_site_tally(name, country_code, tally_day, load_count, failure_count) AS (
 VALUES ('observed.example', 'zz', current_date, 40, 40)
), provider_egress_place_tally(country_code, tally_day, run_count, echo_failure_count) AS (
 VALUES ('zz', current_date, 40, 0)
), site_country AS (`, 1)
		rows, err := (&egressSitePoolDatabaseSource{}).PostgreSQL(context.Background(), query)
		if err != nil {
			tb.Fatal(err)
		}
		if len(rows) != 1 || len(rows[0]) != 6 || rows[0][0] != "zz" || rows[0][1] != "1" {
			tb.Fatalf("unexpected synthetic SQL subset: %v", rows)
		}
		fixture := healthyEgressSitePoolFixture()
		fixture.countries = [][]string{[]string(rows[0])}
		alerts, err := testEgressSitePoolSignal(poolContext).Run(context.Background(), testEgressSitePoolSettings(fixture.source(t)))
		if err != nil {
			tb.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "egress-country-unreachable")
		if !strings.Contains(alert.Observed, "coverage=observed-sites-only") {
			tb.Fatal("actual SQL subset was rendered as complete country-site coverage")
		}
	})
}

// A common class is a same-stage observation, not proof that the echo is the
// cause or that provider-specific admission and routes are healthy.
func TestEgressOutcomesAuthorityNoExitIpCauseUnknown(t *testing.T) {
	alerts := runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 20, observed: 20, successes: 1, failures: 19,
		noExitIp: 19, newestOutcomeAgeSeconds: 30, oldestOutcomeAgeSeconds: 600,
	})
	alert := requireAlertClass(t, alerts, "egress-common-mode")
	for _, phrase := range []string{"does not isolate", "/ip echo", "provider", "same-attempt"} {
		if !strings.Contains(alert.Markdown(), phrase) {
			t.Fatalf("no-exit-IP finding omitted %q: %s", phrase, alert.Markdown())
		}
	}
	for _, rejected := range []string{"not the providers", "therefore the shared", "before attribution is allowed", "After repairing the proved shared boundary"} {
		if strings.Contains(alert.Markdown(), rejected) {
			t.Fatalf("no-exit-IP finding retains unsupported cause %q", rejected)
		}
	}
}

// The owning catalog must preserve the same observation-versus-cause and
// unknown-versus-healthy boundary as the registered probes.
func TestEgressSourceAuthorityCatalog(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range []struct {
		section string
		phrases []string
	}{
		{section: "2.19b", phrases: []string{"observed active scored sites", "Coverage is observed-sites-only", "execution state is unobserved", "RunPost", "current run_at"}},
		{section: "2.19c", phrases: []string{"first-run-overdue", "no healthy derive-not-running sentinel", "current run_at", "rescheduling can mask", "RunPost"}},
		{section: "2.23", phrases: []string{"usable exit-IP observation", "does not isolate", "same-attempt"}},
	} {
		catalog := string(data)
		start := strings.Index(catalog, "### "+contract.section+" ")
		if start < 0 {
			t.Fatalf("catalog section %s missing", contract.section)
		}
		section := catalog[start:]
		if end := strings.Index(section, "\n### "); end >= 0 {
			section = section[:end]
		}
		section = strings.Join(strings.Fields(strings.ReplaceAll(section, "`", "")), " ")
		for _, phrase := range contract.phrases {
			if !strings.Contains(section, phrase) {
				t.Errorf("catalog section %s omits %q", contract.section, phrase)
			}
		}
	}
}
