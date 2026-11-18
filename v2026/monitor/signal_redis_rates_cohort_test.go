// Exact-cohort controls use independent configured and observed identities.
// The native-engine companion verifies the production PromQL on raw samples.
package monitor

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A/B/B_alt still has three series, but cannot attest configured A/B/C.
func TestRedisRatesSignalForeignSourceCannotReplaceConfiguredNode(t *testing.T) {
	sources := redisRatesTestSources()
	sources[2] = redisRateSource{host: "redis-b-alt.example", node: sources[1].node}
	response := redisRatesMimirResponse(redisRatesMimirRows(3, 0, 3, sources), nil)
	settings, _ := redisRatesSyntheticSettings(t, response)
	settings.Grafana.AdminPassword = "test"
	server, _ := newRedisRatesGrafanaServer(t, "test", "5m", "5m", http.StatusOK, "")
	defer server.Close()
	alerts, err := newRedisRatesSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("substituted-cohort alert count = %d, want 1", len(alerts))
	}
	alert := requireAlertClass(t, alerts, "redis-rate-source-coverage")
	for _, want := range []string{"expected_nodes=3", "commands_raw=2 commands_1m=0 commands_5m=2"} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("substituted cohort omitted %q", want)
		}
	}
	requireAlertOmits(t, alert, "redis-a.example", "redis-b.example", "redis-b-alt.example", "17001", "17002", "17003")
}

// An unrelated additional source cannot spoil an otherwise complete cohort.
func TestParseRedisRateCoverageIgnoresForeignCompanion(t *testing.T) {
	sources := append(redisRatesTestSources(), redisRateSource{host: "redis-unconfigured.example", node: "17004"})
	response := redisRatesMimirResponse(redisRatesMimirRows(4, 0, 4, sources), nil)
	coverage, err := parseRedisRateCoverage(response, "synthetic", redisRatesExpectedTestSources())
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.complete() || coverage.count("commands", "raw") != 3 {
		t.Fatalf("complete configured cohort was not retained: %s", coverage.summary())
	}
}

// Source unavailability must leave both independently proved definition
// breaches visible, even when a success envelope contains every expected row.
func TestRedisRatesSignalMimirWarningsKeepIndependentDefinitionFindings(t *testing.T) {
	const privateWarning = "synthetic-private-partial-response-marker"
	response := redisRatesMimirResponse(redisRatesMimirRows(3, 0, 3, redisRatesTestSources()), []string{privateWarning})
	settings, _ := redisRatesSyntheticSettings(t, response)
	settings.Grafana.AdminPassword = "test"
	server, _ := newRedisRatesGrafanaServer(t, "test", "$__rate_interval", "2m", http.StatusOK, "")
	defer server.Close()
	alerts, err := newRedisRatesSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 3 {
		t.Fatalf("partial-response alert count = %d, want 3", len(alerts))
	}
	requireAlertClass(t, alerts, "cannot-observe")
	requireAlertClass(t, alerts, "redis-dashboard-rate-window")
	requireAlertClass(t, alerts, "redis-alert-rate-window")
	for _, alert := range alerts {
		requireAlertOmits(t, alert, privateWarning, "redis-a.example", "redis-b.example")
	}
}

// Every warning list, including a blank warning string, withholds completeness.
func TestParseRedisRateCoverageRejectsPartialWarnings(t *testing.T) {
	for _, warnings := range [][]string{{"synthetic-partial"}, {""}} {
		response := redisRatesMimirResponse(redisRatesMimirRows(3, 0, 3, redisRatesTestSources()), warnings)
		if _, err := parseRedisRateCoverage(response, "synthetic", redisRatesExpectedTestSources()); err == nil {
			t.Fatal("warning-bearing response attested complete coverage")
		}
	}
}

// Count-by preserves extra-series multiplicity; duplicate result rows are
// independently ambiguous and must not depend on which row arrived first.
func TestParseRedisRateCoverageRejectsAmbiguousConfiguredSources(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]redisRatesFixtureRow) []redisRatesFixtureRow
	}{
		{name: "extra-publisher", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			rows[1].Value[1] = "2"
			return rows
		}},
		{name: "duplicate-row", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			return append(rows, rows[1])
		}},
		{name: "nonfinite-count", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			rows[1].Value[1] = "NaN"
			return rows
		}},
		{name: "infinite-count", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			rows[1].Value[1] = "+Inf"
			return rows
		}},
		{name: "fractional-count", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			rows[1].Value[1] = "0.5"
			return rows
		}},
		{name: "negative-count", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			rows[1].Value[1] = "-1"
			return rows
		}},
		{name: "missing-marker", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			return rows[1:]
		}},
		{name: "duplicate-marker", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			return append(rows, rows[0])
		}},
		{name: "missing-job", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			delete(rows[1].Metric, "job")
			return rows
		}},
		{name: "wrong-job", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			rows[1].Metric["job"] = "redis_exporter"
			return rows
		}},
		{name: "wrong-environment", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			rows[1].Metric["env"] = "synthetic-other"
			return rows
		}},
		{name: "unaggregated-label", mutate: func(rows []redisRatesFixtureRow) []redisRatesFixtureRow {
			rows[1].Metric["instance"] = "synthetic-other-publisher"
			return rows
		}},
	}
	for _, test := range cases {
		rows := test.mutate(redisRatesMimirRows(3, 0, 3, redisRatesTestSources()))
		if _, err := parseRedisRateCoverage(redisRatesMimirResponse(rows, nil), "synthetic", redisRatesExpectedTestSources()); err == nil {
			t.Errorf("%s was accepted as unambiguous coverage", test.name)
		}
	}
}

// Empty requested partitions are observed missing coverage, not health or a
// transport failure; their explicit markers distinguish an omitted partition.
func TestParseRedisRateCoverageEmptyConfiguredCohortIsNotHealthy(t *testing.T) {
	coverage, err := parseRedisRateCoverage(redisRatesMimirFixture(0, 0, 0), "synthetic", redisRatesExpectedTestSources())
	if err != nil {
		t.Fatal(err)
	}
	if coverage.complete() || coverage.count("commands", "raw") != 0 {
		t.Fatal("empty configured coverage was healthy")
	}
}

// Markers may precede or follow samples, and map/series order carries no authority.
func TestParseRedisRateCoverageIsIndependentOfSeriesOrder(t *testing.T) {
	rows := redisRatesMimirRows(3, 0, 3, redisRatesTestSources())
	forward, err := parseRedisRateCoverage(redisRatesMimirResponse(rows, nil), "synthetic", redisRatesExpectedTestSources())
	if err != nil {
		t.Fatal(err)
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	reverse, err := parseRedisRateCoverage(redisRatesMimirResponse(rows, nil), "synthetic", redisRatesExpectedTestSources())
	if err != nil {
		t.Fatal(err)
	}
	if !forward.complete() || !reverse.complete() || forward.summary() != reverse.summary() {
		t.Fatal("response order changed configured coverage")
	}
}
