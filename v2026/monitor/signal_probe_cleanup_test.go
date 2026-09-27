package monitor

import (
	"context"
	"strconv"
	"strings"
	"testing"
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
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("reached-channel alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if hasProbeCleanupAlertClass(alerts, "probe-unused-args-retirement") {
		t.Fatalf("reached-channel residual raised the unused-args class: %+v", alerts)
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
