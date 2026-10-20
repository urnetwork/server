package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// These public-path fixtures also compile on the pre-fix source. JSON ignores
// the then-unknown count field, letting the missing-census regression reach
// its causal assertion instead of failing to compile.
func pgbouncerStarvationSettings(t testing.TB, now time.Time, source SignalSource, count int) SignalSettings {
	t.Helper()
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.PostgreSQL.Database = "app"
	settings.Hosts = []HostSettings{
		{Name: "db-1", Roles: []string{"pg-primary"}},
		{Name: "metrics-1", Roles: []string{"services"}},
	}
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"PgBouncerInstances":%d}`, count)), &settings.Hosts[0]); err != nil {
		t.Fatal("could not configure synthetic instance count")
	}
	return settings
}

// A source returns only families requested by the actual public query. The
// old query has no per-gauge source clocks; omit those rows only for that
// query so duplicate/privacy regressions fail at their own root cause.
func pgbouncerStarvationResponseForQuery(t testing.TB, payload string, command string) string {
	t.Helper()
	if strings.Contains(command, "timestamp%28pgbouncer_pool_") {
		return payload
	}
	var response mimirInstantResponse
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &envelope) != nil || json.Unmarshal(envelope["data"], &response.Data) != nil {
		t.Fatal("invalid synthetic payload")
	}
	rows := response.Data.Result[:0]
	for _, row := range response.Data.Result {
		if !strings.HasSuffix(row.Metric["monitor_metric"], "_sample_time") {
			rows = append(rows, row)
		}
	}
	response.Data.Result = rows
	data, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal("could not encode synthetic data")
	}
	envelope["data"] = data
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal("could not encode synthetic response")
	}
	return string(encoded)
}

// Mutates synthetic wire rows before the registered signal reads them.
func pgbouncerStarvationEditedPayload(t testing.TB, now time.Time, fixtures []pgbouncerStarvationFixture, edit func(*mimirInstantResponse)) string {
	t.Helper()
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(pgbouncerStarvationFixtureJson(t, now, fixtures)), &response); err != nil {
		t.Fatal("could not decode synthetic fixture")
	}
	edit(&response)
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal("could not encode edited fixture")
	}
	return string(payload)
}

// Runs the actual registered Signal with an explicit independent census.
// The result variant lets warning controls identify a production decode error.
func pgbouncerStarvationOwnedPayloadResult(t *testing.T, now time.Time, payload string, count int) (Alerts, error) {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" {
			return "", fmt.Errorf("unexpected synthetic gateway")
		}
		return pgbouncerStarvationResponseForQuery(t, payload, command), nil
	}}
	settings := pgbouncerStarvationSettings(t, now, source, count)
	return pgbouncerPoolStarvationSignalForTest(t).Run(context.Background(), settings)
}

func runPgBouncerStarvationOwnedPayload(t *testing.T, now time.Time, payload string, count int) Alerts {
	t.Helper()
	alerts, err := pgbouncerStarvationOwnedPayloadResult(t, now, payload, count)
	if err != nil {
		t.Fatal("public pool signal failed on synthetic response")
	}
	return alerts
}

// All public strings, including incident identity and serialized output, must
// omit private source labels, even on visibility findings.
func requirePgBouncerStarvationPrivate(t testing.TB, alerts Alerts, canaries ...string) {
	t.Helper()
	for _, alert := range alerts {
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal("could not serialize public alert")
		}
		for _, canary := range canaries {
			if strings.Contains(string(encoded), canary) || strings.Contains(alert.Markdown(), canary) || strings.Contains(alert.Identity(), canary) {
				t.Fatal("public pool alert exposed a private source label")
			}
		}
	}
}

// Competing rows cannot choose a measured state through input ordering.
func TestPgBouncerPoolStarvationAuthorityConflictingPublishersAreOrderIndependent(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, reverse := range []bool{false, true} {
		payload := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)}, func(response *mimirInstantResponse) {
			for _, row := range slices.Clone(response.Data.Result) {
				if row.Metric["monitor_metric"] == "servers" {
					row.Metric = maps.Clone(row.Metric)
					row.Metric["publisher"] = "private-publisher-canary"
					row.Value = []json.RawMessage{json.RawMessage(fmt.Sprint(now.Unix())), json.RawMessage(`"20"`)}
					response.Data.Result = append(response.Data.Result, row)
				}
			}
			if reverse {
				slices.Reverse(response.Data.Result)
			}
		})
		alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, 2)
		if requireAlertClassCount(alerts, "pgbouncer-pools-starved") != 0 {
			t.Fatal("ambiguous publishers manufactured a fleet page")
		}
		unknown := requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
		if !unknown.unobservable || !strings.Contains(unknown.Observed, "ambiguous_publisher") {
			t.Fatal("conflicting publishers did not retain unknown authority")
		}
		requirePgBouncerStarvationPrivate(t, alerts, "private-publisher-canary")
	}
}

// Gauges split across sources cannot manufacture a complete starved tuple.
func TestPgBouncerPoolStarvationAuthorityMixedPublisherCannotInventPage(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	payload := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)}, func(response *mimirInstantResponse) {
		for index := range response.Data.Result {
			if response.Data.Result[index].Metric["monitor_metric"] == "servers" {
				response.Data.Result[index].Metric["publisher"] = "other-source.example"
			}
		}
	})
	alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, 2)
	if requireAlertClassCount(alerts, "pgbouncer-pools-starved") != 0 {
		t.Fatal("cross-publisher gauge join manufactured a fleet page")
	}
	requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
}

// Equal duplicates remain ambiguous, including console authority and clocks.
func TestPgBouncerPoolStarvationAuthorityDuplicateFieldsNeverChooseAWinner(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, metric := range []string{"up", "sample_time", "waiting", "servers", "logins", "max_wait", "starved_seconds", "servers_sample_time"} {
		for _, reverse := range []bool{false, true} {
			payload := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1)}, func(response *mimirInstantResponse) {
				for _, row := range slices.Clone(response.Data.Result) {
					if row.Metric["monitor_metric"] == metric {
						response.Data.Result = append(response.Data.Result, row)
					}
				}
				if reverse {
					slices.Reverse(response.Data.Result)
				}
			})
			unknown := requireAlertClass(t, runPgBouncerStarvationOwnedPayload(t, now, payload, 1), "pgbouncer-pools-unobservable")
			if !strings.Contains(unknown.Observed, "duplicate_metric") {
				t.Fatalf("duplicate %s did not remain unknown", metric)
			}
		}
	}
}

// No malformed or incomplete unrelated row erases the independently proven
// lower bound of two old starved pools.
func TestPgBouncerPoolStarvationAuthorityRetainsPageBesidePartialEvidence(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, fault := range []string{"nan", "negative", "fractional", "future", "stale", "host", "index", "database", "metric", "missing_gauge", "foreign_database"} {
		for _, reverse := range []bool{false, true} {
			payload := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2), pgbouncerStarvationHealthy(3)}, func(response *mimirInstantResponse) {
				for index := range response.Data.Result {
					row := &response.Data.Result[index]
					if row.Metric["pgbouncer"] != "3" || row.Metric["monitor_metric"] != "servers" {
						continue
					}
					switch fault {
					case "nan":
						row.Value[1] = json.RawMessage(`"NaN"`)
					case "negative":
						row.Value[1] = json.RawMessage(`"-1"`)
					case "fractional":
						row.Value[1] = json.RawMessage(`"0.5"`)
					case "future":
						row.Value[0] = json.RawMessage(fmt.Sprint(now.Add(time.Minute).Unix()))
					case "stale":
						row.Value[0] = json.RawMessage(fmt.Sprint(now.Add(-3 * time.Minute).Unix()))
					case "host", "database":
						delete(row.Metric, fault)
					case "index":
						delete(row.Metric, "pgbouncer")
					case "metric":
						row.Metric["monitor_metric"] = "unrecognized"
					case "missing_gauge":
						response.Data.Result = append(response.Data.Result[:index], response.Data.Result[index+1:]...)
					case "foreign_database":
						row.Metric["database"] = "private-unowned-database-canary"
					}
					break
				}
				if reverse {
					slices.Reverse(response.Data.Result)
				}
			})
			alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, 3)
			page := requireAlertClass(t, alerts, "pgbouncer-pools-starved")
			if page.Severity != SeverityPage || page.Sustain != 1 || !strings.Contains(page.Observed, "old_starved_pools=2") {
				t.Fatalf("%s changed the independent page", fault)
			}
			requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
			requirePgBouncerStarvationPrivate(t, alerts, "private-unowned-database-canary")
		}
	}
}

// An unrelated publisher collision cannot poison other instance authorities.
func TestPgBouncerPoolStarvationAuthorityAmbiguousInstanceDoesNotEraseIndependentPage(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	payload := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2), pgbouncerStarvationHealthy(3)}, func(response *mimirInstantResponse) {
		for _, row := range slices.Clone(response.Data.Result) {
			if row.Metric["pgbouncer"] == "3" {
				row.Metric = maps.Clone(row.Metric)
				row.Metric["publisher"] = "second-publisher.example"
				response.Data.Result = append(response.Data.Result, row)
			}
		}
	})
	alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, 3)
	page := requireAlertClass(t, alerts, "pgbouncer-pools-starved")
	if !strings.Contains(page.Observed, "old_starved_pools=2") {
		t.Fatal("ambiguous third instance contaminated the page denominator")
	}
	requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
}

// Foreign databases cannot multiply pool cardinality, nor hide the authorized
// database of the same instance when another instance proves the fleet page.
func TestPgBouncerPoolStarvationAuthorityDatabaseCannotManufacturePools(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, count := range []int{1, 2} {
		fixtures := []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1)}
		if count == 2 {
			fixtures = append(fixtures, pgbouncerStarvationLivelocked(2))
		}
		payload := pgbouncerStarvationEditedPayload(t, now, fixtures, func(response *mimirInstantResponse) {
			for _, row := range slices.Clone(response.Data.Result) {
				if row.Metric["pgbouncer"] == "1" && row.Metric["database"] != "" {
					row.Metric = maps.Clone(row.Metric)
					row.Metric["database"] = "private-forged-database-canary"
					response.Data.Result = append(response.Data.Result, row)
				}
			}
		})
		alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, count)
		if count == 1 && requireAlertClassCount(alerts, "pgbouncer-pools-starved") != 0 {
			t.Fatal("an arbitrary database label manufactured a second starved pool")
		}
		if count == 2 {
			page := requireAlertClass(t, alerts, "pgbouncer-pools-starved")
			if !strings.Contains(page.Observed, "old_starved_pools=2") {
				t.Fatal("foreign database rows changed the authorized page count")
			}
		}
		requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
		requirePgBouncerStarvationPrivate(t, alerts, "private-forged-database-canary")
	}
}

// Even the configured database is a private join key, never public prose.
func TestPgBouncerPoolStarvationAuthorityConfiguredDatabaseRemainsPrivate(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	const database = "private-configured-database-canary"
	payload := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1)}, func(response *mimirInstantResponse) {
		for index := range response.Data.Result {
			if response.Data.Result[index].Metric["database"] != "" {
				response.Data.Result[index].Metric["database"] = database
			}
			response.Data.Result[index].Metric["publisher"] = "private-valid-publisher-canary"
		}
	})
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		return pgbouncerStarvationResponseForQuery(t, payload, command), nil
	}}
	settings := pgbouncerStarvationSettings(t, now, source, 1)
	settings.PostgreSQL.Database = database
	alerts, err := pgbouncerPoolStarvationSignalForTest(t).Run(context.Background(), settings)
	if err != nil {
		t.Fatal("configured database fixture failed")
	}
	shard := requireAlertClass(t, alerts, "pgbouncer-pool-starved-shard")
	if shard.Target != "synthetic/db-1/pgbouncer@1" || shard.PageSustain != 5 || shard.Sustain != 2 {
		t.Fatal("privacy correction changed the stable shard identity or cadence")
	}
	requirePgBouncerStarvationPrivate(t, alerts, database, "private-valid-publisher-canary")
}

// Public identities come only from the admitted inventory host and range.
func TestPgBouncerPoolStarvationAuthorityRejectsForgedIdentities(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, c := range []struct{ label, value string }{
		{label: "host", value: "private-unowned-host-canary"},
		{label: "pgbouncer", value: "private-index-canary"},
		{label: "pgbouncer", value: "01"},
		{label: "pgbouncer", value: "-1"},
		{label: "pgbouncer", value: "33"},
		{label: "pgbouncer", value: strings.Repeat("9", 1024)},
		{label: "env", value: "private-wrong-env-canary"},
		{label: "job", value: "private-wrong-job-canary"},
	} {
		payload := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1)}, func(response *mimirInstantResponse) {
			for index := range response.Data.Result {
				response.Data.Result[index].Metric[c.label] = c.value
			}
		})
		alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, 32)
		if requireAlertClassCount(alerts, "pgbouncer-pool-starved-shard") != 0 || requireAlertClassCount(alerts, "pgbouncer-pools-starved") != 0 {
			t.Fatal("unadmitted source identity became a starvation incident")
		}
		requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
		if strings.Contains(c.value, "canary") || len(c.value) > 100 {
			requirePgBouncerStarvationPrivate(t, alerts, c.value)
		}
	}
}

// A current query timestamp does not refresh an older gauge scrape.
func TestPgBouncerPoolStarvationAuthorityRequiresOneScrapeEpoch(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, age := range []time.Duration{15 * time.Second, 3 * time.Minute, -time.Minute} {
		payload := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)}, func(response *mimirInstantResponse) {
			for index := range response.Data.Result {
				row := &response.Data.Result[index]
				if row.Metric["monitor_metric"] == "servers_sample_time" {
					row.Value[1] = json.RawMessage(fmt.Sprintf("%q", fmt.Sprint(now.Add(-age).Unix())))
				}
			}
		})
		alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, 2)
		if requireAlertClassCount(alerts, "pgbouncer-pools-starved") != 0 {
			t.Fatal("gauges from different scrape epochs manufactured a page")
		}
		unknown := requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
		if !strings.Contains(unknown.Observed, "incoherent_sample_time") {
			t.Fatal("mixed source clocks did not retain a structural visibility reason")
		}
	}
}

// The existing freshness and future-skew boundaries stay inclusive.
func TestPgBouncerPoolStarvationAuthorityFreshnessBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		age  time.Duration
		page bool
	}{
		{age: 108 * time.Second, page: true},
		{age: 108*time.Second + time.Millisecond, page: false},
		{age: -42 * time.Second, page: true},
		{age: -42*time.Second - time.Millisecond, page: false},
	} {
		fixtures := []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)}
		for index := range fixtures {
			fixtures[index].sampleAge = c.age
		}
		alerts := runPgBouncerStarvationOwnedPayload(t, now, pgbouncerStarvationFixtureJson(t, now, fixtures), 2)
		if (requireAlertClassCount(alerts, "pgbouncer-pools-starved") == 1) != c.page {
			t.Fatal("source freshness boundary changed")
		}
		if !c.page {
			requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
		}
	}
}

// The expected set comes from configuration, never surviving metric rows.
func TestPgBouncerPoolStarvationAuthorityRequiresCompleteConfiguredCensus(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, count := range []int{0, -1, 32, int(^uint(0) >> 1)} {
		payload := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1)})
		unknown := requireAlertClass(t, runPgBouncerStarvationOwnedPayload(t, now, payload, count), "pgbouncer-pools-unobservable")
		if !unknown.unobservable {
			t.Fatal("incomplete or unconfigured census was treated as healthy")
		}
	}
	payload := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)})
	for _, count := range []int{0, -1, 32} {
		alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, count)
		requireAlertClass(t, alerts, "pgbouncer-pools-starved")
		requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
	}
}

// Source warnings withhold recovery but cannot erase an independent page.
func TestPgBouncerPoolStarvationAuthorityWarningsPreservePositiveEvidence(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, starved := range []bool{false, true} {
		fixtures := []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1), pgbouncerStarvationHealthy(2)}
		if starved {
			fixtures = []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)}
		}
		payload := strings.Replace(pgbouncerStarvationFixtureJson(t, now, fixtures), `"status":"success"`, `"status":"success","warnings":["private-query-warning-canary"]`, 1)
		alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, 2)
		unknown := requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
		if !unknown.unobservable || !strings.Contains(unknown.Observed, "query_warnings") {
			t.Fatal("query warnings supplied complete recovery authority")
		}
		if starved {
			requireAlertClass(t, alerts, "pgbouncer-pools-starved")
		}
		requirePgBouncerStarvationPrivate(t, alerts, "private-query-warning-canary")
	}
}

// The corrected, unique and fully covered healthy cohort really clears.
func TestPgBouncerPoolStarvationAuthorityCompleteHealthyControlIsQuiet(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	payload := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1), pgbouncerStarvationHealthy(2)})
	requireNoPgBouncerStarvationAlerts(t, runPgBouncerStarvationOwnedPayload(t, now, payload, 2))
}

// The exact public signal feeds the real cadence gate and notifier state
// reducer. Explicit run steps prove that held visibility cannot clear a PAGE
// and that two complete healthy runs can, without network or elapsed sleeps.
func TestPgBouncerPoolStarvationAuthorityNotifierKeepsPageUntilCompleteRecovery(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	starved := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)})
	healthy := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1), pgbouncerStarvationHealthy(2)})
	missing := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1)})
	warning := strings.Replace(healthy, `"status":"success"`, `"status":"success","warnings":["private-notifier-warning-canary"]`, 1)
	duplicate := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1), pgbouncerStarvationHealthy(2)}, func(response *mimirInstantResponse) {
		response.Data.Result = append(response.Data.Result, response.Data.Result[0])
	})
	for _, partial := range []string{missing, warning, duplicate} {
		payload := starved
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			return pgbouncerStarvationResponseForQuery(t, payload, command), nil
		}}
		settings := pgbouncerStarvationSettings(t, now, source, 2)
		signal := pgbouncerPoolStarvationSignalForTest(t)
		gate := newCadenceAlertGate()
		notifier := &SlackAlertNotifier{
			settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
			keyIncidents:          map[string]*slackIncident{},
			signalKeyCompleteRuns: map[string]int{},
		}
		for run, current := range []string{starved, partial, partial, healthy, healthy} {
			payload = current
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal("notifier fixture public signal failed")
			}
			ready, held := gate.split(signal, alerts)
			transitions := notifier.observeWithLock(now.Add(time.Duration(run)*time.Minute), signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
			pageTransitions := []slackTransition{}
			for _, transition := range transitions {
				if transition.alert.Class == "pgbouncer-pools-starved" {
					pageTransitions = append(pageTransitions, transition)
				}
			}
			if run == 0 {
				if len(pageTransitions) != 1 || pageTransitions[0].kind != slackRaised {
					t.Fatal("initial independent page was not raised")
				}
			} else if run < 4 {
				if len(pageTransitions) != 0 {
					t.Fatal("visibility gap or one healthy sample falsely cleared a page")
				}
			} else if len(pageTransitions) != 1 || pageTransitions[0].kind != slackCleared {
				t.Fatal("two authoritative healthy runs did not clear the page")
			}
			requirePgBouncerStarvationPrivate(t, alerts, "private-notifier-warning-canary")
		}
	}
}

// Source timestamps must be requested rather than inferred from evaluation.
func TestPgBouncerPoolStarvationAuthorityQueryRequestsEveryGaugeClock(t *testing.T) {
	query := pgbouncerPoolStarvationQuery("synthetic")
	for _, metric := range pgbouncerStarvationPoolMetrics {
		if !strings.Contains(query, "timestamp("+metric+`{env="synthetic",job="pgbouncer"})`) {
			t.Fatal("pool gauge lacks a source timestamp in the public query")
		}
	}
	if pgbouncerPoolStarvationSignalForTest(t).Cadence() != time.Minute {
		t.Fatal("pool authority correction changed the observation cadence")
	}
}

// Nonnegative finite gauges are not sufficient authority for integer counts.
func TestPgBouncerPoolStarvationAuthorityRejectsMalformedCountsAndClocks(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, c := range []struct{ metric, value string }{
		{metric: "waiting", value: "0.5"},
		{metric: "servers", value: "-1"},
		{metric: "logins", value: "0.5"},
		{metric: "waiting", value: "9007199254740992"},
		{metric: "up", value: "2"},
		{metric: "up", value: "NaN"},
		{metric: "max_wait", value: "+Inf"},
		{metric: "sample_time", value: "1e300"},
		{metric: "sample_time", value: "0"},
		{metric: "servers_sample_time", value: "NaN"},
	} {
		payload := pgbouncerStarvationEditedPayload(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)}, func(response *mimirInstantResponse) {
			for index := range response.Data.Result {
				row := &response.Data.Result[index]
				if row.Metric["monitor_metric"] == c.metric {
					row.Value[1] = json.RawMessage(fmt.Sprintf("%q", c.value))
				}
			}
		})
		alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, 2)
		if requireAlertClassCount(alerts, "pgbouncer-pools-starved") != 0 {
			t.Fatalf("malformed %s supplied page authority", c.metric)
		}
		requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
	}
}

// A partial envelope has no recovery authority even if its warning field is
// malformed; independently complete tuples remain a useful lower bound.
func TestPgBouncerPoolStarvationAuthorityWarningShapesRemainPrivateAndScoped(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		value      string
		unknown    bool
		decodeKind string
	}{
		{value: "[]", unknown: false},
		{value: "null", unknown: false},
		{value: `"private-warning-shape-canary"`, unknown: true, decodeKind: "string"},
		{value: `{"detail":"private-warning-shape-canary"}`, unknown: true, decodeKind: "object"},
		{value: `["private-warning-shape-canary"]`, unknown: true},
	} {
		payload := strings.Replace(pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)}), `"status":"success"`, `"status":"success","warnings":`+c.value, 1)
		alerts, err := pgbouncerStarvationOwnedPayloadResult(t, now, payload, 2)
		if err != nil {
			var typeError *json.UnmarshalTypeError
			if c.decodeKind != "" && errors.As(err, &typeError) &&
				typeError.Value == c.decodeKind && typeError.Field == "warnings" &&
				typeError.Type == reflect.TypeOf([]string(nil)) &&
				strings.HasPrefix(err.Error(), "pgbouncer pool starvation: decode Mimir response: ") {
				t.Fatalf("typed %s warning decoding erased independent pool evidence", c.decodeKind)
			}
			t.Fatal("unexpected public pool signal error for warning envelope")
		}
		requireAlertClass(t, alerts, "pgbouncer-pools-starved")
		if (requireAlertClassCount(alerts, "pgbouncer-pools-unobservable") == 1) != c.unknown {
			t.Fatal("warning envelope supplied the wrong coverage authority")
		}
		requirePgBouncerStarvationPrivate(t, alerts, "private-warning-shape-canary")
	}
}

// Per-host expected ranges cannot be supplied by another host's survivors.
func TestPgBouncerPoolStarvationAuthorityExpectedHostCannotDisappear(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	payload := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)})
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		return pgbouncerStarvationResponseForQuery(t, payload, command), nil
	}}
	settings := pgbouncerStarvationSettings(t, now, source, 2)
	other := HostSettings{Name: "db-2", Roles: []string{"pg-primary"}}
	if err := json.Unmarshal([]byte(`{"PgBouncerInstances":2}`), &other); err != nil {
		t.Fatal("could not declare the second host's census")
	}
	settings.Hosts = append(settings.Hosts, other)
	alerts, err := pgbouncerPoolStarvationSignalForTest(t).Run(context.Background(), settings)
	if err != nil {
		t.Fatal("multi-host fixture failed")
	}
	requireAlertClass(t, alerts, "pgbouncer-pools-starved")
	unknown := requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
	if !strings.Contains(unknown.Observed, "missing_instances") {
		t.Fatal("a completely absent expected host became healthy coverage")
	}
}

// Missing database authority cannot treat arbitrary database labels as pools.
func TestPgBouncerPoolStarvationAuthorityMissingDatabaseRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	payload := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)})
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		return pgbouncerStarvationResponseForQuery(t, payload, command), nil
	}}
	settings := pgbouncerStarvationSettings(t, now, source, 2)
	settings.PostgreSQL.Database = ""
	alerts, err := pgbouncerPoolStarvationSignalForTest(t).Run(context.Background(), settings)
	if err != nil {
		t.Fatal("missing database fixture failed")
	}
	if requireAlertClassCount(alerts, "pgbouncer-pools-starved") != 0 {
		t.Fatal("unadmitted database rows manufactured a page")
	}
	requireAlertClass(t, alerts, "pgbouncer-pools-unobservable")
}

// A held visibility finding does not postpone an independent sustain-1 PAGE.
func TestPgBouncerPoolStarvationAuthorityNotifierRaisesPageBesideHeldVisibility(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	payload := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)})
	alerts := runPgBouncerStarvationOwnedPayload(t, now, payload, 32)
	signal := pgbouncerPoolStarvationSignalForTest(t)
	ready, held := newCadenceAlertGate().split(signal, alerts)
	if requireAlertClassCount(ready, "pgbouncer-pools-starved") != 1 || requireAlertClassCount(held, "pgbouncer-pools-unobservable") != 1 {
		t.Fatal("visibility sustain suppressed a proven immediate page")
	}
	notifier := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	transitions := notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
	if len(transitions) != 1 || transitions[0].kind != slackRaised || transitions[0].alert.Class != "pgbouncer-pools-starved" {
		t.Fatal("notifier lost the independent page on a partial execution")
	}
}

// Scoped executions give parent cancellation precedence after a successful
// source return. Unscoped executions preserve the adapter's existing source
// authority and may publish that completed result; neither path adds a read.
func TestPgBouncerPoolStarvationAuthorityCanceledObservationRespectsScope(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	payload := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationLivelocked(1), pgbouncerStarvationLivelocked(2)})
	for _, scoped := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			calls++
			cancel()
			return pgbouncerStarvationResponseForQuery(t, payload, command), nil
		}}
		settings := pgbouncerStarvationSettings(t, now, source, 2)
		if scoped {
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "scope-excluded"})
			settings.ExcludedHosts = []string{"scope-excluded"}
		}
		alerts, err := pgbouncerPoolStarvationSignalForTest(t).Run(ctx, settings)
		cancel()
		if calls != 1 {
			t.Fatal("cancellation started another successful-source read")
		}
		if scoped {
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
				t.Fatal("scoped parent cancellation lost precedence")
			}
		} else {
			if err != nil {
				t.Fatal("unscoped completed source lost its inherited authority")
			}
			requireAlertClass(t, alerts, "pgbouncer-pools-starved")
		}
	}
}

// A configured 32-instance fleet cannot recover from one surviving row.
func TestPgBouncerPoolStarvationAuthorityMissingExpectedInstancesAreUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	payload := pgbouncerStarvationFixtureJson(t, now, []pgbouncerStarvationFixture{pgbouncerStarvationHealthy(1)})
	unknown := requireAlertClass(t, runPgBouncerStarvationOwnedPayload(t, now, payload, 32), "pgbouncer-pools-unobservable")
	if !unknown.unobservable || !strings.Contains(unknown.Observed, "missing_instances") {
		t.Fatal("31 absent expected instances were treated as healthy")
	}
}

// A typed source leaf proves that adapter wrapping preserves native error
// identity through both Is and As without rendering source text publicly.
type pgbouncerStarvationSourceFailure struct{}

func (*pgbouncerStarvationSourceFailure) Error() string { return "synthetic source failure" }

// Both paths retain the actual source failure and stop before another
// gateway. Only scoped execution adds parent cancellation to that error chain.
func TestPgBouncerPoolStarvationAuthorityCanceledFailurePreservesScopedAndUnscopedCause(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 30, 0, 0, time.UTC)
	for _, scoped := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		sourceErr := &pgbouncerStarvationSourceFailure{}
		calls := 0
		source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
			calls++
			cancel()
			return "", sourceErr
		}}
		settings := pgbouncerStarvationSettings(t, now, source, 2)
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-2", Roles: []string{"services"}})
		if scoped {
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "scope-excluded"})
			settings.ExcludedHosts = []string{"scope-excluded"}
		}
		alerts, err := pgbouncerPoolStarvationSignalForTest(t).Run(ctx, settings)
		cancel()
		if !errors.Is(err, sourceErr) || errors.Is(err, context.Canceled) != scoped || len(alerts) != 0 || calls != 1 {
			t.Fatal("cancellation changed scoped/source authority or started another gateway read")
		}
		var sourceLeaf *pgbouncerStarvationSourceFailure
		var contextFailure *signalContextError
		if !errors.As(err, &sourceLeaf) || sourceLeaf != sourceErr || errors.As(err, &contextFailure) != scoped {
			t.Fatal("source or scoped-context error identity was lost")
		}
		if scoped && (!errors.Is(contextFailure.contextErr, context.Canceled) || !errors.Is(contextFailure.sourceErr, sourceErr)) {
			t.Fatal("scoped cancellation no longer retains both original causes")
		}
	}
}
