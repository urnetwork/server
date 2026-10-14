// Fresh installation and exact historical repair preserve evidence without a global fence.
package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Real statement and row triggers for a second client must commit while the
// first client's actual connection insertion remains uncommitted.
func assertProviderWorkSessionIndependentClients(t testing.TB, ctx context.Context) {
	t.Helper()
	clientIds := []Id{NewId(), NewId()}
	Tx(ctx, func(tx PgTx) {
		for _, clientId := range clientIds {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,active) VALUES($1,$2,true)`, clientId, NewId()))
		}
	})
	const insertSql = `INSERT INTO network_client_connection
	 (client_id,connection_id,connect_time,connection_host,connection_service,connection_block,
	 client_address_hash,client_address_port,handler_id)
	 VALUES($1,$2,$3,'synthetic.example','test','test',$4,10001,$5)`
	Db(ctx, func(conn PgConn) {
		held, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		Raise(err)
		defer rollbackTx(ctx, held)
		RaisePgResult(held.Exec(ctx, insertSql, clientIds[0], NewId(), NowUtc(), make([]byte, 32), NewId()))
		Db(ctx, func(other PgConn) {
			independent, err := other.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			Raise(err)
			defer rollbackTx(ctx, independent)
			RaisePgResult(independent.Exec(ctx, `SET LOCAL lock_timeout='1s'`))
			if _, err := independent.Exec(ctx, insertSql, clientIds[1], NewId(), NowUtc(), make([]byte, 32), NewId()); err != nil {
				t.Fatal("independent client waited behind an unrelated connection insertion", err)
			}
			Raise(independent.Commit(ctx))
		})
		Raise(held.Rollback(ctx))
	})
	Db(ctx, func(conn PgConn) {
		var rolledBack, committed int
		Raise(conn.QueryRow(ctx, `SELECT
		 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
		 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$2)`, clientIds[0], clientIds[1]).Scan(&rolledBack, &committed))
		if rolledBack != 0 || committed != 1 {
			t.Fatal("independent commits changed journal atomicity", rolledBack, committed)
		}
	})
}

// v776 itself is safe before later migrations can run or be retried.
func TestProviderWorkSessionFreshMigrationAvoidsGlobalWriterFence(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		ApplyDbMigrationsUpTo(ctx, 776)
		assertProviderWorkSessionIndependentClients(t, ctx)
	})
}

// A real old catalog identity survives the appended correction byte-for-byte;
// only the two lock functions change, and already-retained events keep custody.
func TestProviderWorkSessionMigrationRepairsInstalledGlobalFence(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		ApplyDbMigrationsUpTo(ctx, 775)
		oldIdentity, err := migrationIdentity(newSqlMigration(testLegacyProviderWorkSessionSchemaSql))
		Raise(err)
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, testLegacyProviderWorkSessionSchemaSql))
			Raise(recordMigrationIdentity(ctx, tx, 775, oldIdentity))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO migration_audit(start_version_number,end_version_number,status) VALUES(775,776,'success')`))
		})
		ApplyDbMigrationsUpTo(ctx, 781)
		clientId, connectionId := NewId(), NewId()
		var originalEvent string
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SELECT provider_work_session_append($1,$2,'admit',NULL)`, clientId, connectionId))
			Raise(tx.QueryRow(ctx, `SELECT row_to_json(e)::text FROM provider_work_session_event e WHERE client_id=$1`, clientId).Scan(&originalEvent))
		})
		Db(ctx, func(conn PgConn) {
			var legacy bool
			Raise(conn.QueryRow(ctx, `SELECT position('pg_advisory_xact_lock(-776' in prosrc)>0 FROM pg_proc WHERE oid='provider_work_session_statement_fence()'::regprocedure`).Scan(&legacy))
			if !legacy {
				t.Fatal("historical fixture did not install the global exclusive fence")
			}
		})
		ApplyDbMigrations(ctx)
		assertProviderWorkSessionIndependentClients(t, ctx)
		// Function replacement is safe to replay without changing history.
		MaintenanceTx(ctx, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, providerWorkSessionContentionRepairSql)) })
		Db(ctx, func(conn PgConn) {
			var recorded, retained string
			var sequence int
			Raise(conn.QueryRow(ctx, `SELECT trim(identity_sha256) FROM migration_catalog WHERE migration_index=775`).Scan(&recorded))
			Raise(conn.QueryRow(ctx, `SELECT row_to_json(e)::text,h.sequence FROM provider_work_session_event e JOIN provider_work_session_head h USING(client_id) WHERE client_id=$1`, clientId).Scan(&retained, &sequence))
			if recorded != oldIdentity || retained != originalEvent || sequence != 1 {
				t.Fatal("fence repair rewrote migration history, original evidence, or journal birth")
			}
		})
	})
}

// Historical acceptance requires the exact current SQL, old SQL and ordinal.
func TestProviderWorkSessionMigrationIdentityAliasStaysExact(t *testing.T) {
	current, err := MigrationIdentity(775)
	Raise(err)
	old, err := migrationIdentity(newSqlMigration(testLegacyProviderWorkSessionSchemaSql))
	Raise(err)
	if !matchesProviderWorkSessionMigrationIdentity(775, current, old) {
		t.Fatal("exact historical v776 migration identity lost")
	}
	if matchesProviderWorkSessionMigrationIdentity(776, current, old) ||
		matchesProviderWorkSessionMigrationIdentity(775, strings.Repeat("0", 64), old) ||
		matchesProviderWorkSessionMigrationIdentity(775, current, strings.Repeat("0", 64)) {
		t.Fatal("historical v776 alias accepted another slot or source identity")
	}
	entries := testMigrationCatalogEntries(t, len(migrations))
	entries[775].Identity = old
	if err := validateMigrationCatalog(len(entries), entries); err != nil {
		t.Fatal(err)
	}
}
