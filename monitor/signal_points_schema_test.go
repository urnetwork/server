package monitor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The absent-column source returns the same error class as an old schema
// only if the unsafe data statement is issued. No production table is read.
func pointsSchemaFixture(snapshot, schema []Row, schemaErr error, forbidData bool) (SignalSettings, *[]string, *int) {
	queries := []string{}
	dataCalls := 0
	settings := syntheticSettings(nil)
	settings.Source = &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		queries = append(queries, query)
		switch {
		case strings.Contains(query, "FROM network_points_leaderboard_snapshot"):
			return snapshot, nil
		case strings.Contains(query, "FROM information_schema.columns"):
			return schema, schemaErr
		case strings.Contains(query, "FROM account_point AS point"):
			dataCalls++
			if forbidData {
				return nil, errors.New("synthetic absent completion column")
			}
			return syntheticPointsOperatorSource(12, 60, 0, 0, 0), nil
		default:
			return nil, errors.New("synthetic unexpected query")
		}
	}}
	return settings, &queries, &dataCalls
}

func TestPointsOperatorSchemaMissingColumnCompletesOneShotWithWarning(t *testing.T) {
	for _, age := range []int64{60, 7201} {
		settings, queries, dataCalls := pointsSchemaFixture(
			syntheticPointsSnapshot(age, 12, true, 10, 0), []Row{{"false"}}, nil, true)
		alerts, err := NewWithSignals(settings, NewPointsReadinessSignal()).Run(context.Background())
		if err != nil {
			t.Fatalf("known absent schema aborts one-shot instead of returning readiness evidence: %v", err)
		}
		alert := requireAlertClass(t, alerts, "points-operator-schema-unavailable")
		if alert.Severity != SeverityWarn || *dataCalls != 0 || len(*queries) != 2 {
			t.Fatal("absent schema was queried, paged, or hidden")
		}
		for _, want := range []string{"operator_rollup_readiness=unknown", "migration=686", "not proof", "Do not apply migrations"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("schema readiness boundary missing %q", want)
			}
		}
		if age > 7200 {
			requireAlertClass(t, alerts, "points-leaderboard-stale")
		} else if len(alerts) != 1 {
			t.Fatal("unknown schema fabricated additional source findings")
		}
		if pointsStTableReferencePattern.MatchString(strings.Join(*queries, "\n")) {
			t.Fatal("schema absence fell back to ST")
		}
	}
}

func TestPointsOperatorSchemaPresentHealthyControl(t *testing.T) {
	settings, _, dataCalls := pointsSchemaFixture(
		syntheticPointsSnapshot(60, 12, true, 10, 0), []Row{{"true"}}, nil, false)
	alerts, err := NewWithSignals(settings, NewPointsReadinessSignal()).Run(context.Background())
	if err != nil || len(alerts) != 0 || *dataCalls != 1 {
		t.Fatal("present-schema healthy control changed")
	}
}

func TestPointsOperatorSchemaMissingRetainsAtomicSnapshotPage(t *testing.T) {
	settings, queries, dataCalls := pointsSchemaFixture(
		[]Row{{"60", "12", "10", "true", "9", "0", "0", "0"}}, []Row{{"false"}}, nil, true)
	alerts, err := NewWithSignals(settings, NewPointsReadinessSignal()).Run(context.Background())
	if err != nil || *dataCalls != 0 || len(*queries) != 1 {
		t.Fatal("snapshot atomicity control reached dependent schema/data")
	}
	if requireAlertClass(t, alerts, "points-leaderboard-incomplete").Severity != SeverityPage {
		t.Fatal("missing operator schema suppressed an independent snapshot PAGE")
	}
}

func TestPointsOperatorSchemaMalformedAndReadErrorsFailClosed(t *testing.T) {
	for _, rows := range [][]Row{nil, {{"true"}, {"false"}}, {{"true", "extra"}}, {{"synthetic-private-value"}}} {
		settings, _, dataCalls := pointsSchemaFixture(
			syntheticPointsSnapshot(60, 12, true, 10, 0), rows, nil, false)
		alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
		if err == nil || len(alerts) != 0 || *dataCalls != 0 || strings.Contains(err.Error(), "synthetic-private-value") {
			t.Fatal("malformed preflight became source health or leaked private content")
		}
	}
	readErr := errors.New("synthetic catalog read failure")
	settings, _, dataCalls := pointsSchemaFixture(
		syntheticPointsSnapshot(60, 12, true, 10, 0), nil, readErr, false)
	alerts, err := NewWithSignals(settings, NewPointsReadinessSignal()).Run(context.Background())
	if !errors.Is(err, readErr) || *dataCalls != 0 || len(alerts) != 1 {
		t.Fatal("one-shot hid a real preflight execution failure")
	}
	if alerts[0].Class == "points-operator-schema-unavailable" {
		t.Fatal("a failed catalog read was converted into observed schema absence")
	}
}

// Execute the exact catalog expression over synthetic rows: no DDL, payment
// rows, or production metadata. This catches false positives from wrong type,
// nullable/default drift and wrong-schema lookalikes.
func TestPointsOperatorSchemaSqlMatchesPhysicalContract(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("schema SQL controls require the attested local test environment")
	}
	settings, queries, _ := pointsSchemaFixture(
		syntheticPointsSnapshot(60, 12, true, 10, 0), []Row{{"true"}}, nil, false)
	if _, err := NewPointsReadinessSignal().Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	expression := ""
	for _, query := range *queries {
		if strings.Contains(query, "FROM information_schema.columns") {
			expression = strings.ReplaceAll(query, "information_schema.columns", "synthetic_columns")
		}
	}
	if expression == "" {
		t.Fatal("operator readiness has no parse-safe physical-schema preflight")
	}
	for _, control := range []struct {
		values string
		want   bool
	}{
		{"('public','account_payment','block_rollup_complete','boolean','NO','false')", true},
		{"('public','account_payment','block_rollup_complete','boolean','NO','false::boolean')", true},
		{"('public','account_payment','unrelated_column','boolean','NO','false')", false},
		{"('private','account_payment','block_rollup_complete','boolean','NO','false')", false},
		{"('public','unrelated_table','block_rollup_complete','boolean','NO','false')", false},
		{"('public','account_payment','block_rollup_complete','text','NO','false')", false},
		{"('public','account_payment','block_rollup_complete','boolean','YES','false')", false},
		{"('public','account_payment','block_rollup_complete','boolean','NO','true')", false},
	} {
		query := "WITH synthetic_columns(table_schema,table_name,column_name,data_type,is_nullable,column_default) AS (VALUES " + control.values + ") " + expression
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var observed bool
		server.Db(ctx, func(conn server.PgConn) {
			if err := conn.QueryRow(ctx, query).Scan(&observed); err != nil {
				t.Fatal(err)
			}
		})
		cancel()
		if observed != control.want {
			t.Fatal("physical schema lookalike passed or healthy shape was rejected")
		}
	}
}

func TestPointsOperatorSchemaCatalogQualifiesStagedRollout(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	start := strings.Index(content, "### 17.6 ")
	if start < 0 {
		t.Fatal("missing catalog section")
	}
	end := strings.Index(content[start:], "\n## 18.")
	if end < 0 {
		t.Fatal("missing catalog boundary")
	}
	section := strings.Join(strings.Fields(content[start:start+end]), " ")
	for _, want := range []string{
		"points-operator-schema-unavailable", "one-shot retains the warning and can complete",
		"not automatic permission or necessity to migrate", "exact deployed artifacts",
		"Preflight read errors or malformed rows still fail closed",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("catalog missing %q", want)
		}
	}
}
