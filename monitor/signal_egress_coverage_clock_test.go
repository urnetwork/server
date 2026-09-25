// Execute the actual coverage query/reducer over synthetic relation CTEs.
// Nothing is written, and no production relation is allowed through the seam.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func egressClockSqlTime(value *time.Time) string {
	if value == nil {
		return "NULL::timestamp"
	}
	return "timestamp '" + value.UTC().Format("2006-01-02 15:04:05.999999") + "'"
}

func egressClockSqlCheck(check *model.ProviderBlackholeCheck) string {
	if check == nil {
		return "SELECT 1::integer, NULL::timestamp, false, ''::text, 0::integer, NULL::timestamp, NULL::timestamp WHERE false"
	}
	return fmt.Sprintf("SELECT 1, %s, %t, '%s'::text, %d, %s, %s",
		egressClockSqlTime(&check.CheckedAt), check.OK, strings.ReplaceAll(check.Failure, "'", "''"),
		check.ConsecutiveFailures, egressClockSqlTime(check.FirstFailedAt), egressClockSqlTime(check.NextDueAt))
}

func egressClockFixture(t *testing.T, clock time.Time, check *model.ProviderBlackholeCheck) (Alerts, Row) {
	t.Helper()
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("coverage clock fixture requires the attested local environment")
	}
	prefix := fmt.Sprintf(`WITH network_client_location_reliability(client_id, connected, valid) AS (
 SELECT 1::integer, true, true
), network_client(client_id, active, source_client_id) AS (
 SELECT 1::integer, true, NULL::integer
), provide_key(client_id, provide_mode) AS (
 SELECT 1::integer, 3
), provider_egress_location(client_id, observed_at) AS (
 SELECT 1::integer, %s
), provider_egress_probe_attempt(client_id, attempt_at) AS (
 SELECT 1::integer, %s
), provider_egress_health(client_id, measured_at) AS (
 SELECT 1::integer, %s
), provider_blackhole_check(client_id, checked_at, ok, failure, consecutive_failures, first_failed_at, next_due_at) AS (
 %s
), lifecycle_clock AS MATERIALIZED (SELECT %s AS now_utc)`,
		egressClockSqlTime(&clock), egressClockSqlTime(&clock), egressClockSqlTime(&clock),
		egressClockSqlCheck(check), egressClockSqlTime(&clock))
	clockPattern := regexp.MustCompile(`WITH lifecycle_clock AS MATERIALIZED \(\s*SELECT now\(\) AT TIME ZONE 'UTC' AS now_utc\s*\)`)
	relationPattern := regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([a-z_][a-z0-9_.]*)`)
	allowed := map[string]bool{
		"network_client_location_reliability": true, "network_client": true, "provide_key": true,
		"provider_egress_location": true, "provider_egress_probe_attempt": true,
		"provider_egress_health": true, "provider_blackhole_check": true,
		"lifecycle_clock": true, "shards": true, "eligible": true, "classified": true,
		"snapshot": true, "deadline_counts": true, "deadline_prefixes": true,
		"deadline_slack": true, "generate_series": true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var captured Row
	dataCalls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			if strings.Contains(query, "AS blackhole_schema_armed") {
				return []Row{{"true", "true", "true"}}, nil
			}
			return []Row{{"true", "true"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			dataCalls++
			if dataCalls != 1 || len(clockPattern.FindAllStringIndex(query, -1)) != 1 {
				return nil, errors.New("coverage clock query seam changed")
			}
			for _, match := range relationPattern.FindAllStringSubmatch(query, -1) {
				if !allowed[strings.ToLower(match[1])] {
					return nil, errors.New("coverage clock query gained an unshadowed relation")
				}
			}
			query = clockPattern.ReplaceAllStringFunc(query, func(string) string { return prefix })
			var queryErr error
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, query)
				if err != nil {
					queryErr = err
					return
				}
				defer rows.Close()
				for rows.Next() {
					values, err := rows.Values()
					if err != nil {
						queryErr = err
						return
					}
					if captured != nil || (len(values) != 23 && len(values) != 24) {
						queryErr = errors.New("coverage clock aggregate shape changed")
						return
					}
					captured = make(Row, len(values))
					for i, value := range values {
						text, ok := value.(string)
						if !ok {
							queryErr = errors.New("coverage clock aggregate is not text")
							return
						}
						captured[i] = text
					}
				}
				queryErr = rows.Err()
			}, server.OptNoRetry(), server.OptReadOnly())
			return []Row{captured}, queryErr
		default:
			return nil, errors.New("unexpected coverage clock query")
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(ctx, syntheticSettings(source))
	if err != nil || dataCalls != 1 {
		t.Fatalf("SQL-to-signal fixture did not reach assertions: calls=%d err=%v", dataCalls, err)
	}
	requireEgressConfigPrivacy(t, alerts)
	return alerts, captured
}

func egressClockExposure(t *testing.T, row Row) int64 {
	t.Helper()
	if len(row) != 24 {
		t.Fatalf("missing separate measured-verdict exposure aggregate: got %d columns", len(row))
	}
	value, err := strconv.ParseInt(row[23], 10, 64)
	if err != nil {
		t.Fatal("invalid exposure aggregate")
	}
	return value
}

func TestEgressCoverageClockRescheduledNotMeasuredRetainsExposure(t *testing.T) {
	clock := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	previous := &model.ProviderBlackholeCheck{CheckedAt: clock.Add(-model.ProviderBlackholeCheckMaxAge - time.Hour), OK: true}
	next, changed := model.NextProviderBlackholeCheck(previous, model.ProviderBlackholeCheckReport{
		CheckedAt: clock.Add(-time.Minute), NotMeasured: true, Failure: model.ProviderBlackholeNotMeasuredFailure,
	}, clock, model.DefaultProviderEgressRules())
	if !changed || !next.CheckedAt.Equal(previous.CheckedAt) || next.NextDueAt == nil || !next.NextDueAt.After(clock) {
		t.Fatal("actual model did not preserve the old measurement and defer the unmeasured retry")
	}
	alerts, row := egressClockFixture(t, clock, next)
	if row[7] != "0" || row[11] != "0" {
		t.Fatalf("future retry became scheduler-ready or stale measurement became current: ready=%s current=%s", row[7], row[11])
	}
	if egressClockExposure(t, row) != 1 {
		t.Fatal("backoff erased aged measured-verdict exposure")
	}
	alert := requireAlertClass(t, alerts, "egress-blackhole-stalled")
	for _, required := range []string{"due=0", "verdict_refresh_due=1", "not_measured", "checked_at", "next_due_at", "not a submission counter", "same-attempt", "running artifact"} {
		if !strings.Contains(alert.Markdown(), required) {
			t.Fatalf("blackhole coverage qualifier missing %q", required)
		}
	}
	for _, forbidden := range []string{"not reaching a persisted probe outcome", "This is a software execution or operational rollout failure", "no submissions"} {
		if strings.Contains(alert.Markdown(), forbidden) {
			t.Fatalf("backoff was over-attributed: %q", forbidden)
		}
	}
}

func TestEgressCoverageClockFirstNotMeasuredIsNotHealthyMeasurement(t *testing.T) {
	clock := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	next, changed := model.NextProviderBlackholeCheck(nil, model.ProviderBlackholeCheckReport{
		CheckedAt: clock.Add(-time.Minute), NotMeasured: true, Failure: model.ProviderBlackholeNotMeasuredFailure,
	}, clock, model.DefaultProviderEgressRules())
	if !changed {
		t.Fatal("actual model did not create the first unknown row")
	}
	alerts, row := egressClockFixture(t, clock, next)
	if row[7] != "0" || row[9] != "-1" || row[11] != "0" || row[13] != "0" {
		t.Fatalf("unmeasured first row manufactured healthy measurement/throughput: ready=%s age=%s current=%s hour=%s", row[7], row[9], row[11], row[13])
	}
	if egressClockExposure(t, row) != 1 {
		t.Fatal("first unknown row erased missing-measurement exposure")
	}
	requireAlertClass(t, alerts, "egress-blackhole-stalled")
}

func TestEgressCoverageClockFreshFailedRetryIsSchedulerReady(t *testing.T) {
	clock := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	check := &model.ProviderBlackholeCheck{CheckedAt: clock.Add(-5 * time.Minute), Failure: "all_destinations_failed", ConsecutiveFailures: 1, NextDueAt: &clock}
	alerts, row := egressClockFixture(t, clock, check)
	if row[7] != "1" {
		t.Fatal("current measured failed retry due now was hidden by the old 90-minute age predicate")
	}
	if egressClockExposure(t, row) != 0 || len(alerts) != 0 {
		t.Fatal("ready retry with current measured evidence became a coverage stall")
	}
}

func TestEgressCoverageClockLegacyDueBoundaryMatchesScheduler(t *testing.T) {
	clock := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		age   time.Duration
		ready string
	}{
		{model.ProviderBlackholeCheckDueAge - time.Second, "0"},
		{model.ProviderBlackholeCheckDueAge, "1"},
		{model.ProviderBlackholeCheckDueAge + time.Second, "1"},
	} {
		_, row := egressClockFixture(t, clock, &model.ProviderBlackholeCheck{CheckedAt: clock.Add(-test.age), OK: true})
		if row[7] != test.ready {
			t.Fatalf("legacy NULL schedule mismatch at age %s: ready=%s want=%s", test.age, row[7], test.ready)
		}
	}
}

func TestEgressCoverageClockPassingAndTlsRemainMeasured(t *testing.T) {
	clock := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	due := clock.Add(time.Hour)
	for _, check := range []*model.ProviderBlackholeCheck{
		{CheckedAt: clock.Add(-time.Minute), OK: true, NextDueAt: &due},
		{CheckedAt: clock.Add(-time.Minute), Failure: model.ProviderBlackholeTlsAuthenticationFailure, NextDueAt: &due},
	} {
		alerts, row := egressClockFixture(t, clock, check)
		if row[7] != "0" || row[9] != "60" || row[11] != "1" || row[13] != "1" || len(alerts) != 0 {
			t.Fatal("healthy current measurement control changed")
		}
	}
}

func TestEgressCoverageClockMissingRowRemainsVisible(t *testing.T) {
	clock := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	alerts, row := egressClockFixture(t, clock, nil)
	if row[7] != "1" || row[9] != "-1" || row[11] != "0" {
		t.Fatal("missing row became healthy")
	}
	requireAlertClass(t, alerts, "egress-blackhole-stalled")
}

func TestEgressCoverageClockMissingScheduleSchemaStopsDataRead(t *testing.T) {
	dataCalls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			if strings.Contains(query, "AS blackhole_schema_armed") {
				return []Row{{"true", "true", "false"}}, nil
			}
			return []Row{{"true", "true"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		default:
			dataCalls++
			return nil, errors.New("synthetic dependent schema query must not execute")
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || dataCalls != 0 {
		t.Fatalf("missing schedule schema reached dependent I/O: calls=%d err=%v", dataCalls, err)
	}
	alert := requireAlertClass(t, alerts, "egress-probe-unarmed")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Markdown(), "blackhole_schema_armed=false") {
		t.Fatal("missing schedule schema did not remain explicitly unobservable")
	}
}

func TestEgressCoverageClockMalformedSchemaFailsClosed(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"true", "true", "private-synthetic-malformed"}}, nil
	}}
	_, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err == nil || strings.Contains(err.Error(), "private-synthetic-malformed") {
		t.Fatal("malformed schema reply was healthy or leaked its value")
	}
}

func TestEgressCoverageClockLegacyAggregateCannotClearTicket(t *testing.T) {
	row := syntheticEgressCoverageActivity(egressCoverageSnapshot{
		eligible: 1, fullCurrent: 1, blackholeCurrent: 1,
		fullAgeSeconds: 1, blackholeAgeSeconds: 1,
	})
	if len(row) < 23 {
		t.Fatal("fixture lost its historical columns")
	}
	if _, err := parseEgressCoverageActivity([]pgRow{pgRow(row[:23])}, 1); err == nil {
		t.Fatal("legacy response lacking measured exposure was accepted as healthy")
	}
}

func TestEgressCoverageClockCatalogSeparatesTheThreeClocks(t *testing.T) {
	raw, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(raw), "### 2.19 Provider egress probe coverage")
	if start < 0 {
		t.Fatal("owning coverage catalog section missing")
	}
	end := strings.Index(string(raw)[start:], "### 2.19a ")
	if end < 0 {
		t.Fatal("owning coverage catalog section missing")
	}
	section := string(raw)[start : start+end]
	for _, required := range []string{
		"COALESCE(next_due_at, checked_at + 90 minutes)", "verdict_refresh_due",
		"not_measured", "not a submission counter", "24 columns",
		"migration 709", "same-attempt", "no-full", "serial",
	} {
		if !strings.Contains(section, required) {
			t.Fatalf("coverage catalog misses %q", required)
		}
	}
}
