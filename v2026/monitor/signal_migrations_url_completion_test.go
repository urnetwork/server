// Real catalog faults must reach the same migration alert as the live probe.
package monitor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func migrationUrlCompletionTestQuery(t testing.TB) string {
	t.Helper()
	query := ""
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
	prefix, _, ok := strings.Cut(query, "SELECT version.value,")
	if !ok || !strings.Contains(query, strings.Join(migrationUrlCompletionArtifactQueries, ",\n")) {
		t.Fatal("the migration signal did not emit the completion contracts")
	}
	return prefix + "SELECT ARRAY[" + strings.Join(migrationUrlCompletionArtifactQueries, ",") + "]"
}

// Each published prefix is healthy without its future artifacts. Once a
// version is recorded, missing or incomplete evidence cannot clear its gate.
func TestMigrationUrlCompletionArtifactLifetimes(t *testing.T) {
	if len(migrationUrlCompletionArtifactQueries) != 8 {
		t.Fatal("versions 732 through 739 need eight catalog contracts")
	}
	for version := 731; version <= 739; version++ {
		row := syntheticMigrationArtifactRow(version)
		for _, artifact := range migrationArtifacts {
			if version < artifact.requiredVersion {
				row[artifact.rowColumn] = "f"
			}
		}
		for _, missing := range []int{0, version} {
			if missing == 731 {
				continue
			}
			observed := append(Row{}, row...)
			if missing > 0 {
				observed[missing-589] = "f"
			}
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(version), nil
				}
				return []Row{observed}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			drift := false
			for _, alert := range alerts {
				if alert.Class == "migration-schema-drift" {
					drift = true
					if missing == 0 || alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), fmt.Sprintf("@v%d", missing)) {
						t.Fatalf("version %d has the wrong schema finding: %s", version, alert.Markdown())
					}
				} else if alert.Class != "migration-behind" {
					t.Fatalf("unexpected migration finding: %s", alert.Class)
				}
			}
			if drift != (missing > 0) {
				t.Fatalf("version %d missing %d drift=%t", version, missing, drift)
			}
		}
	}
}

func TestMigrationUrlCompletionPartialCatalogCannotPass(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		// The old detector's row ends before every newly required artifact.
		return []Row{syntheticMigrationArtifactRow(head)[:143]}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "migration-schema-drift")
	for version := 732; version <= 739; version++ {
		if !strings.Contains(alert.Markdown(), fmt.Sprintf("@v%d", version)) {
			t.Fatalf("partial catalog silently cleared v%d", version)
		}
	}
}

func TestMigrationUrlCompletionCatalogGuardsOnMigratedDatabase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := migrationUrlCompletionTestQuery(t)
		server.Tx(ctx, func(tx server.PgTx) {
			read := func() []bool {
				var matches []bool
				server.Raise(tx.QueryRow(ctx, query).Scan(&matches))
				if len(matches) != 8 {
					t.Fatalf("catalog returned %d completion contracts", len(matches))
				}
				return matches
			}
			assertHealthy := func() {
				for index, matched := range read() {
					if !matched {
						t.Fatalf("migrated schema failed its v%d guard", index+732)
					}
				}
			}
			assertHealthy()
			server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT completion_catalog_additive;
				ALTER TABLE provider_url_probe_run ADD COLUMN synthetic_future_detail text`))
			assertHealthy()
			server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT completion_catalog_additive`))
			faults := []struct {
				name, sql string
				version   int
			}{
				{name: "missing receipt relation", version: 732, sql: `DROP TABLE provider_url_probe_run`},
				{name: "unlogged receipts", version: 732, sql: `ALTER TABLE provider_url_probe_run SET UNLOGGED`},
				{name: "wrong claim default", version: 732, sql: `ALTER TABLE provider_egress_probe_cycle ALTER COLUMN claim_ordinal SET DEFAULT 1`},
				{name: "nullable rolling count", version: 732, sql: `ALTER TABLE provider_egress_probe_cycle ALTER COLUMN completed_run_count DROP NOT NULL`},
				{name: "missing nonnegative count", version: 732, sql: `ALTER TABLE provider_egress_probe_cycle DROP CONSTRAINT provider_egress_probe_cycle_completed_run_count_check`},
				{name: "fabricated expiry", version: 732, sql: `ALTER TABLE provider_egress_probe_cycle ALTER COLUMN completed_next_expiry_at SET DEFAULT now()`},
				{name: "ready by default", version: 732, sql: `ALTER TABLE provider_egress_probe_cycle ALTER COLUMN completed_priority_ready SET DEFAULT true`},
				{name: "missing receipt identity", version: 732, sql: `ALTER TABLE provider_url_probe_run DROP CONSTRAINT provider_url_probe_run_pkey`},
				{name: "wrong receipt clock", version: 732, sql: `ALTER TABLE provider_url_probe_run ALTER COLUMN completed_at TYPE timestamptz`},
				{name: "unbounded failure", version: 732, sql: `ALTER TABLE provider_url_probe_run ALTER COLUMN probe_failure TYPE text`},
				{name: "counted by default", version: 732, sql: `ALTER TABLE provider_url_probe_run ALTER COLUMN counted SET DEFAULT true`},
				{name: "unvalidated claim check", version: 732, sql: `ALTER TABLE provider_url_probe_run DROP CONSTRAINT provider_url_probe_run_claim_ordinal_check; ALTER TABLE provider_url_probe_run ADD CONSTRAINT provider_url_probe_run_claim_ordinal_check CHECK(claim_ordinal>0) NOT VALID`},
				{name: "missing completion bounds", version: 732, sql: `DO $$ DECLARE item record; BEGIN
					FOR item IN SELECT conname FROM pg_constraint WHERE conrelid='provider_url_probe_run'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%claimed_at%' LOOP
					EXECUTE format('ALTER TABLE provider_url_probe_run DROP CONSTRAINT %I',item.conname); END LOOP; END $$`},
				{name: "missing active receipt index", version: 732, sql: `DROP INDEX provider_url_probe_run_active`},
				{name: "unfiltered receipt retention", version: 732, sql: `DROP INDEX provider_url_probe_run_retention; CREATE INDEX provider_url_probe_run_retention ON provider_url_probe_run(claimed_at,client_id,claim_ordinal)`},
				{name: "missing invalidation trigger", version: 732, sql: `DROP TRIGGER provider_url_probe_ready_invalidate ON provider_egress_probe_cycle`},
				{name: "disabled invalidation", version: 732, sql: `ALTER TABLE provider_egress_probe_cycle DISABLE TRIGGER provider_url_probe_ready_invalidate`},
				{name: "replica-only invalidation", version: 732, sql: `ALTER TABLE provider_egress_probe_cycle ENABLE REPLICA TRIGGER provider_url_probe_ready_invalidate`},
				{name: "incomplete update columns", version: 732, sql: `DROP TRIGGER provider_url_probe_ready_invalidate ON provider_egress_probe_cycle; CREATE TRIGGER provider_url_probe_ready_invalidate BEFORE UPDATE OF next_attempt_at ON provider_egress_probe_cycle FOR EACH ROW EXECUTE FUNCTION provider_url_probe_ready_invalidate()`},
				{name: "late invalidation", version: 732, sql: `DROP TRIGGER provider_url_probe_ready_invalidate ON provider_egress_probe_cycle; CREATE TRIGGER provider_url_probe_ready_invalidate AFTER UPDATE OF next_attempt_at,eligible ON provider_egress_probe_cycle FOR EACH ROW EXECUTE FUNCTION provider_url_probe_ready_invalidate()`},
				{name: "conditional invalidation", version: 732, sql: `DROP TRIGGER provider_url_probe_ready_invalidate ON provider_egress_probe_cycle; CREATE TRIGGER provider_url_probe_ready_invalidate BEFORE UPDATE OF next_attempt_at,eligible ON provider_egress_probe_cycle FOR EACH ROW WHEN (false) EXECUTE FUNCTION provider_url_probe_ready_invalidate()`},
				{name: "wrong function binding", version: 732, sql: `CREATE FUNCTION synthetic_invalidate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$; DROP TRIGGER provider_url_probe_ready_invalidate ON provider_egress_probe_cycle; CREATE TRIGGER provider_url_probe_ready_invalidate BEFORE UPDATE OF next_attempt_at,eligible ON provider_egress_probe_cycle FOR EACH ROW EXECUTE FUNCTION synthetic_invalidate()`},
				{name: "no-op containing correct branch", version: 732, sql: `CREATE OR REPLACE FUNCTION provider_url_probe_ready_invalidate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN /* IF NEW.next_attempt_at IS DISTINCT FROM OLD.next_attempt_at OR NOT NEW.eligible THEN NEW.completed_priority_ready := false; END IF; */ RETURN NEW; END $$`},
				{name: "changed function settings", version: 732, sql: `ALTER FUNCTION provider_url_probe_ready_invalidate() SET search_path=public`},
			}
			for _, index := range syntheticMigrationPartialIndexContracts() {
				if 733 <= index.version && index.version <= 739 {
					faults = append(faults, struct {
						name, sql string
						version   int
					}{name: "missing " + index.name, sql: "DROP INDEX " + index.name, version: index.version})
				}
			}
			for _, fault := range faults {
				server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT completion_catalog_fault`))
				server.RaisePgResult(tx.Exec(ctx, fault.sql))
				matches := read()
				if matches[fault.version-732] {
					t.Fatalf("%s passed its v%d guard", fault.name, fault.version)
				}
				row := syntheticMigrationArtifactRow(server.MigrationCount())
				for index, matched := range matches {
					row[index+143] = fmt.Sprint(matched)
				}
				source := &syntheticSource{postgresFn: func(emitted string) ([]Row, error) {
					if strings.Contains(emitted, "FROM migration_catalog") {
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
					found = found || alert.Class == "migration-schema-drift" && alert.Severity == SeverityPage &&
						strings.Contains(alert.Markdown(), fmt.Sprintf("@v%d", fault.version))
				}
				if !found {
					t.Fatalf("%s did not reach the owning migration alert", fault.name)
				}
				server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT completion_catalog_fault`))
				assertHealthy()
			}
			// A pre-rollout catalog is queried without relation/function casts
			// that could abort the entire signal merely because it is not ready.
			server.RaisePgResult(tx.Exec(ctx, `DROP TABLE provider_url_probe_run;
				DROP FUNCTION provider_url_probe_ready_invalidate() CASCADE;
				ALTER TABLE provider_egress_probe_cycle DROP COLUMN claim_ordinal,
					DROP COLUMN completed_run_count CASCADE,DROP COLUMN completed_next_expiry_at CASCADE,
					DROP COLUMN completed_priority_ready CASCADE;
				DROP INDEX transfer_balance_active_network_end_start_id`))
			for index, matched := range read() {
				if matched {
					t.Fatalf("absent future artifact passed v%d guard", index+732)
				}
			}
		})
	})
}
