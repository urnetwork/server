package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

var pointsStTableReferencePattern = regexp.MustCompile(`(?i)\b(?:from|join)\s+(?:[a-z_][a-z0-9_]*\.)?st_epoch\b`)

// All identities and aggregates here are synthetic. Legacy ST queries return
// no history so the unchanged signal compiles and reaches the semantic RED.
func runPointsOperatorFixture(t testing.TB, settings SignalSettings, snapshot, source []Row) (Alerts, []string, error) {
	t.Helper()
	queries := []string{}
	settings.Source = &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		queries = append(queries, query)
		switch {
		case strings.Contains(query, "FROM network_points_leaderboard_snapshot"):
			return snapshot, nil
		case pointsStTableReferencePattern.MatchString(query):
			return nil, nil
		case strings.Contains(query, "FROM account_point AS point"):
			return source, nil
		default:
			t.Fatalf("unexpected points query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
	return alerts, queries, err
}

func syntheticPointsSnapshot(age int64, block uint64, available bool, rows uint64, positive uint64) []Row {
	return []Row{{fmt.Sprint(age), fmt.Sprint(block), fmt.Sprint(rows), fmt.Sprint(available), fmt.Sprint(rows), fmt.Sprint(positive), fmt.Sprint(positive), fmt.Sprint(positive)}}
}

func syntheticPointsOperatorSource(block uint64, closeAge int64, missing, pending uint64, pendingAge int64) []Row {
	return []Row{{fmt.Sprint(block), fmt.Sprint(closeAge), fmt.Sprint(missing), fmt.Sprint(pending), fmt.Sprint(pendingAge)}}
}

func requirePointsOperatorNoPage(t testing.TB, alerts Alerts) {
	t.Helper()
	for _, alert := range alerts {
		if alert.Severity == SeverityPage {
			t.Fatalf("unproved operator handoff became PAGE: %s", alert.Markdown())
		}
	}
}

func TestPointsOperatorReadinessAvailableWithoutSt(t *testing.T) {
	for _, status := range []STConfigurationStatus{STConfigurationExplicitDisabled, STConfigurationUnavailable, STConfigurationEnabledInvalid, STConfigurationEnabled} {
		for _, positive := range []uint64{0, 4} {
			settings := syntheticSettings(nil)
			settings.STConfigStatus = status
			settings.VerificationEnabled = status == STConfigurationEnabled || status == STConfigurationEnabledInvalid
			if status == STConfigurationEnabled {
				settings.STDeploymentKey = "synthetic:unrelated-st-deployment"
			}
			alerts, queries, err := runPointsOperatorFixture(t, settings,
				syntheticPointsSnapshot(60, 12, true, 10, positive),
				syntheticPointsOperatorSource(12, 10000, 0, 0, 0))
			if err != nil || len(alerts) != 0 {
				t.Fatalf("valid operator snapshot depends on ST state %s (positive=%d): alerts=%+v err=%v", status, positive, alerts, err)
			}
			if len(queries) != 2 || pointsStTableReferencePattern.MatchString(strings.Join(queries, "\n")) {
				t.Fatal("operator readiness still reads chain epoch history")
			}
		}
	}
}

func TestPointsOperatorReadinessPendingRollupIsNotStReadiness(t *testing.T) {
	settings := syntheticSettings(nil)
	settings.STConfigStatus = STConfigurationExplicitDisabled
	alerts, _, err := runPointsOperatorFixture(t, settings,
		syntheticPointsSnapshot(60, 0, false, 10, 0), syntheticPointsOperatorSource(12, 10000, 0, 3, 950400))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "points-epoch-metrics-unavailable")
	requirePointsOperatorNoPage(t, alerts)
	for _, want := range []string{"source=operator-paid-traffic", "pending_rollup_points=3", "oldest_pending_point_age_seconds=950400", "not stalled duration", "bounded rollup batches"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("rollup diagnosis missing %q: %s", want, alert.Markdown())
		}
	}
	for _, wrong := range []string{"because the Main ST", "finalized ST epoch", "obtain the first legitimate finalized epoch"} {
		if strings.Contains(alert.Markdown(), wrong) {
			t.Fatalf("operator diagnosis retained unrelated closure %q", wrong)
		}
	}
}

func TestPointsOperatorReadinessMissingPaymentIsVisible(t *testing.T) {
	alerts, _, err := runPointsOperatorFixture(t, syntheticSettings(nil),
		syntheticPointsSnapshot(60, 0, false, 10, 0), syntheticPointsOperatorSource(12, 10000, 2, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "points-epoch-metrics-unavailable")
	if !strings.Contains(alert.Observed, "missing_payment_points=2 pending_rollup_points=0") || !strings.Contains(alert.Action, "missing links require source diagnosis") {
		t.Fatalf("missing payment evidence was lost: %s", alert.Markdown())
	}
	requirePointsOperatorNoPage(t, alerts)
}

func TestPointsOperatorReadinessNewPaymentDoesNotInvalidateSnapshot(t *testing.T) {
	for _, block := range []uint64{11, 12} {
		alerts, _, err := runPointsOperatorFixture(t, syntheticSettings(nil),
			syntheticPointsSnapshot(60, block, true, 10, 4), syntheticPointsOperatorSource(12, 10000, 0, 1, 30))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "points-block-rollup-incomplete")
		requirePointsOperatorNoPage(t, alerts)
		if !strings.Contains(alert.Mechanism, "not invalidated by a later pending payment") {
			t.Fatalf("immutable snapshot boundary is missing: %s", alert.Markdown())
		}
	}
}

func TestPointsOperatorReadinessUnknownCompletionRemainsPending(t *testing.T) {
	for _, available := range []bool{false, true} {
		for _, snapshotAge := range []int64{60, 7300} {
			block := uint64(0)
			if available {
				block = 11
			}
			alerts, _, err := runPointsOperatorFixture(t, syntheticSettings(nil),
				syntheticPointsSnapshot(snapshotAge, block, available, 10, 0), syntheticPointsOperatorSource(12, 604700, 0, 0, 0))
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, "points-epoch-rebuild-pending")
			requirePointsOperatorNoPage(t, alerts)
			if !strings.Contains(alert.Observed, "rollup_completion_age=unknown") || !strings.Contains(alert.Mechanism, "old Sunday boundary cannot prove") {
				t.Fatalf("completion provenance was promoted to certainty: %s", alert.Markdown())
			}
			if snapshotAge > 7200 {
				requireAlertClass(t, alerts, "points-leaderboard-stale")
			}
		}
	}
}

func TestPointsOperatorReadinessFirstSundayTransition(t *testing.T) {
	for _, state := range []struct {
		closed    uint64
		published uint64
		available bool
		wantClass string
	}{
		{closed: 0, published: 0, available: false},
		{closed: 1, published: 0, available: false, wantClass: "points-epoch-rebuild-pending"},
		{closed: 1, published: 1, available: true},
	} {
		alerts, _, err := runPointsOperatorFixture(t, syntheticSettings(nil),
			syntheticPointsSnapshot(0, state.published, state.available, 10, 0), syntheticPointsOperatorSource(state.closed, 0, 0, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if state.wantClass == "" {
			if len(alerts) != 0 {
				t.Fatalf("valid operator calendar state %+v alerted: %+v", state, alerts)
			}
		} else {
			requireAlertClass(t, alerts, state.wantClass)
		}
		requirePointsOperatorNoPage(t, alerts)
	}
}

func TestPointsOperatorReadinessInternalContradictionsStillPage(t *testing.T) {
	for _, state := range []struct {
		block     uint64
		available bool
		positive  uint64
		class     string
	}{
		{block: 1, available: false, class: "points-epoch-availability-drift"},
		{block: 0, available: false, positive: 1, class: "points-epoch-availability-drift"},
		{block: 0, available: true, class: "points-epoch-availability-drift"},
		{block: 13, available: true, class: "points-epoch-snapshot-drift"},
	} {
		alerts, _, err := runPointsOperatorFixture(t, syntheticSettings(nil),
			syntheticPointsSnapshot(60, state.block, state.available, 10, state.positive), syntheticPointsOperatorSource(12, 10000, 0, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, state.class)
		if alert.Severity != SeverityPage {
			t.Fatalf("same-snapshot/calendar contradiction was suppressed: %+v", alert)
		}
	}
}

// These existing atomicity safeguards must pass even on the ST-coupled RED.
func TestPointsOperatorReadinessAtomicControl(t *testing.T) {
	for _, state := range []struct {
		snapshot []Row
		class    string
	}{
		{snapshot: nil, class: "points-leaderboard-unavailable"},
		{snapshot: []Row{{"60", "1", "10", "true", "9", "0", "0", "0"}}, class: "points-leaderboard-incomplete"},
		{snapshot: syntheticPointsSnapshot(60, 1, false, 10, 1), class: "points-epoch-availability-drift"},
	} {
		alerts, _, err := runPointsOperatorFixture(t, syntheticSettings(nil), state.snapshot, syntheticPointsOperatorSource(12, 10000, 0, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, state.class)
	}
}

func TestPointsOperatorReadinessRetainsSnapshotIntegrityAndFreshness(t *testing.T) {
	for _, state := range []struct {
		snapshot []Row
		class    string
	}{
		{snapshot: nil, class: "points-leaderboard-unavailable"},
		{snapshot: []Row{{"60", "12", "10", "true", "9", "0", "0", "0"}}, class: "points-leaderboard-incomplete"},
		{snapshot: syntheticPointsSnapshot(7201, 12, true, 10, 0), class: "points-leaderboard-stale"},
		{snapshot: syntheticPointsSnapshot(-1, 12, true, 10, 0), class: "points-leaderboard-stale"},
		{snapshot: syntheticPointsSnapshot(7200, 12, true, 10, 0)},
	} {
		alerts, _, err := runPointsOperatorFixture(t, syntheticSettings(nil), state.snapshot, syntheticPointsOperatorSource(12, 10000, 0, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if state.class == "" {
			if len(alerts) != 0 {
				t.Fatalf("exact snapshot age boundary alerted: %+v", alerts)
			}
			continue
		}
		alert := requireAlertClass(t, alerts, state.class)
		if state.class == "points-leaderboard-stale" && alert.Sustain != 2 {
			t.Fatalf("stale snapshot sustain changed: %d", alert.Sustain)
		}
	}
}

func TestPointsOperatorReadinessMissingOrMalformedSourceFailsClosed(t *testing.T) {
	for _, rows := range [][]Row{
		nil,
		{{"12", "30", "0", "0", "0"}, {"12", "30", "0", "0", "0"}},
		{{"12", "30", "0", "0"}},
		{{"synthetic-private-value", "30", "0", "0", "0"}},
		{{"12", "-1", "0", "0", "0"}},
		{{"12", "604800", "0", "0", "0"}},
		{{"0", "1", "0", "0", "0"}},
		{{"12", "30", "-1", "0", "0"}},
		{{"12", "30", "0", "0", "1"}},
	} {
		alerts, _, err := runPointsOperatorFixture(t, syntheticSettings(nil), syntheticPointsSnapshot(60, 12, true, 10, 0), rows)
		if err == nil || len(alerts) != 0 {
			t.Fatalf("malformed/missing source became a verdict: alerts=%+v err=%v", alerts, err)
		}
		if strings.Contains(err.Error(), "synthetic-private-value") {
			t.Fatal("malformed source content escaped through a parser error")
		}
	}
}

func TestPointsOperatorReadinessReadFailureNeverFallsBackToSt(t *testing.T) {
	settings := syntheticSettings(nil)
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic:unrelated-st-deployment"
	readErr := errors.New("synthetic bounded rollup read failure")
	settings.Source = &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM network_points_leaderboard_snapshot") {
			return syntheticPointsSnapshot(60, 12, true, 10, 0), nil
		}
		if pointsStTableReferencePattern.MatchString(query) {
			t.Fatal("rollup read failure may not fall back to ST")
		}
		return nil, readErr
	}}
	alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
	if !errors.Is(err, readErr) || len(alerts) != 0 {
		t.Fatalf("rollup read error became health: alerts=%+v err=%v", alerts, err)
	}
}

func TestPointsOperatorStTableDiscriminator(t *testing.T) {
	for _, state := range []struct {
		query string
		want  bool
	}{
		{query: "SELECT latest_epoch FROM network_points_leaderboard_snapshot", want: false},
		{query: "SELECT latest_epoch FROM public.st_epoch", want: true},
		{query: "SELECT epoch FROM\n st_epoch", want: true},
		{query: "SELECT epoch FROM source JOIN st_epoch ON true", want: true},
		{query: "SELECT latest_epoch FROM st_epoch_extra", want: false},
	} {
		if got := pointsStTableReferencePattern.MatchString(state.query); got != state.want {
			t.Fatalf("ST table discriminator got %t want %t for synthetic query %q", got, state.want, state.query)
		}
	}
}

func TestPointsOperatorReadinessRejectsMalformedSnapshot(t *testing.T) {
	for _, snapshot := range [][]Row{
		{{"60", "12", "10", "synthetic-invalid", "10", "0", "0", "0"}},
		{{"60", "12", "10", "true", "10", "11", "0", "0"}},
		{{"60", "12", "10", "true", "10", "0", "0"}},
	} {
		alerts, queries, err := runPointsOperatorFixture(t, syntheticSettings(nil), snapshot, syntheticPointsOperatorSource(12, 30, 0, 0, 0))
		if err == nil || len(alerts) != 0 || len(queries) != 1 {
			t.Fatalf("malformed snapshot escaped: alerts=%+v queries=%d err=%v", alerts, len(queries), err)
		}
	}
}

// Capture the exact production statement through the public signal, then
// shadow its tables with synthetic CTEs. No fixture writes or live data reads.
func pointsOperatorSqlFixture(t testing.TB, clock time.Time, pointCte string) [5]int64 {
	t.Helper()
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("operator rollup SQL fixture requires the attested local test environment")
	}
	_, queries, err := runPointsOperatorFixture(t, syntheticSettings(nil), syntheticPointsSnapshot(60, 1, true, 10, 0), syntheticPointsOperatorSource(1, 60, 0, 0, 0))
	if err != nil || len(queries) != 2 || !strings.Contains(queries[1], "FROM account_point AS point") {
		t.Fatalf("operator source statement absent: queries=%d err=%v", len(queries), err)
	}
	query := strings.Replace(queries[1], "WITH lifecycle_clock AS MATERIALIZED", pointCte+", lifecycle_clock AS MATERIALIZED", 1)
	query = strings.ReplaceAll(query, "clock_timestamp()", "timestamptz '"+clock.UTC().Format(time.RFC3339Nano)+"'")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var values [5]int64
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("operator source lost its single-row calendar aggregate")
		}
		if err := rows.Scan(&values[0], &values[1], &values[2], &values[3], &values[4]); err != nil {
			t.Fatal(err)
		}
		if rows.Next() || rows.Err() != nil {
			t.Fatalf("operator source row shape changed: %v", rows.Err())
		}
	})
	return values
}

func TestPointsOperatorSourceSqlSundayCalendar(t *testing.T) {
	const emptyPoints = `WITH account_payment(payment_id, block_rollup_complete) AS (
	 SELECT 1, true WHERE false
	), account_point(account_payment_id, point_value, create_time) AS (
	 SELECT 1, 1, timestamp '2000-01-01' WHERE false
	)`
	genesis := model.SubnetBlockGenesis
	for _, state := range []struct {
		clock  time.Time
		closed int64
		age    int64
	}{
		{clock: genesis.Add(-time.Second), closed: 0, age: 0},
		{clock: genesis, closed: 0, age: 0},
		{clock: genesis.Add(model.SubnetBlockDuration - time.Microsecond), closed: 0, age: 0},
		{clock: genesis.Add(model.SubnetBlockDuration), closed: 1, age: 0},
		{clock: genesis.Add(2*model.SubnetBlockDuration - time.Second), closed: 1, age: 604799},
		{clock: genesis.Add(2*model.SubnetBlockDuration + time.Second), closed: 2, age: 1},
	} {
		values := pointsOperatorSqlFixture(t, state.clock, emptyPoints)
		if values != [5]int64{state.closed, state.age, 0, 0, 0} {
			t.Fatalf("UTC operator boundary %s got %v, want closed=%d age=%d", state.clock, values, state.closed, state.age)
		}
	}
}

func TestPointsOperatorSourceSqlMatchesCompletenessEligibility(t *testing.T) {
	genesis := model.SubnetBlockGenesis
	pointCte := fmt.Sprintf(`WITH account_payment(payment_id, block_rollup_complete) AS (
	 VALUES (1, true), (2, false)
	), account_point(account_payment_id, point_value, create_time) AS (
	 VALUES (1, 10, timestamp '%[1]s'),
	        (2, 10, timestamp '%[2]s'),
	        (3, 10, timestamp '%[2]s'),
	        (NULL::integer, 10, timestamp '%[2]s'),
	        (2, 10, timestamp '%[3]s'),
	        (2, 0, timestamp '%[2]s'),
	        (2, -10, timestamp '%[2]s')
	)`, genesis.Format("2006-01-02 15:04:05"), genesis.Add(10*24*time.Hour).Format("2006-01-02 15:04:05"), genesis.Add(-time.Second).Format("2006-01-02 15:04:05"))
	values := pointsOperatorSqlFixture(t, genesis.Add(21*24*time.Hour+time.Hour), pointCte)
	if values != [5]int64{3, 3600, 2, 1, 11*24*3600 + 3600} {
		t.Fatalf("rollup census lost missing/pending or post-genesis positive eligibility: %v", values)
	}
}

func TestPointsOperatorReadinessDocumentation(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	start := strings.Index(content, "### 17.6 ")
	if start < 0 {
		t.Fatal("points catalog section missing")
	}
	end := strings.Index(content[start:], "\n## 18.")
	if end < 0 {
		t.Fatal("points catalog section missing")
	}
	section := strings.Join(strings.Fields(content[start:start+end]), " ")
	for _, want := range []string{
		"Sunday-00 UTC", "seven-day", "operator paid-traffic", "ST is not a prerequisite",
		"missing_payment_points", "pending_rollup_points", "no persisted completion timestamp",
		"new pending payment does not invalidate an earlier snapshot", "points-block-rollup-incomplete",
		"old Sunday boundary alone cannot prove an overdue rebuild", "source-version boundary",
		"Do not enable ST", "signal_points_readiness_test.go",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("operator readiness catalog missing %q", want)
		}
	}
	for _, stale := range []string{"come only from **finalized epochs", "Closure requires the reviewed Main ST deployment", "only the reviewed ST/finalized-epoch operational closure"} {
		if strings.Contains(section, stale) {
			t.Fatalf("active catalog retains superseded ST closure %q", stale)
		}
	}
}
