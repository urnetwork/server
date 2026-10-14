// The URL/ARIN migration monitor proves live catalog shape and version lifetime.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Every intermediate prefix is coherent; v729 retires only v726's old index
// contract, while its replacement keeps the marker and ordinal columns checked.
func TestMigrationFp2ArtifactLifetimes(t *testing.T) {
	if len(migrationFp2ArtifactQueries) != 9 {
		t.Fatal("the nine appended versions do not have nine query contracts")
	}
	for version := 721; version <= 730; version++ {
		row := syntheticMigrationArtifactRow(version)
		for _, artifact := range migrationArtifacts {
			if artifact.requiredVersion >= 722 && (version < artifact.requiredVersion ||
				(artifact.removedVersion > 0 && artifact.removedVersion <= version)) {
				row[artifact.rowColumn] = "f"
			}
			if artifact.requiredVersion == 726 && artifact.removedVersion != 729 {
				t.Fatal("v726's replaced partial-index lifetime is not exact")
			}
		}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		for _, alert := range alerts {
			if alert.Class != "migration-behind" {
				t.Fatalf("coherent prefix%d pages: %s", version, alert.Markdown())
			}
		}
	}
}

// Reuse the production catalog CTEs and exact nine emitted expressions. The
// isolated database's real pg_catalog, not fabricated true rows, judges shape.
func migrationFp2TestQuery(t testing.TB) string {
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
	if !ok || !strings.Contains(query, strings.Join(migrationFp2ArtifactQueries, ",\n")) {
		t.Fatal("the real migration signal did not emit the FP2 contracts")
	}
	return prefix + "SELECT ARRAY[" + strings.Join(migrationFp2ArtifactQueries, ",") + "]"
}

// Missing objects, look-alike defaults/keys/predicates, wrong generation and
// invalid checks must page; rollback restores each fault before the next one.
func TestMigrationFp2CatalogGuardsOnMigratedDatabase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := migrationFp2TestQuery(t)
		server.Tx(ctx, func(tx server.PgTx) {
			read := func() []bool {
				var matches []bool
				server.Raise(tx.QueryRow(ctx, query).Scan(&matches))
				if len(matches) != 9 {
					t.Fatalf("catalog returned%d contracts", len(matches))
				}
				return matches
			}
			for index, matches := range read() {
				if matches != (index != 4) {
					t.Fatalf("migrated schema v%d match=%t, want%t", index+722, matches, index != 4)
				}
			}
			for _, fault := range []struct {
				name, sql string
				version   int
			}{
				{name: "missing connection ARIN flag", version: 722, sql: `ALTER TABLE network_client_location DROP COLUMN arin_risk`},
				{name: "wrong rollup ARIN default", version: 722, sql: `ALTER TABLE network_client_location_reliability ALTER COLUMN arin_non_quality SET DEFAULT true`},
				{name: "nullable ARIN fact", version: 722, sql: `ALTER TABLE network_client_location ALTER COLUMN arin_non_quality DROP NOT NULL`},
				{name: "missing history", version: 723, sql: `DROP TABLE provider_egress_health_history`},
				{name: "unlogged history", version: 723, sql: `ALTER TABLE provider_egress_health_history SET UNLOGGED`},
				{name: "missing receipt identity", version: 723, sql: `ALTER TABLE provider_egress_health_history DROP CONSTRAINT provider_egress_health_history_pkey`},
				{name: "wrong count type", version: 723, sql: `ALTER TABLE provider_egress_health_history ALTER COLUMN ok_count TYPE bigint`},
				{name: "unvalidated count check", version: 723, sql: `ALTER TABLE provider_egress_health_history DROP CONSTRAINT provider_egress_health_history_ok_count_check; ALTER TABLE provider_egress_health_history ADD CONSTRAINT provider_egress_health_history_ok_count_check CHECK(ok_count>=0) NOT VALID`},
				{name: "wrong count check", version: 723, sql: `DO $$ DECLARE check_name text; BEGIN
					SELECT conname INTO STRICT check_name FROM pg_constraint WHERE conrelid='provider_egress_health_history'::regclass AND pg_get_constraintdef(oid)='CHECK ((total_count >= ok_count))';
					EXECUTE format('ALTER TABLE provider_egress_health_history DROP CONSTRAINT %I',check_name);
					END $$; ALTER TABLE provider_egress_health_history ADD CONSTRAINT synthetic_wrong_total CHECK(total_count>=0)`},
				{name: "wrong history order", version: 723, sql: `DROP INDEX provider_egress_health_history_client_time; CREATE INDEX provider_egress_health_history_client_time ON provider_egress_health_history(measured_at,client_id)`},
				{name: "missing queue deadline", version: 723, sql: `ALTER TABLE provider_egress_probe_cycle DROP COLUMN next_attempt_at CASCADE`},
				{name: "fabricated security timestamp", version: 724, sql: `ALTER TABLE provider_egress_health ALTER COLUMN security_measured_at SET DEFAULT now()`},
				{name: "eligible by default", version: 725, sql: `ALTER TABLE provider_egress_probe_cycle ALTER COLUMN eligible SET DEFAULT true`},
				{name: "unfiltered eligible index", version: 725, sql: `DROP INDEX provider_egress_probe_cycle_eligible_next_attempt; CREATE INDEX provider_egress_probe_cycle_eligible_next_attempt ON provider_egress_probe_cycle(next_attempt_at,client_id)`},
				{name: "missing URL security", version: 727, sql: `DROP TABLE provider_egress_url_security`},
				{name: "provider-only security key", version: 727, sql: `ALTER TABLE provider_egress_url_security DROP CONSTRAINT provider_egress_url_security_pkey; ALTER TABLE provider_egress_url_security ADD PRIMARY KEY(client_id)`},
				{name: "unbounded security key", version: 727, sql: `ALTER TABLE provider_egress_url_security ALTER COLUMN url_key TYPE text`},
				{name: "missing TLS predicate", version: 727, sql: `DROP INDEX provider_egress_url_security_unresolved; CREATE INDEX provider_egress_url_security_unresolved ON provider_egress_url_security(client_id,url_key)`},
				{name: "missing retained quarantine", version: 727, sql: `ALTER TABLE provider_egress_health DROP COLUMN legacy_tls_authentication_failure`},
				{name: "wrong evidence type", version: 727, sql: `ALTER TABLE provider_egress_health_history ALTER COLUMN url_probe_evidence TYPE json`},
				{name: "ordinary slot lookalike", version: 728, sql: `ALTER TABLE provider_egress_probe_cycle ALTER COLUMN slot_id DROP EXPRESSION`},
				{name: "wrong slot generation", version: 728, sql: `ALTER TABLE provider_egress_probe_cycle DROP COLUMN slot_id CASCADE; ALTER TABLE provider_egress_probe_cycle ADD COLUMN slot_id smallint GENERATED ALWAYS AS (((hashtext(client_id::text)%256)+256)%256) STORED; CREATE INDEX provider_egress_probe_cycle_slot_next_attempt ON provider_egress_probe_cycle(slot_id,next_attempt_at,client_id) WHERE eligible`},
				{name: "wrong slot index order", version: 728, sql: `DROP INDEX provider_egress_probe_cycle_slot_next_attempt; CREATE INDEX provider_egress_probe_cycle_slot_next_attempt ON provider_egress_probe_cycle(next_attempt_at,slot_id,client_id) WHERE eligible`},
				{name: "future evidence included", version: 729, sql: `DROP INDEX provider_egress_health_history_url_success; CREATE INDEX provider_egress_health_history_url_success ON provider_egress_health_history(client_id,measured_at DESC) WHERE url_probe AND url_probe_policy_version>=1 AND ok_count=1`},
				{name: "old success predicate restored", version: 729, sql: `DROP INDEX provider_egress_health_history_url_success; CREATE INDEX provider_egress_health_history_url_success ON provider_egress_health_history(client_id,measured_at DESC) WHERE url_probe AND ok_count=1`},
				{name: "missing historical marker", version: 729, sql: `ALTER TABLE provider_egress_health_history DROP COLUMN url_probe CASCADE`},
				{name: "missing historical ordinal", version: 729, sql: `ALTER TABLE provider_egress_probe_cycle DROP COLUMN outcome_count`},
				{name: "legacy evidence promoted", version: 729, sql: `ALTER TABLE provider_egress_health_history ALTER COLUMN url_probe_policy_version SET DEFAULT 1`},
				{name: "fabricated lookup generation", version: 730, sql: `ALTER TABLE network_client_location ALTER COLUMN arin_database_build_epoch SET DEFAULT 1`},
				{name: "wrong lookup clock type", version: 730, sql: `ALTER TABLE network_client_location ALTER COLUMN arin_lookup_at TYPE timestamptz`},
			} {
				server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT fp2_catalog_fault`))
				server.RaisePgResult(tx.Exec(ctx, fault.sql))
				matches := read()
				if matches[fault.version-722] {
					t.Fatalf("%s passed its v%d guard", fault.name, fault.version)
				}
				row := syntheticMigrationArtifactRow(server.MigrationCount())
				for index, matched := range matches {
					row[index+133] = fmt.Sprint(matched)
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
					if alert.Class == "migration-schema-drift" && alert.Severity == SeverityPage && strings.Contains(alert.Markdown(), fmt.Sprintf("@v%d", fault.version)) {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s did not reach the owning migration alert", fault.name)
				}
				server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT fp2_catalog_fault`))
			}
			// Restore the original v726 index and prove it was required before729,
			// rather than teaching the old guard to accept the future definition.
			server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT fp2_catalog_old_index`))
			server.RaisePgResult(tx.Exec(ctx, `DROP INDEX provider_egress_health_history_url_success;
				CREATE INDEX provider_egress_health_history_url_success ON provider_egress_health_history(client_id,measured_at DESC) WHERE url_probe AND ok_count=1`))
			matches := read()
			if !matches[4] || matches[7] {
				t.Fatal("old and selected-version partial indexes are not distinguished")
			}
			server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT fp2_catalog_old_index`))
		})
	})
}

// All new guards remain safe to query against the actual pre-rollout catalog.
func TestMigrationFp2MissingFutureObjectsAreQueryable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := migrationFp2TestQuery(t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				DROP TABLE provider_egress_health_history,provider_egress_probe_cycle,provider_egress_url_security;
				ALTER TABLE network_client_location DROP COLUMN arin_risk,DROP COLUMN arin_non_quality,DROP COLUMN arin_lookup_at,DROP COLUMN arin_database_build_epoch;
				ALTER TABLE network_client_location_reliability DROP COLUMN arin_risk,DROP COLUMN arin_non_quality;
				ALTER TABLE provider_egress_health DROP COLUMN security_measured_at,DROP COLUMN legacy_tls_authentication_failure;`))
			var matches []bool
			server.Raise(tx.QueryRow(ctx, query).Scan(&matches))
			for index, matched := range matches {
				if matched {
					t.Fatalf("absent future v%d objects appear present", index+722)
				}
			}
		})
	})
}

// Every index guard rejects invalid and not-ready evidence independently. This
// uses the exact emitted helper over catalog-shaped rows without system writes.
func TestMigrationFp2IndexReadinessGuards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		guard := migrationFp2Index("provider_egress_probe_cycle", "provider_egress_probe_cycle_slot_next_attempt", "slot_id, next_attempt_at, client_id", "eligible", false)
		server.Db(ctx, func(conn server.PgConn) {
			for _, test := range []struct{ valid, ready, want bool }{
				{valid: true, ready: true, want: true}, {valid: false, ready: true}, {valid: true, ready: false},
			} {
				var matched bool
				server.Raise(conn.QueryRow(ctx, `WITH index_artifact AS (
					SELECT 'provider_egress_probe_cycle'::text AS table_name,'provider_egress_probe_cycle_slot_next_attempt'::text AS index_name,
					'CREATE INDEX provider_egress_probe_cycle_slot_next_attempt ON public.provider_egress_probe_cycle USING btree (slot_id, next_attempt_at, client_id) WHERE eligible'::text AS definition,
					'eligible'::text AS predicate_definition,$1::boolean AS indisvalid,$2::boolean AS indisready)
					SELECT `+guard, test.valid, test.ready).Scan(&matched))
				if matched != test.want {
					t.Fatalf("valid=%t ready=%t admitted=%t", test.valid, test.ready, matched)
				}
			}
		})
	})
}
