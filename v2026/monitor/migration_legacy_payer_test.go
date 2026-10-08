// These controls use emitted catalog expressions and actual migrated schemas.
// Structural faults roll back locally; no payer or financial rows are changed.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Keep the index CTE exactly as emitted, without evaluating unrelated catalog
// artifacts in each fault control or requiring migration_audit to exist yet.
func legacySettlementPayerTestQuery(t testing.TB) string {
	t.Helper()
	var query string
	source := &syntheticSource{postgresFn: func(emitted string) ([]Row, error) {
		if strings.Contains(emitted, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(server.MigrationCount()), nil
		}
		query = emitted
		return []Row{syntheticMigrationArtifactRow(server.MigrationCount())}, nil
	}}
	if _, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	if len(legacySettlementPayerArtifactQueries) != 3 ||
		!strings.Contains(query, strings.Join(legacySettlementPayerArtifactQueries, ",\n")) {
		t.Fatal("the real migration signal omitted the payer scheduling contracts")
	}
	_, indexQuery, found := strings.Cut(query, "index_artifact AS (")
	if !found {
		t.Fatal("the migration signal omitted its index catalog")
	}
	indexQuery, _, found = strings.Cut(indexQuery, "), constraint_artifact AS (")
	if !found {
		t.Fatal("the migration signal changed its index catalog boundary")
	}
	return "WITH index_artifact AS (" + indexQuery + ") SELECT ARRAY[" +
		strings.Join(legacySettlementPayerArtifactQueries, ",") + "]"
}

// Future absence is pending migration; the same absent artifact at its
// published version must reach its exact schema-drift attribution.
func TestMigrationsLegacyPayerArtifactsFollowPublishedVersions(t *testing.T) {
	legacySettlementPayerTestQuery(t)
	for version := 791; version <= 793; version++ {
		var artifact *migrationArtifact
		for i := range migrationArtifacts {
			if migrationArtifacts[i].requiredVersion == version {
				if artifact != nil {
					t.Fatal("payer scheduling migration has duplicate artifact contracts")
				}
				artifact = &migrationArtifacts[i]
			}
		}
		if artifact == nil || artifact.rowColumn != version-589 {
			t.Fatal("published payer scheduling migration has no exact artifact row contract")
		}
		for _, head := range []int{version - 1, version} {
			row := syntheticMigrationArtifactRow(head)
			row[artifact.rowColumn] = "f"
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(head), nil
				}
				return []Row{row}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			if head < version {
				if len(alerts) != 1 || alerts[0].Class != "migration-behind" {
					t.Fatal("unpublished payer scheduling artifact was reported as schema drift")
				}
			} else if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), fmt.Sprintf("%s@v%d", artifact.name, version)) {
				t.Fatal("payer scheduling schema drift lost its owning migration")
			}
		}
	}
}

// Querying missing future tables or functions must remain safe. Each appended
// migration, including the restartable index appends, admits only its prefix.
func TestMigrationsLegacyPayerExactSchemaPrefixes(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := legacySettlementPayerTestQuery(t)
		check := func(head int) {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var matches []bool
				server.Raise(conn.QueryRow(ctx, query).Scan(&matches))
				if len(matches) != 3 {
					t.Fatal("payer scheduling catalog returned an incomplete artifact row")
				}
				for i, matched := range matches {
					if matched != (head >= 791+i) {
						t.Fatalf("payer scheduling artifact%d does not follow prefix%d", 791+i, head)
					}
				}
			})
		}
		check(0)
		for head := 790; head <= 793; head++ {
			server.ApplyDbMigrationsUpTo(ctx, head)
			check(head)
		}
	})
}

// Faults execute the same emitted predicates on pg_catalog. Signal reduction
// receives those real booleans; unrelated artifacts keep their healthy values.
func TestMigrationsLegacyPayerCatalogFaultsReachSignal(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		query := legacySettlementPayerTestQuery(t)
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			read := func() []bool {
				t.Helper()
				var matches []bool
				server.Raise(tx.QueryRow(ctx, query).Scan(&matches))
				if len(matches) != 3 {
					t.Fatal("payer scheduling fault returned an incomplete catalog row")
				}
				return matches
			}
			healthy := func() {
				t.Helper()
				for _, matched := range read() {
					if !matched {
						t.Fatal("healthy payer scheduling schema failed its catalog contract")
					}
				}
			}
			healthy()
			for _, fault := range []struct {
				name, sql string
				version   int
			}{
				{name: "missing payer", version: 791, sql: `ALTER TABLE legacy_settlement_intent RENAME COLUMN payer_network_id TO synthetic_payer`},
				{name: "required payer", version: 791, sql: `ALTER TABLE legacy_settlement_intent ALTER COLUMN payer_network_id SET NOT NULL`},
				{name: "wrong payer type", version: 791, sql: `ALTER TABLE legacy_settlement_intent ALTER COLUMN payer_network_id TYPE text USING payer_network_id::text`},
				{name: "defaulted payer", version: 791, sql: `ALTER TABLE legacy_settlement_intent ALTER COLUMN payer_network_id SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`},
				{name: "generated payer", version: 791, sql: `ALTER TABLE legacy_settlement_intent DROP COLUMN payer_network_id; ALTER TABLE legacy_settlement_intent ADD COLUMN payer_network_id uuid GENERATED ALWAYS AS (contract_id) STORED`},
				{name: "missing rolling writer function", version: 791, sql: `DROP FUNCTION assign_legacy_settlement_intent_payer() CASCADE`},
				{name: "missing rolling writer trigger", version: 791, sql: `DROP TRIGGER legacy_settlement_intent_assign_payer ON legacy_settlement_intent`},
				{name: "disabled rolling writer", version: 791, sql: `ALTER TABLE legacy_settlement_intent DISABLE TRIGGER legacy_settlement_intent_assign_payer`},
				{name: "replica-only rolling writer", version: 791, sql: `ALTER TABLE legacy_settlement_intent ENABLE REPLICA TRIGGER legacy_settlement_intent_assign_payer`},
				{name: "late rolling writer", version: 791, sql: `DROP TRIGGER legacy_settlement_intent_assign_payer ON legacy_settlement_intent; CREATE TRIGGER legacy_settlement_intent_assign_payer AFTER INSERT ON legacy_settlement_intent FOR EACH ROW EXECUTE FUNCTION assign_legacy_settlement_intent_payer()`},
				{name: "wrong rolling writer event", version: 791, sql: `DROP TRIGGER legacy_settlement_intent_assign_payer ON legacy_settlement_intent; CREATE TRIGGER legacy_settlement_intent_assign_payer BEFORE UPDATE ON legacy_settlement_intent FOR EACH ROW EXECUTE FUNCTION assign_legacy_settlement_intent_payer()`},
				{name: "conditional rolling writer", version: 791, sql: `DROP TRIGGER legacy_settlement_intent_assign_payer ON legacy_settlement_intent; CREATE TRIGGER legacy_settlement_intent_assign_payer BEFORE INSERT ON legacy_settlement_intent FOR EACH ROW WHEN (NEW.shard = 0) EXECUTE FUNCTION assign_legacy_settlement_intent_payer()`},
				{name: "no-op rolling writer", version: 791, sql: `CREATE OR REPLACE FUNCTION assign_legacy_settlement_intent_payer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END; $$`},
				{name: "definer rolling writer", version: 791, sql: `ALTER FUNCTION assign_legacy_settlement_intent_payer() SECURITY DEFINER`},
				{name: "configured rolling writer", version: 791, sql: `ALTER FUNCTION assign_legacy_settlement_intent_payer() SET search_path TO public`},
				{name: "missing payer due index", version: 792, sql: `DROP INDEX legacy_settlement_intent_payer_due`},
				{name: "payer due key order", version: 792, sql: `DROP INDEX legacy_settlement_intent_payer_due; CREATE INDEX legacy_settlement_intent_payer_due ON legacy_settlement_intent (shard,next_attempt_time,payer_network_id,contract_id) WHERE payer_network_id IS NOT NULL`},
				{name: "unfiltered payer due index", version: 792, sql: `DROP INDEX legacy_settlement_intent_payer_due; CREATE INDEX legacy_settlement_intent_payer_due ON legacy_settlement_intent (shard,payer_network_id,next_attempt_time,contract_id)`},
				{name: "wrong payer due predicate", version: 792, sql: `DROP INDEX legacy_settlement_intent_payer_due; CREATE INDEX legacy_settlement_intent_payer_due ON legacy_settlement_intent (shard,payer_network_id,next_attempt_time,contract_id) WHERE payer_network_id IS NULL`},
				{name: "unique payer due index", version: 792, sql: `DROP INDEX legacy_settlement_intent_payer_due; CREATE UNIQUE INDEX legacy_settlement_intent_payer_due ON legacy_settlement_intent (shard,payer_network_id,next_attempt_time,contract_id) WHERE payer_network_id IS NOT NULL`},
				{name: "included payer due column", version: 792, sql: `DROP INDEX legacy_settlement_intent_payer_due; CREATE INDEX legacy_settlement_intent_payer_due ON legacy_settlement_intent (shard,payer_network_id,next_attempt_time,contract_id) INCLUDE(outcome) WHERE payer_network_id IS NOT NULL`},
				{name: "missing registration index", version: 793, sql: `DROP INDEX legacy_settlement_intent_payer_missing`},
				{name: "registration key order", version: 793, sql: `DROP INDEX legacy_settlement_intent_payer_missing; CREATE INDEX legacy_settlement_intent_payer_missing ON legacy_settlement_intent (contract_id,shard) WHERE payer_network_id IS NULL`},
				{name: "unfiltered registration index", version: 793, sql: `DROP INDEX legacy_settlement_intent_payer_missing; CREATE INDEX legacy_settlement_intent_payer_missing ON legacy_settlement_intent (shard,contract_id)`},
				{name: "wrong registration predicate", version: 793, sql: `DROP INDEX legacy_settlement_intent_payer_missing; CREATE INDEX legacy_settlement_intent_payer_missing ON legacy_settlement_intent (shard,contract_id) WHERE payer_network_id IS NOT NULL`},
				{name: "wrong registration method", version: 793, sql: `DROP INDEX legacy_settlement_intent_payer_missing; CREATE INDEX legacy_settlement_intent_payer_missing ON legacy_settlement_intent USING brin(shard,contract_id) WHERE payer_network_id IS NULL`},
			} {
				server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT payer_catalog_fault`))
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				matches := read()
				if matches[fault.version-791] {
					t.Fatalf("%s cleared its published catalog contract", fault.name)
				}
				row := syntheticMigrationArtifactRow(server.MigrationCount())
				for i, matched := range matches {
					row[202+i] = fmt.Sprint(matched)
				}
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					if strings.Contains(query, "FROM migration_catalog") {
						return syntheticMigrationCatalogRows(server.MigrationCount()), nil
					}
					return []Row{row}, nil
				}}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, alert := range alerts {
					if alert.Class == "migration-schema-drift" && alert.Severity == SeverityPage &&
						strings.Contains(alert.Markdown(), fmt.Sprintf("@v%d", fault.version)) {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s did not reach its owning schema alert", fault.name)
				}
				server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT payer_catalog_fault`))
				healthy()
			}
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE legacy_settlement_intent ENABLE ALWAYS TRIGGER legacy_settlement_intent_assign_payer`))
			healthy() // Always-enabled still covers ordinary rolling writers.
		})
	})
}

// An interrupted concurrent build must fail independently for invalid or
// not-ready state, without requiring privileged writes to PostgreSQL catalogs.
func TestMigrationsLegacyPayerIndexReadiness(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			for _, index := range []struct{ name, keys, predicate string }{
				{"legacy_settlement_intent_payer_due", "shard, payer_network_id, next_attempt_time, contract_id", "(payer_network_id IS NOT NULL)"},
				{"legacy_settlement_intent_payer_missing", "shard, contract_id", "(payer_network_id IS NULL)"},
			} {
				guard := migrationFp2Index("legacy_settlement_intent", index.name, index.keys, index.predicate, false)
				definition := "CREATE INDEX " + index.name + " ON public.legacy_settlement_intent USING btree (" + index.keys + ") WHERE " + index.predicate
				for _, state := range []struct{ valid, ready, want bool }{
					{valid: true, ready: true, want: true}, {valid: false, ready: true}, {valid: true, ready: false},
				} {
					var matched bool
					server.Raise(conn.QueryRow(ctx, `WITH index_artifact AS (
 SELECT 'legacy_settlement_intent'::text AS table_name, $1::text AS index_name,
 $2::text AS definition, $3::text AS predicate_definition,
 $4::boolean AS indisvalid, $5::boolean AS indisready) SELECT `+guard,
						index.name, definition, index.predicate, state.valid, state.ready).Scan(&matched))
					if matched != state.want {
						t.Fatal("payer scheduling index readiness failed its catalog contract")
					}
				}
			}
		})
	})
}
