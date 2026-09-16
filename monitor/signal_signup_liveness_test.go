package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

var signupTestTime = time.Date(2026, 9, 16, 6, 30, 0, 0, time.UTC)

func signupTestMetrics(t *testing.T, fields map[string]float64) string {
	t.Helper()
	result := []any{}
	for kind, value := range fields {
		result = append(result, map[string]any{"metric": map[string]string{"monitor_signup": kind}, "value": []any{signupTestTime.Unix(), strconv.FormatFloat(value, 'f', -1, 64)}})
	}
	raw, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func signupTestDemand() map[string]float64 {
	return map[string]float64{"requests": 80.25, "successes": 64.28, "server_errors": 0, "series": 8, "samples": 55, "resets": 0, "source_age": 14}
}

func signupTestTotals() map[string]float64 {
	return map[string]float64{"publishers": 8, "minimum": 1016189, "maximum": 1016192, "source_age": 14}
}

func runSignupTest(t *testing.T, created, deleted, churn int64, demand, totals string) (Alerts, error) {
	t.Helper()
	return runSignupWindowTest(t, created, deleted, churn, created, created, demand, totals)
}

func runSignupWindowTest(t *testing.T, created, deleted, churn, createdSixHours, createdGuarded int64, demand, totals string) (Alerts, error) {
	t.Helper()
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if query != signupLivenessQuery {
				t.Fatal("signup did not execute the bounded owning query")
			}
			return []Row{{strconv.FormatInt(signupTestTime.Unix(), 10), "1016192", fmt.Sprint(created), fmt.Sprint(deleted), fmt.Sprint(churn), fmt.Sprint(createdSixHours), fmt.Sprint(createdGuarded)}}, nil
		},
		hostFn: func(host HostSettings, command string) (string, error) {
			decoded, err := url.QueryUnescape(command)
			if err != nil || host.Name != "signup-metrics-synthetic" || !strings.Contains(command, "--max-time 15 --max-filesize 65536") || !strings.Contains(decoded, "&time="+strconv.FormatInt(signupTestTime.Unix(), 10)) || strings.Contains(decoded, "job=") {
				t.Fatal("signup metrics lost its configured gateway, bounds, or fixed database clock")
			}
			if strings.Contains(decoded, "urnetwork_http_requests_total") {
				if !strings.Contains(decoded, `route="POST ^/auth/network-create$"`) || !strings.Contains(decoded, `service="api"`) ||
					!strings.Contains(decoded, `status=~"2..",outcome="completed"`) || !strings.Contains(decoded, "offset 5m") {
					t.Fatal("signup demand lost its exact route, completed-response, or completed-window boundary")
				}
				return demand, nil
			}
			if !strings.Contains(decoded, "urnetwork_stats_total_networks") || !strings.Contains(decoded, `service="taskworker"`) {
				t.Fatal("unexpected signup metric family")
			}
			return totals, nil
		},
	}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return signupTestTime }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "signup-metrics-synthetic", Roles: []string{"services"}})
	return NewSignupLivenessSignal().Run(context.Background(), settings)
}

func TestSignupLivenessCurrentProgressAndDeletionControls(t *testing.T) {
	for _, counts := range [][3]int64{{64, 17, 30}, {50, 50, 30}, {1, 100, 30}} {
		alerts, err := runSignupTest(t, counts[0], counts[1], counts[2], signupTestMetrics(t, signupTestDemand()), signupTestMetrics(t, signupTestTotals()))
		if err != nil || len(alerts) != 0 {
			t.Fatalf("positive accepted creation, including flat/declining net total, alerted: error=%v classes=%d", err, len(alerts))
		}
	}
	// A matured quality cohort can be empty while today's signup path is stuck.
	quality, err := NewSignupQualitySignal().Run(context.Background(), syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"2026-09-13", "0", "0"}}, nil
	}}))
	if err != nil || len(quality) != 0 {
		t.Fatal("quality guard's low-volume contract unexpectedly changed")
	}
	alerts, err := runSignupTest(t, 0, 0, 0, signupTestMetrics(t, signupTestDemand()), signupTestMetrics(t, signupTestTotals()))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "signup-no-durable-progress")
	if alert.Sustain != 2 || alert.Target != "network-create" || NewSignupLivenessSignal().Cadence() != 10*time.Minute {
		t.Fatal("signup liveness changed its identity/cadence/sustain contract")
	}
	for _, want := range []string{"extrapolated, not an exact event count", "not proof that every request was valid", "five minutes before and after", "no request-volume floor", "audit_deleted=0", "low demand is insufficient"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("signup warning omitted boundary %q", want)
		}
	}
	requireAlertClass(t, alerts, "signup-liveness-unproven")
	requireAlertOmits(t, alert, "signup-metrics-synthetic", "network_id=", "instance=", "automated signup", "all signup is broken")
}

func TestSignupLivenessNoDemandOrExpectedRefusalsRemainUnproven(t *testing.T) {
	for _, requests := range []float64{0, 0.001, 19.999, 100} {
		demand := signupTestDemand()
		demand["requests"], demand["successes"], demand["server_errors"] = requests, 0, 0
		alerts, err := runSignupTest(t, 0, 0, 0, signupTestMetrics(t, demand), signupTestMetrics(t, signupTestTotals()))
		if err != nil || len(alerts) != 1 {
			t.Fatal("no demand or expected-refusal traffic lost its independent unknown witness boundary")
		}
		alert := requireAlertClass(t, alerts, "signup-liveness-unproven")
		if alert.Severity != SeverityWarn || alert.Sustain != 2 || !strings.Contains(alert.Markdown(), "not a signup outage") || !strings.Contains(alert.Markdown(), "witness_window_seconds=21600") {
			t.Fatal("missing witness was not described as sustained WARN/unknown")
		}
		alerts, err = runSignupWindowTest(t, 0, 0, 0, 1, 0, signupTestMetrics(t, demand), signupTestMetrics(t, signupTestTotals()))
		if err != nil || len(alerts) != 0 {
			t.Fatal("a recent durable witness with no server errors or success/audit contradiction alerted")
		}
	}
}

func TestSignupLivenessSuccessContradictionHasNoVolumeFloor(t *testing.T) {
	for _, successes := range []float64{0.001, 1, 19.999, 64.28} {
		demand := signupTestDemand()
		demand["requests"], demand["successes"] = successes, successes
		// An earlier six-hour witness does not hide a current response/audit
		// contradiction, even if the estimated success count is below one.
		alerts, err := runSignupWindowTest(t, 0, 0, 0, 1, 0, signupTestMetrics(t, demand), signupTestMetrics(t, signupTestTotals()))
		if err != nil || len(alerts) != 1 {
			t.Fatal("positive success/audit contradiction was suppressed or over-attributed")
		}
		requireAlertClass(t, alerts, "signup-no-durable-progress")
		alerts, err = runSignupWindowTest(t, 0, 0, 0, 1, 1, signupTestMetrics(t, demand), signupTestMetrics(t, signupTestTotals()))
		if err != nil || len(alerts) != 0 {
			t.Fatal("accepted creation in the five-minute edge allowance did not satisfy the guard")
		}
	}
}

func TestSignupLivenessSuccessQueryRequiresCompletedTransport(t *testing.T) {
	query := signupMetricQuery("synthetic", true)
	if !strings.Contains(query, `status=~"2..",outcome="completed"`) {
		t.Fatal("2xx response/audit comparison includes canceled or aborted transports")
	}
	if !strings.Contains(query, `status=~"5.."}`) || strings.Contains(query, `status=~"5..",outcome="completed"`) {
		t.Fatal("5xx observation lost panic or canceled server-error outcomes")
	}
}

func TestSignupLivenessServerErrorsAreIndependentOfDurableProgress(t *testing.T) {
	for _, created := range []int64{0, 64} {
		for _, failures := range []float64{0.001, 1, 8.04} {
			demand := signupTestDemand()
			demand["requests"], demand["successes"], demand["server_errors"] = float64(created)+failures, float64(created), failures
			alerts, err := runSignupWindowTest(t, created, 0, 0, created+1, created, signupTestMetrics(t, demand), signupTestMetrics(t, signupTestTotals()))
			if err != nil || len(alerts) != 1 {
				t.Fatal("server-error boundary was suppressed by durable progress or incorrectly became a success/audit contradiction")
			}
			alert := requireAlertClass(t, alerts, "signup-route-server-errors")
			for _, want := range []string{"independently of successful or durable creations", "400/409/429", "preserve genuine or ambiguous internal failures as 500", "429/Retry-After"} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatalf("independent server-error finding omitted %q", want)
				}
			}
			if alert.Sustain != 2 {
				t.Fatal("server errors lost their sustain guard")
			}
		}
	}
}

func TestSignupLivenessTotalDriftAndRefreshAllowance(t *testing.T) {
	for _, test := range []struct {
		min, max float64
		warn     bool
	}{{min: 1016152, max: 1016232}, {min: 1016151, max: 1016192, warn: true}, {min: 1016192, max: 1016233, warn: true}} {
		totals := signupTestTotals()
		totals["minimum"], totals["maximum"] = test.min, test.max
		alerts, err := runSignupTest(t, 64, 17, 30, signupTestMetrics(t, signupTestDemand()), signupTestMetrics(t, totals))
		if err != nil {
			t.Fatal(err)
		}
		if !test.warn {
			if len(alerts) != 0 {
				t.Fatal("exact churn/refresh tolerance boundary alerted")
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "signup-total-drift")
		for _, want := range []string{"allowance=40", "not stalled signup", "Fresh transport timestamps do not prove fresh database collection"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("drift warning omitted %q", want)
			}
		}
	}
}

func TestSignupLivenessUnknownDoesNotMaskIndependentFinding(t *testing.T) {
	for _, demand := range []bool{true, false} {
		for _, change := range []struct {
			key   string
			value float64
		}{{key: "source_age", value: 90.001}, {key: "source_age", value: -1}, {key: "source_age", value: 1e30}} {
			fields := signupTestTotals()
			if demand {
				fields = signupTestDemand()
			}
			fields[change.key] = change.value
			if _, err := parseSignupMetrics(signupTestMetrics(t, fields), signupTestTime, demand); err == nil {
				t.Fatal("stale/invalid metric evidence was accepted")
			}
		}
	}
	for _, change := range []struct {
		key   string
		value float64
	}{{key: "resets", value: 1}, {key: "samples", value: 1}, {key: "series", value: 0}, {key: "series", value: 1.5}, {key: "successes", value: 1000}} {
		fields := signupTestDemand()
		fields[change.key] = change.value
		if _, err := parseSignupMetrics(signupTestMetrics(t, fields), signupTestTime, true); err == nil {
			t.Fatal("reset, warmup, absent, or contradictory demand was accepted")
		}
	}
	totals := signupTestTotals()
	totals["maximum"] = 2000000
	alerts, err := runSignupTest(t, 0, 0, 0, `{"status":"error","error":"private-customer@example.invalid"}`, signupTestMetrics(t, totals))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "signup-total-drift")
	requireAlertClass(t, alerts, "signup-liveness-unproven")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	requireAlertOmits(t, unknown, "private-customer@example.invalid")
	alerts, err = runSignupTest(t, 0, 0, 0, signupTestMetrics(t, signupTestDemand()), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "signup-no-durable-progress")
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestSignupLivenessStrictAggregateParsingAndCancellation(t *testing.T) {
	valid := signupTestMetrics(t, signupTestDemand())
	for _, invalid := range []string{
		`{}`, `{"status":"success","data":{"resultType":"vector","result":[]}}`,
		strings.Replace(valid, `"status":"success"`, `"warnings":["private-warning"],"status":"success"`, 1),
		strings.Replace(valid, `"monitor_signup":"requests"`, `"monitor_signup":"unrecognized-private-label"`, 1),
		strings.Replace(valid, `"monitor_signup":"requests"`, `"monitor_signup":"successes"`, 1),
		strings.Replace(valid, `"80.25"`, `"NaN"`, 1),
		strings.Replace(valid, `"80.25"`, `"+Inf"`, 1),
		strings.Replace(valid, fmt.Sprint(signupTestTime.Unix()), fmt.Sprint(signupTestTime.Add(-time.Minute).Unix()), 1),
		strings.Repeat("x", 65537),
	} {
		_, err := parseSignupMetrics(invalid, signupTestTime, true)
		if err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatal("malformed metrics did not fail closed without echoing content")
		}
	}
	for _, rows := range [][]pgRow{
		nil, {{"1"}}, {{"0", "1", "0", "0", "0", "0", "0"}},
		{{fmt.Sprint(signupTestTime.Unix()), "private-value", "0", "0", "0", "0", "0"}},
		{{fmt.Sprint(signupTestTime.Unix()), "1", "-1", "0", "0", "0", "0"}},
		{{fmt.Sprint(signupTestTime.Unix()), "1", "1", "0", "0", "1", "0"}},
		{{fmt.Sprint(signupTestTime.Unix()), "1", "0", "0", "0", "0", "1"}},
		{{fmt.Sprint(signupTestTime.Unix()), "9007199254740992", "0", "0", "0", "0", "0"}},
	} {
		if _, err := parseSignupDurableSnapshot(rows, signupTestTime); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("malformed durable evidence did not fail closed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	touched := false
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { touched = true; return nil, nil }}
	_, err := NewSignupLivenessSignal().Run(ctx, syntheticSettings(source))
	if !errors.Is(err, context.Canceled) || touched {
		t.Fatal("canceled signup observation contacted a source or became healthy")
	}
}

func TestSignupLivenessDurableSqlWindowBoundaries(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("signup SQL fixture requires the attested local environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Shadow both tables with VALUES, and replace only the clock. The exact
	// production reducer runs without any real table, identity, or write.
	query := strings.Replace(signupLivenessQuery, "WITH clock AS MATERIALIZED", `WITH network AS (SELECT 1 FROM generate_series(1, 100)), audit_network_event(event_time, event_type) AS (
	 VALUES (timestamp '2026-09-16 00:29:59', 'network_created'),
	        (timestamp '2026-09-16 00:30:00', 'network_created'),
	        (timestamp '2026-09-16 05:19:59', 'network_created'),
	        (timestamp '2026-09-16 05:20:00', 'network_created'),
	        (timestamp '2026-09-16 05:24:59', 'network_created'),
	        (timestamp '2026-09-16 05:25:00', 'network_created'),
	        (timestamp '2026-09-16 06:24:59', 'network_created'),
	        (timestamp '2026-09-16 06:25:00', 'network_created'),
	        (timestamp '2026-09-16 06:20:00', 'network_deleted'),
	        (timestamp '2026-09-16 06:30:00', 'network_created'),
	        (timestamp '2026-09-16 06:20:00', 'unrelated_event')
	), clock AS MATERIALIZED`, 1)
	query = strings.ReplaceAll(query, "statement_timestamp()", "timestamptz '2026-09-16 06:30:00+00'")
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var values [7]int64
		if !rows.Next() {
			t.Fatal("signup SQL returned no aggregate")
		}
		if err := rows.Scan(&values[0], &values[1], &values[2], &values[3], &values[4], &values[5], &values[6]); err != nil {
			t.Fatal(err)
		}
		if values != [7]int64{signupTestTime.Unix(), 100, 2, 1, 3, 7, 5} || rows.Next() || rows.Err() != nil {
			t.Fatalf("signup SQL boundary reduction differs: %v", values)
		}
	})
}

func TestSignupLivenessDurableSqlNoEventsRetainsClock(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("signup SQL fixture requires the attested local environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	query := strings.Replace(signupLivenessQuery, "WITH clock AS MATERIALIZED", `WITH network AS (SELECT 1 WHERE false), audit_network_event(event_time, event_type) AS (
	 SELECT timestamp '2026-09-16 06:30:00', 'network_created'::text WHERE false
	), clock AS MATERIALIZED`, 1)
	query = strings.ReplaceAll(query, "statement_timestamp()", "timestamptz '2026-09-16 06:30:00+00'")
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var values [7]int64
		if !rows.Next() {
			t.Fatal("empty audit input lost the authoritative clock aggregate")
		}
		if err := rows.Scan(&values[0], &values[1], &values[2], &values[3], &values[4], &values[5], &values[6]); err != nil {
			t.Fatal(err)
		}
		if values != [7]int64{signupTestTime.Unix(), 0, 0, 0, 0, 0, 0} || rows.Next() || rows.Err() != nil {
			t.Fatal("empty audit input became missing evidence instead of an unproven witness")
		}
	})
}
