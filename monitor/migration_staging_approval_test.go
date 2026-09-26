// Published staging approval metadata is independent of the monitor's expected
// contract; mutated snapshots exercise the same emitted SQL without schema writes.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Version 720 must tolerate the future table; version 721 must name its drift.
func TestMigrationsSignalStagingApprovalVersionGate(t *testing.T) {
	for _, version := range []int{720, 721} {
		row := syntheticMigrationArtifactRow(version)
		row[132] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			if !strings.Contains(query, competitionStagingApprovalCatalogQuery) ||
				!strings.Contains(query, competitionStagingApprovalArtifactQuery) {
				t.Fatal("migration query omitted staging approval evidence or contract")
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		wantCount := 0
		if version < server.MigrationCount() {
			requireAlertClass(t, alerts, "migration-behind")
			wantCount++
		}
		if version == 721 {
			alert := requireAlertClass(t, alerts, "migration-schema-drift")
			if alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(),
				"competition staging winner approval and append-only guards@v721") {
				t.Fatal("missing staging approval did not page with its published version")
			}
			wantCount++
		}
		if len(alerts) != wantCount {
			t.Fatalf("version %d: alerts=%d, want %d", version, len(alerts), wantCount)
		}
	}
}

// Fresh migrations prove the actual collection and canonical constraint
// rendering; every synthetic drift then executes the production expression.
func TestMigrationsSignalStagingApprovalExecutesExactGuard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		source := &migrationPingDatabaseSource{}
		if _, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source)); err != nil {
			t.Fatal(err)
		}
		if len(source.artifactRow) < 133 || !migrationBool(source.artifactRow[132]) {
			t.Fatal("published staging approval does not satisfy the collected catalog contract")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var publishedJson []byte
			server.Raise(conn.QueryRow(ctx, `
				WITH `+competitionStagingApprovalCatalogQuery+`
				SELECT jsonb_build_object(
					'relations', (SELECT jsonb_agg(actual) FROM staging_approval_relation_artifact AS actual),
					'columns', (SELECT jsonb_agg(actual) FROM staging_approval_column_artifact AS actual),
					'constraints', (
						SELECT jsonb_agg(jsonb_build_object(
							'table_name', 'competition_staging_winner_approval',
							'constraint_type', contype::text,
							'definition', regexp_replace(pg_get_constraintdef(oid), '[[:space:]]+', ' ', 'g'),
							'validated', convalidated
						)) FROM pg_constraint
						WHERE conrelid = to_regclass('public.competition_staging_winner_approval')
						  AND contype IN ('p', 'f', 'c')
					),
					'functions', (
						SELECT jsonb_agg(jsonb_build_object(
							'function_name', proname, 'function_oid', oid, 'definition', prosrc
						)) FROM pg_proc WHERE oid IN (
							to_regprocedure('public.competition_append_only_guard()'),
							to_regprocedure('public.competition_staging_winner_approval_guard()')
						)
					),
					'triggers', (
						SELECT jsonb_agg(jsonb_build_object(
							'table_name', 'competition_staging_winner_approval',
							'trigger_name', tgname, 'function_oid', tgfoid, 'trigger_type', tgtype::int,
							'enabled', tgenabled = 'O', 'unconditional', tgqual IS NULL,
							'update_columns', ARRAY(
								SELECT attname::text FROM pg_attribute
								WHERE attrelid = tgrelid AND attnum = ANY(tgattr) ORDER BY attname
							)
						)) FROM pg_trigger
						WHERE tgrelid = to_regclass('public.competition_staging_winner_approval')
						  AND NOT tgisinternal
					)
				)
			`).Scan(&publishedJson))
			var published map[string][]map[string]any
			server.Raise(json.Unmarshal(publishedJson, &published))
			for section, wantCount := range map[string]int{
				"relations": 1, "columns": 7, "constraints": 7, "functions": 2, "triggers": 2,
			} {
				if len(published[section]) != wantCount {
					t.Fatalf("published %s count=%d, want %d", section, len(published[section]), wantCount)
				}
			}
			type approvalCase struct {
				name    string
				section string
				index   int
				field   string
				value   any
				remove  bool
			}
			cases := []approvalCase{
				{name: "healthy"},
				{name: "canonical whitespace"},
				{name: "missing table", section: "relations", remove: true},
				{name: "wrong relation kind", section: "relations", field: "table_kind", value: "v"},
			}
			for section, fields := range map[string]map[string]any{
				"columns":     {"column_name": "synthetic_other_column", "data_type": "synthetic_type", "is_nullable": "YES", "column_default": "NULL", "character_maximum_length": 4096},
				"constraints": {"table_name": "synthetic_other_table", "constraint_type": "x", "definition": "CHECK (true)", "validated": false},
				"functions":   {"function_name": "synthetic_other_guard", "definition": "BEGIN RETURN NEW; END"},
				"triggers":    {"table_name": "synthetic_other_table", "trigger_name": "synthetic_other_trigger", "function_oid": 0, "trigger_type": 5, "enabled": false, "unconditional": false, "update_columns": []string{"job_id"}},
			} {
				for index := range published[section] {
					cases = append(cases, approvalCase{
						name: fmt.Sprintf("%s/%d/missing", section, index), section: section, index: index, remove: true,
					})
					for field, value := range fields {
						cases = append(cases, approvalCase{
							name: fmt.Sprintf("%s/%d/%s", section, index, field), section: section, index: index, field: field, value: value,
						})
					}
				}
			}
			for index, function := range published["functions"] {
				if function["function_name"] != "competition_staging_winner_approval_guard" {
					continue
				}
				for _, predicate := range []string{
					"round.round_id = NEW.round_id", "round.staging = true", "round.canceled = false",
					"round.finalized_at IS NOT NULL", "round.winner_job_id = NEW.job_id",
					"job.round_id = NEW.round_id", "job.state = 'succeeded'", "job.job_id = NEW.job_id",
				} {
					definition := function["definition"].(string)
					if !strings.Contains(definition, predicate) {
						t.Fatalf("published approval guard lacks %s", predicate)
					}
					cases = append(cases, approvalCase{
						name: "weakened " + predicate, section: "functions", index: index,
						field: "definition", value: strings.Replace(definition, predicate, "true", 1),
					})
				}
			}
			for _, test := range cases {
				var observed map[string][]map[string]any
				server.Raise(json.Unmarshal(publishedJson, &observed))
				if test.remove {
					rows := observed[test.section]
					observed[test.section] = append(rows[:test.index], rows[test.index+1:]...)
				} else if test.section != "" {
					observed[test.section][test.index][test.field] = test.value
				} else if test.name == "canonical whitespace" {
					for _, function := range observed["functions"] {
						function["definition"] = strings.ReplaceAll(function["definition"].(string), "\t", " \n\t")
					}
				}
				observedJson, err := json.Marshal(observed)
				server.Raise(err)
				var admitted bool
				server.Raise(conn.QueryRow(ctx, `
					WITH staging_approval_relation_artifact AS (
						SELECT * FROM jsonb_to_recordset($1::jsonb->'relations') AS actual(table_kind text)
					), staging_approval_column_artifact AS (
						SELECT * FROM jsonb_to_recordset($1::jsonb->'columns') AS actual(
							column_name text, data_type text, is_nullable text,
							column_default text, character_maximum_length integer
						)
					), constraint_artifact AS (
						SELECT * FROM jsonb_to_recordset($1::jsonb->'constraints') AS actual(
							table_name text, constraint_type text, definition text, validated boolean
						)
					), competition_function_artifact AS (
						SELECT function_name, function_oid,
							btrim(regexp_replace(definition, '[[:space:]]+', ' ', 'g')) AS definition
						FROM jsonb_to_recordset($1::jsonb->'functions') AS actual(
							function_name text, function_oid integer, definition text
						)
					), competition_trigger_artifact AS (
						SELECT * FROM jsonb_to_recordset($1::jsonb->'triggers') AS actual(
							table_name text, trigger_name text, function_oid integer,
							trigger_type integer, enabled boolean, unconditional boolean,
							update_columns text[]
						)
					)
					SELECT `+competitionStagingApprovalArtifactQuery, string(observedJson),
				).Scan(&admitted))
				want := test.name == "healthy" || test.name == "canonical whitespace"
				if admitted != want {
					t.Errorf("%s admitted=%t, want %t", test.name, admitted, want)
				}
			}
		}, server.OptReadOnly(), server.OptNoRetry())
	})
}
