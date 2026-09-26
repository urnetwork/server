// Keeps automatic staging winners separate from production honesty review and
// exercises the emitted migration guard without changing any database objects.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Read independent published function bodies, including the superseded staging
// policy, so fixture health cannot be defined by the monitor's own expectation.
func syntheticStagingGuardSources(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile("../db_migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	guardNameDefinitions := map[string]string{}
	for _, guard := range []struct {
		name        string
		declaration string
		delimiter   string
		last        bool
	}{
		{name: "review", declaration: "CREATE FUNCTION competition_staging_candidate_review_guard()", delimiter: "$competition_staging_candidate_review_guard$"},
		{name: "winner", declaration: "CREATE OR REPLACE FUNCTION competition_round_honesty_review_guard()", delimiter: "$competition_round_honesty_review_gate$", last: true},
		{name: "previous_winner", declaration: "CREATE OR REPLACE FUNCTION competition_round_honesty_review_guard()", delimiter: "$competition_round_honesty_review_gate$"},
		{name: "historical", declaration: "CREATE OR REPLACE FUNCTION competition_round_honesty_review_guard()", delimiter: "$competition_round_honesty_review_gate$"},
	} {
		position := strings.Index(source, guard.declaration)
		if guard.last {
			position = strings.LastIndex(source, guard.declaration)
		} else if guard.name == "previous_winner" {
			latest := strings.LastIndex(source, guard.declaration)
			position = strings.LastIndex(source[:latest], guard.declaration)
		}
		if position < 0 {
			t.Fatalf("missing published %s guard", guard.name)
		}
		_, remaining, ok := strings.Cut(source[position:], "AS "+guard.delimiter)
		if !ok {
			t.Fatalf("missing published %s body start", guard.name)
		}
		definition, _, ok := strings.Cut(remaining, guard.delimiter)
		if !ok {
			t.Fatalf("missing published %s body end", guard.name)
		}
		guardNameDefinitions[guard.name] = definition
	}
	return guardNameDefinitions
}

// The migration bodies and emitted catalog collection must agree independently
// of synthetic artifact booleans, including historical nullable lookups.
func TestMigrationsSignalStagingWinnerPinsPublishedFunctions(t *testing.T) {
	guardNameDefinitions := syntheticStagingGuardSources(t)
	for _, guard := range []struct{ name, delimiter, query string }{
		{name: "review", delimiter: "$staging_review_body$", query: competitionStagingWinnerArtifactQuery},
		{name: "previous_winner", delimiter: "$staging_winner_body$", query: competitionStagingWinnerArtifactQuery},
		{name: "winner", delimiter: "$staging_winner_body$", query: competitionStagingBestWinnerArtifactQuery},
	} {
		_, remaining, ok := strings.Cut(guard.query, guard.delimiter)
		if !ok {
			t.Fatalf("missing %s function expectation", guard.name)
		}
		definition, _, ok := strings.Cut(remaining, guard.delimiter)
		if !ok || strings.Join(strings.Fields(definition), " ") != strings.Join(strings.Fields(guardNameDefinitions[guard.name]), " ") {
			t.Fatalf("%s monitor contract differs from its published migration body", guard.name)
		}
	}
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		if !strings.Contains(query, competitionStagingWinnerArtifactQuery) ||
			!strings.Contains(query, competitionStagingBestWinnerArtifactQuery) {
			t.Fatal("migration query does not execute both staging winner contracts")
		}
		normalized := strings.Join(strings.Fields(query), " ")
		for _, required := range []string{
			"function_record.oid = to_regprocedure('public.' || expected.function_name || '()')",
			"function_record.prokind = 'f'",
			"function_record.prorettype = 'pg_catalog.trigger'::regtype",
			"btrim(regexp_replace( function_record.prosrc, '[[:space:]]+', ' ', 'g' )) AS definition",
			"trigger_record.tgtype::int AS trigger_type",
			"trigger_record.tgenabled = 'O' AS enabled",
			"trigger_record.tgqual IS NULL AS unconditional",
			"attribute_record.attrelid = relation.oid AND attribute_record.attnum = ANY(trigger_record.tgattr) ORDER BY attribute_record.attname",
			"WHERE namespace.nspname = 'public' AND relation.relname IN ('competition_candidate_review', 'competition_round') AND NOT trigger_record.tgisinternal",
		} {
			if !strings.Contains(normalized, required) {
				t.Fatalf("staging winner catalog collection lost %q", required)
			}
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("published staging winner contract produced alerts=%+v error=%v", alerts, err)
	}
}

// Each head requires its own published guard; the initial staging policy stays
// retired and the significance-gated winner guard is retired by head 691.
func TestMigrationsSignalStagingWinnerArtifactLifetime(t *testing.T) {
	for _, test := range []struct {
		version int
		missing bool
	}{
		{version: 651},
		{version: 652},
		{version: 683},
		{version: 684},
		{version: 684, missing: true},
		{version: 685},
		{version: 690},
		{version: 691},
		{version: 691, missing: true},
	} {
		row := syntheticMigrationArtifactRow(test.version)
		for _, artifact := range migrationArtifacts {
			if test.version < artifact.requiredVersion ||
				artifact.removedVersion != 0 && artifact.removedVersion <= test.version ||
				artifact.requiredVersion == test.version && test.missing {
				row[artifact.rowColumn] = "f"
			}
		}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(test.version), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		wantAlerts := 0
		if test.version < server.MigrationCount() {
			wantAlerts++
			requireAlertClass(t, alerts, "migration-behind")
		}
		if test.missing {
			wantAlerts++
			alert := requireAlertClass(t, alerts, "migration-schema-drift")
			want := "competition staging automatic winner and review isolation@v684"
			if test.version == 691 {
				want = "competition staging best-safe winner and review isolation@v691"
			}
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("missing staging winner contract was not identified: %s", alert.Markdown())
			}
		}
		if len(alerts) != wantAlerts {
			t.Fatalf("version %d missing=%t: alerts=%+v, want %d", test.version, test.missing, alerts, wantAlerts)
		}
	}
}

// The immutable-round trigger is a second winner gate. The v691 honesty
// trigger alone cannot publish a best-safe staging winner if this one still
// requires takeover eligibility.
func TestMigrationsSignalStagingLifecycleGuardRequiredAt720(t *testing.T) {
	for _, version := range []int{719, 720} {
		row := syntheticMigrationArtifactRow(version)
		row[131] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			if !strings.Contains(query, "competition_round_immutable_guard") ||
				!strings.Contains(query, "NEW.staging OR") {
				t.Fatal("migration query omitted the staging lifecycle guard")
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if version == 719 {
			requireAlertClass(t, alerts, "migration-behind")
			if len(alerts) != 1 {
				t.Fatalf("version 719 alerts=%+v, want only migration-behind", alerts)
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "migration-schema-drift")
		if !strings.Contains(alert.Markdown(), "competition staging lifecycle winner eligibility@v720") {
			t.Fatalf("version 720 alerts=%+v, want lifecycle-guard drift", alerts)
		}
		// A newer append-only migration independently reports migration-behind;
		// it must not hide the missing v720 lifecycle guard.
		wantAlerts := 1
		if server.MigrationCount() > version {
			requireAlertClass(t, alerts, "migration-behind")
			wantAlerts++
		}
		if len(alerts) != wantAlerts {
			t.Fatalf("version 720 alerts=%+v, want %d", alerts, wantAlerts)
		}
	}
}

// Execute the production artifact expression over synthetic catalog rows using
// only a read-only connection, including policy, trigger and function drift.
func TestMigrationsSignalStagingWinnerExecutesAutomaticGuard(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("staging winner query fixtures require the attested local test environment")
	}
	type functionObservation struct {
		Name       string `json:"function_name"`
		FunctionId int    `json:"function_oid"`
		Definition string `json:"definition"`
	}
	type triggerObservation struct {
		Table         string   `json:"table_name"`
		Name          string   `json:"trigger_name"`
		FunctionId    int      `json:"function_oid"`
		Kind          int      `json:"trigger_type"`
		Enabled       bool     `json:"enabled"`
		Unconditional bool     `json:"unconditional"`
		UpdateColumns []string `json:"update_columns"`
	}
	guardNameDefinitions := syntheticStagingGuardSources(t)
	functionObservations := []functionObservation{
		{Name: "competition_staging_candidate_review_guard", FunctionId: 1001, Definition: guardNameDefinitions["review"]},
		{Name: "competition_round_honesty_review_guard", FunctionId: 1002, Definition: guardNameDefinitions["winner"]},
	}
	triggerObservations := []triggerObservation{
		{Table: "competition_candidate_review", Name: "competition_staging_candidate_review_blocked", FunctionId: 1001, Kind: 7, Enabled: true, Unconditional: true, UpdateColumns: []string{}},
		{Table: "competition_round", Name: "competition_round_honesty_reviewed", FunctionId: 1002, Kind: 19, Enabled: true, Unconditional: true, UpdateColumns: []string{"finalized_at", "winner_job_id"}},
	}
	type guardCase struct {
		name    string
		changed string
		replace string
	}
	guardCases := []guardCase{
		{name: "healthy"},
		{name: "canonical whitespace"},
		{name: "historical staging rejection"},
		{name: "no staging placeability", changed: `AND score_json @> '{"placeable":true}'::jsonb`, replace: `AND score_json @> '{}'::jsonb`},
		{name: "production significance bypass", changed: `"statistically_significant":true`, replace: `"statistically_significant":false`},
		{name: "production margin bypass", changed: `"recommended_next_epoch_takeover_margin_supported":true`, replace: `"recommended_next_epoch_takeover_margin_supported":false`},
		{name: "empty gates admitted", changed: "score_json->'gates' <> '{}'::jsonb", replace: "true"},
		{name: "failed gates admitted", changed: "WHERE NOT COALESCE((gate.value->>'passed')::boolean, false)", replace: "WHERE false"},
		{name: "raw score reversed", changed: "(score_json->>'raw_score')::numeric ASC", replace: "(score_json->>'raw_score')::numeric DESC"},
		{name: "legacy ordering reversed", changed: "(score_json->>'normalized_score')::numeric END DESC", replace: "(score_json->>'normalized_score')::numeric END ASC"},
		{name: "shared baseline ignored", changed: "FROM competition_round_baseline WHERE round_id = NEW.round_id", replace: "FROM competition_round_baseline WHERE false"},
		{name: "tie break reversed", changed: "submitted_at, job_id", replace: "job_id, submitted_at"},
		{name: "nullable mismatch bypass", changed: "NEW.winner_job_id IS DISTINCT FROM expected_staging_winner", replace: "NEW.winner_job_id <> expected_staging_winner"},
		{name: "staging early return", changed: "IF NEW.staging = true THEN", replace: "IF NEW.staging = true THEN RETURN NEW;"},
		{name: "production approval bypass", changed: "decision = 'approved'", replace: "decision = 'rejected'"},
		{name: "production unresolved bypass", changed: "IF NEW.winner_job_id IS NULL AND EXISTS (", replace: "IF false AND EXISTS ("},
		{name: "active jobs ignored", changed: "state IN ('queued', 'running')", replace: "state IN ('failed', 'canceled')"},
	}
	for _, name := range []string{"review", "winner"} {
		for _, fault := range []string{"function missing", "function renamed", "function no-op", "trigger missing", "trigger disabled", "trigger conditional", "trigger wrong binding", "trigger wrong event", "trigger wrong table", "trigger wrong columns"} {
			guardCases = append(guardCases, guardCase{name: name + "/" + fault})
		}
	}
	head := server.MigrationCount()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server.Db(ctx, func(conn server.PgConn) {
		for _, test := range guardCases {
			observedFunctions := append([]functionObservation(nil), functionObservations...)
			observedTriggers := append([]triggerObservation(nil), triggerObservations...)
			if test.changed != "" {
				if !strings.Contains(observedFunctions[1].Definition, test.changed) {
					t.Fatalf("%s does not mutate the published function", test.name)
				}
				observedFunctions[1].Definition = strings.Replace(observedFunctions[1].Definition, test.changed, test.replace, 1)
			}
			for index, name := range []string{"review", "winner"} {
				switch test.name {
				case "canonical whitespace":
					observedFunctions[index].Definition = strings.ReplaceAll(observedFunctions[index].Definition, " ", " \n\t")
				case name + "/function missing":
					observedFunctions[index].FunctionId = 0
					observedFunctions[index].Definition = ""
				case name + "/function renamed":
					observedFunctions[index].Name = "synthetic_other_guard"
				case name + "/function no-op":
					observedFunctions[index].Definition = "BEGIN RETURN NEW; END"
				case name + "/trigger missing":
					observedTriggers[index].Name = "synthetic_other_trigger"
				case name + "/trigger disabled":
					observedTriggers[index].Enabled = false
				case name + "/trigger conditional":
					observedTriggers[index].Unconditional = false
				case name + "/trigger wrong binding":
					observedTriggers[index].FunctionId = 1003
				case name + "/trigger wrong event":
					observedTriggers[index].Kind = 5
				case name + "/trigger wrong table":
					observedTriggers[index].Table = "synthetic_other_table"
				case name + "/trigger wrong columns":
					observedTriggers[index].UpdateColumns = []string{"staging"}
				}
			}
			if test.name == "historical staging rejection" {
				observedFunctions[1].Definition = guardNameDefinitions["historical"]
			}
			functionJson, err := json.Marshal(observedFunctions)
			if err != nil {
				t.Fatal(err)
			}
			triggerJson, err := json.Marshal(observedTriggers)
			if err != nil {
				t.Fatal(err)
			}
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(head), nil
				}
				if !strings.Contains(query, competitionStagingBestWinnerArtifactQuery) {
					t.Fatal("migration query omitted the tested staging winner expression")
				}
				var admitted bool
				err := conn.QueryRow(ctx, `
					WITH competition_function_artifact AS (
						SELECT function_name, function_oid,
						       btrim(regexp_replace(definition, '[[:space:]]+', ' ', 'g')) AS definition
						FROM jsonb_to_recordset($1::jsonb) AS actual(
							function_name text, function_oid integer, definition text
						)
					), competition_trigger_artifact AS (
						SELECT * FROM jsonb_to_recordset($2::jsonb) AS actual(
							table_name text, trigger_name text, function_oid integer,
							trigger_type integer, enabled boolean, unconditional boolean,
							update_columns text[]
						)
					)
					SELECT `+competitionStagingBestWinnerArtifactQuery, string(functionJson), string(triggerJson),
				).Scan(&admitted)
				if err != nil {
					return nil, err
				}
				row := syntheticMigrationArtifactRow(head)
				row[102] = fmt.Sprint(admitted)
				return []Row{row}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			if test.name == "healthy" || test.name == "canonical whitespace" {
				if len(alerts) != 0 {
					t.Fatalf("%s produced schema drift: %s", test.name, alerts.ToMarkdown())
				}
			} else {
				alert := requireAlertClass(t, alerts, "migration-schema-drift")
				if len(alerts) != 1 || alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), "competition staging best-safe winner and review isolation@v691") {
					t.Fatalf("%s lost the staging winner gate: %s", test.name, alerts.ToMarkdown())
				}
			}
		}
	}, server.OptReadOnly(), server.OptNoRetry())
}
