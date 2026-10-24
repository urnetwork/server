package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

const retiredChildNetworkClientIndexTestDefinition = "CREATE INDEX network_client_retired_child_deactivate_time ON public.network_client USING btree (deactivate_time, client_id) WHERE ((NOT active) AND (source_client_id IS NOT NULL))"

// Pure: both migrations are appended after the close evidence retention, once
// each, in their published forms, and the monitor finds them by content.
func TestIdentityRetentionMigrationsAreAppended(t *testing.T) {
	evidenceVersion := migrationVersionOf(contractCloseReportEvidenceRetentionGuardSql)
	indexVersion := RetiredChildNetworkClientIndexMigrationVersion()
	historyVersion := ClientKeyHistoryRetentionMigrationVersion()
	if !(evidenceVersion < indexVersion && indexVersion < historyVersion && historyVersion <= MigrationCount()) {
		t.Fatalf("versions: close evidence %d, retired child index %d, key history %d, count %d", evidenceVersion, indexVersion, historyVersion, MigrationCount())
	}
	indexMigration, ok := migrations[indexVersion-1].(*OnlineSqlMigration)
	if !ok || indexMigration.recoverySql != retiredChildNetworkClientIndexRecoverySql || indexMigration.auditSql != retiredChildNetworkClientIndexAuditSql || len(indexMigration.productionSqlSteps()) != 2 {
		t.Fatal("the retired child index is not a restartable online migration")
	}
	if !strings.Contains(indexMigration.sql, "CREATE INDEX CONCURRENTLY") {
		t.Fatal("the retired child index does not build online")
	}
	historyMigration, ok := migrations[historyVersion-1].(*SqlMigration)
	if !ok || !strings.HasPrefix(strings.TrimSpace(historyMigration.sql), "SET LOCAL lock_timeout = '5s';") {
		t.Fatal("the key history retention migration does not bound its lock wait")
	}
}

// Pure: an unknown body is not a migration version.
func TestMigrationVersionOfRefusesUnknownSql(t *testing.T) {
	refused := func() (refused bool) {
		defer func() {
			refused = recover() != nil
		}()
		migrationVersionOf("SELECT 'not a migration'")
		return
	}()
	if !refused {
		t.Fatal("an unknown migration body was given a version")
	}
}

// Pure: the monitor pins function bodies split from exactly one dollar-quoted
// definition each, and the guard's horizon is the contract retention.
func TestClientKeyHistoryRetentionBodiesAreDelimited(t *testing.T) {
	for _, tag := range []string{"$st_client_key_history_immutable_guard$", "$st_client_key_retire_deleted_client$"} {
		if count := strings.Count(clientKeyHistoryRetentionSchemaSql, tag); count != 2 {
			t.Fatalf("%s occurs %d times", tag, count)
		}
	}
	guard := ClientKeyHistoryGuardBodySql()
	for _, want := range []string{
		"TG_OP = 'DELETE'",
		"current_setting('urnetwork.client_key_history_retention', true) = 'on'",
		"interval '91 days'",
		"FROM st_client_key_gone",
		"FROM st_client_key_head",
		"FROM network_client",
		"RAISE EXCEPTION 'signed client-key history is immutable'",
	} {
		if !strings.Contains(guard, want) {
			t.Fatalf("history guard body lacks %q: %q", want, guard)
		}
	}
	retire := ClientKeyRetireDeletedClientBodySql()
	for _, want := range []string{
		"UPDATE st_client_key_head SET retired = true WHERE client_id = OLD.client_id",
		"INSERT INTO st_client_key_gone",
		"WHEN OLD.source_client_id IS NULL THEN now() AT TIME ZONE 'UTC'",
		"ELSE COALESCE(OLD.deactivate_time, now() AT TIME ZONE 'UTC')",
	} {
		if !strings.Contains(retire, want) {
			t.Fatalf("client delete trigger body lacks %q: %q", want, retire)
		}
	}
	if ContractRetention != 91*24*time.Hour {
		t.Fatalf("the guard's 91 days no longer matches the contract retention %v", ContractRetention)
	}
}

// DB-backed: under the owner rule this runs only after the branch, with its
// migrations, is merged to main.
//
// Replays the online index over a same-name leftover, again over its built
// index, and through the schema audit's transactional path; each leaves the
// exact usable index the monitor pins.
func TestRetiredChildNetworkClientIndexMigrationReplay(t *testing.T) {
	migration := migrations[RetiredChildNetworkClientIndexMigrationVersion()-1].(*OnlineSqlMigration)
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		assertIndex := func(conn PgConn) {
			t.Helper()
			var definition string
			var valid, ready, live bool
			Raise(conn.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid), indisvalid, indisready, indislive
				FROM pg_index WHERE indexrelid = to_regclass('public.network_client_retired_child_deactivate_time')
				AND indrelid = 'public.network_client'::regclass`).Scan(&definition, &valid, &ready, &live))
			if definition != retiredChildNetworkClientIndexTestDefinition || !valid || !ready || !live {
				t.Fatalf("retired child index=%q valid=%t ready=%t live=%t", definition, valid, ready, live)
			}
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE network_client (
				client_id uuid PRIMARY KEY, source_client_id uuid NULL, active boolean NOT NULL DEFAULT true, deactivate_time timestamp NULL);
				CREATE INDEX network_client_retired_child_deactivate_time ON network_client (deactivate_time)`))
			Raise(executeOnlineSqlMigrationOnConn(ctx, conn, migration))
			assertIndex(conn)
			Raise(executeOnlineSqlMigrationOnConn(ctx, conn, migration))
			assertIndex(conn)
		}, OptReadWrite(), OptNoRetry())
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, migration.auditSql))
		})
		MaintenanceDb(ctx, func(conn PgConn) { assertIndex(conn) }, OptReadOnly(), OptNoRetry())
	})
}

// DB-backed: the key history migration over a populated database rewrites no
// signed row and records nothing by itself: a client deleted before it is
// left to the retention's discovery, and one deleted after it is recorded by
// the delete trigger. The guard then refuses an unopted delete and a truncate.
func TestClientKeyHistoryRetentionMigrationKeepsPopulatedHistory(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		version := ClientKeyHistoryRetentionMigrationVersion()
		ApplyDbMigrationsUpTo(ctx, version-1)
		domain := []byte(strings.Repeat("d", 32))
		insertHistory := func(conn PgConn, clientId Id, networkId Id, fill string) {
			RaisePgResult(conn.Exec(ctx, `
				INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
				VALUES ($1, 1, $2, $3, $4, '{}')
			`, clientId, domain, []byte(strings.Repeat(fill, 32)), "sha256:"+strings.Repeat(fill, 64)))
			RaisePgResult(conn.Exec(ctx, `INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation) VALUES ($1, $2, $3, 1)`, clientId, domain, networkId))
		}
		networkId, presentId, deletedId := NewId(), NewId(), NewId()
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				INSERT INTO network_client (client_id, network_id, description, create_time, auth_time)
				VALUES ($1, $2, 'test', now(), now()), ($3, $2, 'test', now(), now())
			`, presentId, networkId, deletedId))
			insertHistory(conn, presentId, networkId, "a")
			insertHistory(conn, deletedId, networkId, "b")
			RaisePgResult(conn.Exec(ctx, `DELETE FROM network_client WHERE client_id = $1`, deletedId))
		}, OptReadWrite(), OptNoRetry())

		ApplyDbMigrationsUpTo(ctx, version)
		countGone := func() (count int) {
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_client_key_gone`).Scan(&count))
			})
			return
		}
		if count := countGone(); count != 0 {
			t.Fatalf("the migration recorded %d gone clients itself", count)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			var historyCount int
			Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_client_key_history`).Scan(&historyCount))
			if historyCount != 2 {
				t.Fatalf("the migration changed signed history: %d rows", historyCount)
			}
			RaisePgResult(conn.Exec(ctx, `DELETE FROM network_client WHERE client_id = $1`, presentId))
		}, OptReadWrite(), OptNoRetry())
		if count := countGone(); count != 1 {
			t.Fatalf("a delete after the migration recorded %d gone clients, want 1", count)
		}
		for _, statement := range []string{
			`DELETE FROM st_client_key_history`,
			`TRUNCATE st_client_key_history CASCADE`,
		} {
			var err error
			MaintenanceDb(ctx, func(conn PgConn) {
				_, err = conn.Exec(ctx, statement)
			}, OptReadWrite(), OptNoRetry())
			if err == nil || !strings.Contains(err.Error(), "signed client-key history is immutable") {
				t.Fatalf("%s was not refused: %v", statement, err)
			}
		}
		ApplyDbMigrations(ctx)
		if DbVersion(ctx) != MigrationCount() {
			t.Fatal("the migrations after key history retention did not apply")
		}
	})
}
