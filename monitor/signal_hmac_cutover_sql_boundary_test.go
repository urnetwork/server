// Execute the actual HMAC query and public reducer over synthetic table CTEs.
// These tests use only the attested local database, never fixture writes or
// production rows. The existing uppercase test prefix is retained so the
// established HMAC focused gate selects these controls.
package monitor

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A synthetic check repeated for a disjoint fixed integer identity range.
// The integers exist only in the CTE and never leave the aggregate query.
func hmacCutoverSqlCheckRows(first, last int, check *model.ProviderBlackholeCheck) string {
	if check == nil {
		return "SELECT 0::integer, NULL::timestamp, false, ''::text, 0::integer, NULL::timestamp WHERE false"
	}
	firstFailed := "NULL::timestamp"
	if check.FirstFailedAt != nil {
		firstFailed = "timestamp '" + check.FirstFailedAt.UTC().Format("2006-01-02 15:04:05.999999") + "'"
	}
	return fmt.Sprintf(
		"SELECT id, timestamp '%s', %t, '%s'::text, %d, %s FROM generate_series(%d, %d) AS source(id)",
		check.CheckedAt.UTC().Format("2006-01-02 15:04:05.999999"), check.OK,
		strings.ReplaceAll(check.Failure, "'", "''"), check.ConsecutiveFailures, firstFailed, first, last,
	)
}

// Freeze only time and the four relation inputs; the production classification,
// current-verdict predicate, cohort aggregation and public reducer are intact.
func runHmacCutoverSqlFixture(t testing.TB, clock time.Time, legacySql, compatibleSql string, ambiguousCompatible bool) (Alerts, hmacCutoverSnapshot) {
	t.Helper()
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("HMAC SQL fixture requires the attested local test environment")
	}
	rules := model.GetProviderEgressRules()
	if rules.DarkConsecutiveFailures != 3 || rules.DarkMinimumSpanSeconds != 1800 {
		t.Fatal("HMAC SQL fixture requires canonical local three-failure/thirty-minute rules; no causal assertion reached")
	}
	compatibleDescription := "synthetic provider linux 2026.6.1"
	if ambiguousCompatible {
		compatibleDescription = "synthetic provider linux 2026.6.1 2026.7.1"
	}
	pass := &model.ProviderBlackholeCheck{CheckedAt: clock.Add(-time.Minute), OK: true}
	clockPrefix := fmt.Sprintf(`WITH network_client_location_reliability(client_id, connected, valid) AS (
    SELECT id, true, true FROM generate_series(1, 60) AS source(id)
), network_client(client_id, network_id, active, source_client_id, description) AS (
    SELECT id, id, true, NULL::integer,
           CASE WHEN id <= 20 THEN 'synthetic provider linux 2026.1.1'
                WHEN id <= 40 THEN '%s'
                ELSE 'synthetic unknown metadata' END
    FROM generate_series(1, 60) AS source(id)
), provide_key(client_id, provide_mode) AS (
    SELECT id, 3 FROM generate_series(1, 60) AS source(id)
), provider_blackhole_check(client_id, checked_at, ok, failure, consecutive_failures, first_failed_at) AS (
    %s UNION ALL %s UNION ALL %s
), clock AS MATERIALIZED (
    SELECT timestamp '%s' AS utc_now
)`, compatibleDescription, legacySql, compatibleSql, hmacCutoverSqlCheckRows(41, 60, pass), clock.UTC().Format("2006-01-02 15:04:05.999999"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var captured []Row
	calls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "monitor-schema-readiness") {
			return []Row{{"true"}}, nil
		}
		calls++
		const originalClock = "WITH clock AS MATERIALIZED (\n    SELECT now() AT TIME ZONE 'UTC' AS utc_now\n)"
		if calls != 1 || strings.Count(query, originalClock) != 1 || strings.Count(query, "monitor-signal-2.24-hmac-cutover") != 1 {
			return nil, fmt.Errorf("synthetic HMAC query boundary changed")
		}
		// Refuse an unshadowed dependency if the owning query changes later.
		allowed := map[string]bool{
			"network_client_location_reliability": true, "network_client": true,
			"provide_key": true, "provider_blackhole_check": true, "clock": true,
			"eligible": true, "metadata": true, "classified": true, "checked": true, "aggregate": true,
		}
		for _, match := range regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([a-z_][a-z0-9_.]*)`).FindAllStringSubmatch(query, -1) {
			if !allowed[strings.ToLower(match[1])] {
				return nil, fmt.Errorf("synthetic HMAC query gained an unshadowed relation")
			}
		}
		query = strings.Replace(query, originalClock, clockPrefix, 1)
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
				if len(captured) != 0 || len(values) != 13 {
					queryErr = fmt.Errorf("synthetic HMAC aggregate shape changed")
					return
				}
				row := make(Row, len(values))
				for i, value := range values {
					text, ok := value.(string)
					if !ok {
						queryErr = fmt.Errorf("synthetic HMAC aggregate is not text")
						return
					}
					row[i] = text
				}
				captured = append(captured, row)
			}
			queryErr = rows.Err()
		}, server.OptNoRetry(), server.OptReadOnly())
		return captured, queryErr
	}}
	alerts, err := NewHMACCutoverSignal().Run(ctx, syntheticSettings(source))
	if err != nil || calls != 1 {
		t.Fatalf("synthetic SQL-to-signal control failed before assertions: calls=%d error=%v", calls, err)
	}
	pgRows := make([]pgRow, len(captured))
	for i, row := range captured {
		pgRows[i] = pgRow(row)
	}
	snapshot, err := parseHMACCutoverSnapshot(pgRows)
	if err != nil {
		t.Fatalf("synthetic SQL aggregate invalid: %v", err)
	}
	for _, alert := range alerts {
		if strings.Contains(alert.Markdown(), "synthetic provider") || strings.Contains(alert.Markdown(), "synthetic unknown") {
			t.Fatal("synthetic descriptions escaped the aggregate boundary")
		}
	}
	return alerts, snapshot
}

// The old query pages from twenty single failed checks; the corrected one
// preserves readiness uncertainty without manufacturing any dark verdict.
func TestHMACCutoverSqlSingleFailureDoesNotBecomeDark(t *testing.T) {
	clock := storedContractHMACCutover().Add(24 * time.Hour)
	checkedAt := clock.Add(-time.Minute)
	single := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 1, FirstFailedAt: &checkedAt}
	pass := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, OK: true}
	alerts, snapshot := runHmacCutoverSqlFixture(t, clock, hmacCutoverSqlCheckRows(1, 20, single), hmacCutoverSqlCheckRows(21, 40, pass), false)
	if snapshot.legacyChecked != 0 || snapshot.legacyDark != 0 || snapshot.compatibleChecked != 20 || snapshot.compatibleOK != 20 {
		t.Fatalf("single failure became a verdict: legacy_checked=%d legacy_dark=%d compatible_checked=%d compatible_ok=%d", snapshot.legacyChecked, snapshot.legacyDark, snapshot.compatibleChecked, snapshot.compatibleOK)
	}
	if len(alerts) != 1 || requireAlertClass(t, alerts, "contract-hmac-readiness").Severity != SeverityWarn {
		t.Fatal("single failures did not preserve readiness uncertainty")
	}
}

// Mature failure and immediate TLS controls still page; a current pass is a
// passing verdict, not a dark one, while claimed legacy metadata remains risk.
func TestHMACCutoverSqlDarkAndPassControls(t *testing.T) {
	clock := storedContractHMACCutover().Add(24 * time.Hour)
	checkedAt := clock.Add(-time.Minute)
	firstFailedAt := checkedAt.Add(-30 * time.Minute)
	pass := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, OK: true}
	for _, state := range []struct {
		name  string
		check *model.ProviderBlackholeCheck
		dark  int64
	}{
		{name: "mature", check: &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &firstFailedAt}, dark: 20},
		{name: "tls", check: &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: model.ProviderBlackholeTlsAuthenticationFailure, ConsecutiveFailures: 1, FirstFailedAt: &checkedAt}, dark: 20},
		{name: "pass", check: pass, dark: 0},
	} {
		alerts, snapshot := runHmacCutoverSqlFixture(t, clock, hmacCutoverSqlCheckRows(1, 20, state.check), hmacCutoverSqlCheckRows(21, 40, pass), false)
		if snapshot.legacyChecked != 20 || snapshot.legacyDark != state.dark || snapshot.legacyOK != 20-state.dark {
			t.Fatalf("%s changed the verdict partition: checked=%d dark=%d ok=%d", state.name, snapshot.legacyChecked, snapshot.legacyDark, snapshot.legacyOK)
		}
		class := "contract-hmac-readiness"
		if state.dark > 0 {
			class = "contract-hmac-incompatible"
		}
		if len(alerts) != 1 {
			t.Fatalf("%s returned %d findings", state.name, len(alerts))
		}
		requireAlertClass(t, alerts, class)
	}
}

// Missing, immature and expired measurements are not passing controls and
// cannot make the claimed legacy cohort look behaviorally dark.
func TestHMACCutoverSqlIncompleteAndStaleRemainUnclassified(t *testing.T) {
	clock := storedContractHMACCutover().Add(24 * time.Hour)
	checkedAt := clock.Add(-time.Minute)
	shortStart := checkedAt.Add(-30*time.Minute + time.Second)
	oldCheck := clock.Add(-model.ProviderBlackholeCheckMaxAge - time.Second)
	oldStart := oldCheck.Add(-time.Hour)
	pass := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, OK: true}
	for _, state := range []struct {
		name  string
		check *model.ProviderBlackholeCheck
	}{
		{name: "missing"},
		{name: "old migration row", check: &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed"}},
		{name: "unmeasured first check", check: &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: model.ProviderBlackholeNotMeasuredFailure}},
		{name: "short span", check: &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &shortStart}},
		{name: "null first failure", check: &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3}},
		{name: "expired dark", check: &model.ProviderBlackholeCheck{CheckedAt: oldCheck, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &oldStart}},
		{name: "expired pass", check: &model.ProviderBlackholeCheck{CheckedAt: oldCheck, OK: true}},
	} {
		alerts, snapshot := runHmacCutoverSqlFixture(t, clock, hmacCutoverSqlCheckRows(1, 20, state.check), hmacCutoverSqlCheckRows(21, 40, pass), false)
		if snapshot.legacy != 20 || snapshot.legacyChecked != 0 || snapshot.legacyDark != 0 || snapshot.legacyOK != 0 {
			t.Errorf("%s entered a verdict denominator: checked=%d dark=%d ok=%d", state.name, snapshot.legacyChecked, snapshot.legacyDark, snapshot.legacyOK)
			continue
		}
		if len(alerts) != 1 {
			t.Fatalf("%s returned %d findings", state.name, len(alerts))
		}
		requireAlertClass(t, alerts, "contract-hmac-readiness")
	}
}

// Ten passes plus ten immature failures are only ten compatible verdicts,
// not a twenty-verdict 50% control capable of satisfying the PAGE threshold.
func TestHMACCutoverSqlCompatibleImmaturityDoesNotCertifyControl(t *testing.T) {
	clock := storedContractHMACCutover().Add(24 * time.Hour)
	checkedAt := clock.Add(-time.Minute)
	firstFailedAt := checkedAt.Add(-30 * time.Minute)
	dark := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &firstFailedAt}
	pass := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, OK: true}
	single := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 1, FirstFailedAt: &checkedAt}
	compatibleSql := hmacCutoverSqlCheckRows(21, 30, pass) + " UNION ALL " + hmacCutoverSqlCheckRows(31, 40, single)
	alerts, snapshot := runHmacCutoverSqlFixture(t, clock, hmacCutoverSqlCheckRows(1, 20, dark), compatibleSql, false)
	if snapshot.compatibleChecked != 10 || snapshot.compatibleOK != 10 || snapshot.compatibleDark != 0 {
		t.Fatalf("immature compatible checks certified the control: checked=%d ok=%d dark=%d", snapshot.compatibleChecked, snapshot.compatibleOK, snapshot.compatibleDark)
	}
	if len(alerts) != 1 {
		t.Fatalf("incomplete compatible control returned %d findings", len(alerts))
	}
	requireAlertClass(t, alerts, "contract-hmac-readiness")
}

// No new measurement is not deletion of an earlier still-current verdict.
// Build the stored row through the actual model state machine before SQL.
func TestHMACCutoverSqlUnmeasuredPreservesPriorVerdict(t *testing.T) {
	clock := storedContractHMACCutover().Add(24 * time.Hour)
	checkedAt := clock.Add(-time.Hour)
	firstFailedAt := checkedAt.Add(-30 * time.Minute)
	pass := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, OK: true}
	for _, prior := range []*model.ProviderBlackholeCheck{
		pass,
		{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &firstFailedAt},
	} {
		retained, changed := model.NextProviderBlackholeCheck(prior, model.ProviderBlackholeCheckReport{
			CheckedAt: clock.Add(-time.Minute), NotMeasured: true, Failure: model.ProviderBlackholeNotMeasuredFailure,
		}, clock, model.DefaultProviderEgressRules())
		if !changed || retained.CheckedAt != prior.CheckedAt || retained.OK != prior.OK || retained.ConsecutiveFailures != prior.ConsecutiveFailures {
			t.Fatal("unmeasured fixture changed prior measured evidence")
		}
		alerts, snapshot := runHmacCutoverSqlFixture(t, clock, hmacCutoverSqlCheckRows(1, 20, retained), hmacCutoverSqlCheckRows(21, 40, pass), false)
		if snapshot.legacyChecked != 20 || (prior.OK && snapshot.legacyOK != 20) || (!prior.OK && snapshot.legacyDark != 20) {
			t.Fatal("unmeasured attempt erased the prior current verdict")
		}
		class := "contract-hmac-incompatible"
		if prior.OK {
			class = "contract-hmac-readiness"
		}
		requireAlertClass(t, alerts, class)
	}
}

// A date-ambiguous description is unknown even when its current check passed.
func TestHMACCutoverSqlUnknownMetadataCannotCertifyCompatibility(t *testing.T) {
	clock := storedContractHMACCutover().Add(24 * time.Hour)
	checkedAt := clock.Add(-time.Minute)
	firstFailedAt := checkedAt.Add(-30 * time.Minute)
	dark := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &firstFailedAt}
	pass := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, OK: true}
	alerts, snapshot := runHmacCutoverSqlFixture(t, clock, hmacCutoverSqlCheckRows(1, 20, dark), hmacCutoverSqlCheckRows(21, 40, pass), true)
	if snapshot.eligible != 60 || snapshot.legacy != 20 || snapshot.unknown != 40 || snapshot.compatible != 0 || snapshot.compatibleChecked != 0 {
		t.Fatal("unknown metadata was attributed to a compatible control")
	}
	requireAlertClass(t, alerts, "contract-hmac-readiness")
}
