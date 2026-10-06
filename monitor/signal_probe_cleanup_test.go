package monitor

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func syntheticProbeCleanupSource(row Row) *syntheticSource {
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.Contains(query, "monitor-signal-2.25-probe-cleanup") {
			return nil, nil
		}
		return []Row{row}, nil
	}}
}

func hasProbeCleanupAlertClass(alerts []Alert, class string) bool {
	for _, alert := range alerts {
		if alert.Class == class {
			return true
		}
	}
	return false
}

func TestProbeCleanupSignalSyntheticSevereUnusedArgsLeak(t *testing.T) {
	row := Row{"1", "64556", "63174", "5872", "14", "57288", "1246", "0", "21599", "57288", "0"}
	alerts, err := NewProbeCleanupSignal().Run(
		context.Background(), syntheticSettings(syntheticProbeCleanupSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "probe-unused-args-retirement")
	if alert.Severity != SeverityPage {
		t.Fatalf("severity=%s, want page", alert.Severity)
	}
	for _, want := range []string{
		"57288 of 63174 mature",
		"never opened a connection",
		"created_6h=64556",
		"mature_inactive=5872",
		"mature_active_connected=14",
		"mature_active_disconnected=57288",
		"mature_active_disconnected_percent=90.7",
		"mature_active_disconnected_never_connected=57288",
		"mature_active_disconnected_ever_connected=0",
		"oldest_active_disconnected_age_seconds=21599",
		"direct RemoveClientArgs calls",
		"retirement-admitted",
		"CloseAndWait joins it",
		"Do not bulk-deactivate production rows",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("cleanup alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if hasProbeCleanupAlertClass(alerts, "probe-child-retirement") {
		t.Fatalf("never-connected residual raised the reached-channel class: %+v", alerts)
	}
}

func TestProbeCleanupSignalSyntheticReachedChannelLeak(t *testing.T) {
	row := Row{"1", "250", "200", "170", "10", "20", "25", "0", "901", "0", "20"}
	alerts, err := NewProbeCleanupSignal().Run(
		context.Background(), syntheticSettings(syntheticProbeCleanupSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "probe-child-retirement")
	if alert.Severity != SeverityPage {
		t.Fatalf("severity=%s, want page", alert.Severity)
	}
	for _, want := range []string{
		"20 of 200 mature",
		"previously opened a connection",
		"reached-channel teardown branch",
		"ordered tunnel close",
		"Server's qualityprobe tunnel code",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("reached-channel alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if hasProbeCleanupAlertClass(alerts, "probe-unused-args-retirement") {
		t.Fatalf("reached-channel residual raised the unused-args class: %+v", alerts)
	}
}

func TestProbeCleanupSignalSyntheticSparseUnusedArgsKeepsCauseUnknown(t *testing.T) {
	// A small residual in an otherwise retired recent cohort must retain its
	// own warning without claiming a specific mint or cleanup step failed.
	row := Row{"1", "903514", "877732", "877715", "0", "17", "751", "0", "20162", "17", "0"}
	alerts, err := NewProbeCleanupSignal().Run(
		context.Background(), syntheticSettings(syntheticProbeCleanupSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "probe-unused-args-retirement")
	if alert.Severity != SeverityWarn {
		t.Fatalf("sparse residual severity=%s, want warn", alert.Severity)
	}
	for _, want := range []string{
		"17 of 877732 mature",
		"The aggregate does not identify which mint or cleanup step failed",
		"Compare the running artifact",
		"optional Redis cache fill",
		"committed child identity",
		"separate PostgreSQL connection",
		"before and after commit",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("sparse cleanup alert missing causal qualifier %q", want)
		}
	}
	if hasProbeCleanupAlertClass(alerts, "probe-child-retirement") {
		t.Fatal("never-connected residual was assigned to reached-channel teardown")
	}
}

func TestProbeCleanupSignalSyntheticBranchesAlertIndependently(t *testing.T) {
	row := Row{"1", "250", "200", "169", "10", "21", "25", "0", "901", "1", "20"}
	alerts, err := NewProbeCleanupSignal().Run(
		context.Background(), syntheticSettings(syntheticProbeCleanupSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	reachedChannel := requireAlertClass(t, alerts, "probe-child-retirement")
	if reachedChannel.Severity != SeverityPage {
		t.Fatalf("reached-channel severity=%s, want page", reachedChannel.Severity)
	}
	unusedArgs := requireAlertClass(t, alerts, "probe-unused-args-retirement")
	if unusedArgs.Severity != SeverityWarn {
		t.Fatalf("unused-args severity=%s, want warn", unusedArgs.Severity)
	}
}

func TestProbeCleanupSignalSyntheticThresholds(t *testing.T) {
	tests := []struct {
		name               string
		matureCreated      int64
		matureInactive     int64
		matureConnected    int64
		matureDisconnected int64
		wantAlert          bool
		wantSeverity       Severity
	}{
		{name: "empty cohort"},
		{name: "connected is in flight", matureCreated: 200, matureInactive: 190, matureConnected: 10},
		{name: "one residual warns", matureCreated: 200, matureInactive: 190, matureConnected: 9, matureDisconnected: 1, wantAlert: true, wantSeverity: SeverityWarn},
		{name: "small cohort residual warns", matureCreated: 10, matureInactive: 9, matureDisconnected: 1, wantAlert: true, wantSeverity: SeverityWarn},
		{name: "page boundary", matureCreated: 200, matureInactive: 180, matureDisconnected: 20, wantAlert: true, wantSeverity: SeverityPage},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			created := testCase.matureCreated + 50
			oldest := int64(0)
			if testCase.matureDisconnected > 0 {
				oldest = 601
			}
			row := Row{
				"1",
				strconv.FormatInt(created, 10),
				strconv.FormatInt(testCase.matureCreated, 10),
				strconv.FormatInt(testCase.matureInactive, 10),
				strconv.FormatInt(testCase.matureConnected, 10),
				strconv.FormatInt(testCase.matureDisconnected, 10),
				"25", "0", strconv.FormatInt(oldest, 10),
				"0", strconv.FormatInt(testCase.matureDisconnected, 10),
			}
			alerts, err := NewProbeCleanupSignal().Run(
				context.Background(), syntheticSettings(syntheticProbeCleanupSource(row)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !testCase.wantAlert {
				if len(alerts) != 0 {
					t.Fatalf("alerts=%+v, want healthy", alerts)
				}
				return
			}
			alert := requireAlertClass(t, alerts, "probe-child-retirement")
			if alert.Severity != testCase.wantSeverity {
				t.Fatalf("severity=%s, want %s", alert.Severity, testCase.wantSeverity)
			}
		})
	}
}

func TestProbeCleanupSignalSyntheticMissingIdentity(t *testing.T) {
	row := Row{"0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0"}
	alerts, err := NewProbeCleanupSignal().Run(
		context.Background(), syntheticSettings(syntheticProbeCleanupSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "probe-child-retirement-identity")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Markdown(), "Guessing from a description") {
		t.Fatalf("identity warning=%s", alert.Markdown())
	}
}

func TestProbeCleanupSignalSyntheticMissingDeactivationTime(t *testing.T) {
	row := Row{"1", "100", "50", "50", "0", "0", "25", "3", "0", "0", "0"}
	alerts, err := NewProbeCleanupSignal().Run(
		context.Background(), syntheticSettings(syntheticProbeCleanupSource(row)),
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "probe-child-retirement-integrity")
	if alert.Severity != SeverityWarn {
		t.Fatalf("severity=%s, want warn", alert.Severity)
	}
	for _, want := range []string{
		"3 recently inactive",
		"inactive_without_deactivate_time=3",
		"changed active without setting deactivate_time",
		"do not backfill or delete production rows",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("integrity alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestParseProbeCleanupSnapshotRejectsAmbiguity(t *testing.T) {
	tests := []struct {
		name string
		rows []pgRow
	}{
		{name: "missing"},
		{name: "bad shape", rows: []pgRow{{"1", "1"}}},
		{name: "bad authority", rows: []pgRow{{"2", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0"}}},
		{name: "negative", rows: []pgRow{{"1", "-1", "0", "0", "0", "0", "0", "0", "0", "0", "0"}}},
		{name: "mature partition", rows: []pgRow{{"1", "10", "5", "2", "1", "1", "0", "0", "601", "0", "1"}}},
		{name: "mature exceeds recent", rows: []pgRow{{"1", "4", "5", "3", "1", "1", "0", "0", "601", "0", "1"}}},
		{name: "fresh exceeds remainder", rows: []pgRow{{"1", "10", "5", "3", "1", "1", "6", "0", "601", "0", "1"}}},
		{name: "age without residual", rows: []pgRow{{"1", "10", "5", "4", "1", "0", "5", "0", "601", "0", "0"}}},
		{name: "residual history partition", rows: []pgRow{{"1", "10", "5", "3", "1", "1", "5", "0", "601", "1", "1"}}},
		{name: "population without authority", rows: []pgRow{{"0", "1", "0", "0", "0", "0", "1", "0", "0", "0", "0"}}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseProbeCleanupSnapshot(testCase.rows); err == nil {
				t.Fatal("contradictory cleanup aggregate was accepted")
			}
		})
	}
}

func TestProbeCleanupQueryIsBoundedAndPrivate(t *testing.T) {
	query := probeCleanupQuery()
	for _, want := range []string{
		"monitor-signal-2.25-probe-cleanup",
		"FROM prober_identity",
		"nc.network_id = p.network_id",
		"nc.source_client_id = p.client_id",
		"ncc.connected",
		"interval '6 hours'",
		"interval '10 minutes'",
		"inactive_without_deactivate_time",
		"residual_connection_history AS MATERIALIZED",
		"mature_active_disconnected_never_connected",
		"mature_active_disconnected_ever_connected",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("cleanup query missing %q:\n%s", want, query)
		}
	}
	finalSelect := query[strings.LastIndex(query, "SELECT\n    (SELECT count(*) FROM prober)"):]
	for _, forbidden := range []string{"network_id", "client_id", "connection_id", "by_client_jwt"} {
		if strings.Contains(finalSelect, forbidden) {
			t.Fatalf("final cleanup aggregate exports %q:\n%s", forbidden, finalSelect)
		}
	}
}

func TestProbeCleanupQueryMatureActiveOnlyExactAggregate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE TEMP TABLE prober_identity (singleton boolean, network_id text, client_id text) ON COMMIT DROP;
				CREATE TEMP TABLE network_client (
					client_id text, network_id text, source_client_id text,
					active boolean, create_time timestamp, deactivate_time timestamp
				) ON COMMIT DROP;
				CREATE TEMP TABLE network_client_connection (client_id text, connected boolean) ON COMMIT DROP;
				INSERT INTO pg_temp.prober_identity VALUES (true, 'synthetic-network', 'synthetic-parent');
				INSERT INTO pg_temp.network_client VALUES
					('inactive-no-time', 'synthetic-network', 'synthetic-parent', false, statement_timestamp() AT TIME ZONE 'UTC' - interval '1 hour', NULL),
					('inactive-with-time', 'synthetic-network', 'synthetic-parent', false, statement_timestamp() AT TIME ZONE 'UTC' - interval '2 hours', statement_timestamp() AT TIME ZONE 'UTC'),
					('active-connected', 'synthetic-network', 'synthetic-parent', true, statement_timestamp() AT TIME ZONE 'UTC' - interval '20 minutes', NULL),
					('active-never-connected', 'synthetic-network', 'synthetic-parent', true, statement_timestamp() AT TIME ZONE 'UTC' - interval '45 minutes', NULL),
					('active-ever-connected', 'synthetic-network', 'synthetic-parent', true, statement_timestamp() AT TIME ZONE 'UTC' - interval '30 minutes', NULL),
					('fresh-active', 'synthetic-network', 'synthetic-parent', true, statement_timestamp() AT TIME ZONE 'UTC' - interval '2 minutes', NULL),
					('fresh-inactive-no-time', 'synthetic-network', 'synthetic-parent', false, statement_timestamp() AT TIME ZONE 'UTC' - interval '2 minutes', NULL),
					('outside-six-hours', 'synthetic-network', 'synthetic-parent', true, statement_timestamp() AT TIME ZONE 'UTC' - interval '7 hours', NULL),
					('other-parent', 'synthetic-network', 'other-parent', true, statement_timestamp() AT TIME ZONE 'UTC' - interval '1 hour', NULL),
					('other-network', 'other-network', 'synthetic-parent', true, statement_timestamp() AT TIME ZONE 'UTC' - interval '1 hour', NULL);
				INSERT INTO pg_temp.network_client_connection VALUES
					('active-connected', true),
					('active-ever-connected', false),
					('fresh-active', true),
					('outside-six-hours', true);
			`))

			rows, err := tx.Query(ctx, probeCleanupQuery())
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("no cleanup aggregate")
				}
				values := make([]string, 11)
				dest := make([]any, len(values))
				for i := range values {
					dest[i] = &values[i]
				}
				server.Raise(rows.Scan(dest...))
				want := []string{"1", "7", "5", "2", "1", "2", "1", "2", "2700", "1", "1"}
				for i := range want {
					if values[i] != want[i] {
						t.Errorf("field %d = %s, want %s", i, values[i], want[i])
					}
				}
				if rows.Next() {
					t.Error("extra cleanup aggregate row")
				}
			})
		})
	})
}
