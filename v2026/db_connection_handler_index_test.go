package server

import (
	"context"
	"testing"
	"time"
)

const connectionHandlerIndexTestDefinition = "CREATE INDEX network_client_connection_connected_handler_id ON public.network_client_connection USING btree (handler_id) WHERE connected"

// Replay the registered online migration over same-name residue, again over its
// completed index, and through the transactional schema-audit path. Each must
// leave the exact usable index; the completed one keeps its identity.
func TestConnectionHandlerIndexMigrationReplay(t *testing.T) {
	index := migrationIndex(t, "network_client_connection_connected_handler_id")
	if index+1 != 820 {
		t.Fatalf("open connection handler index migration = %d, want 820", index+1)
	}
	migration, ok := migrations[index].(*OnlineSqlMigration)
	if !ok || len(migration.productionSqlSteps()) != 2 {
		t.Fatal("open connection handler index needs separate recovery and online-create steps")
	}
	completion := migration.completion
	if completion == nil || completion.version != 820 || completion.table != "network_client_connection" ||
		completion.index != "network_client_connection_connected_handler_id" ||
		completion.definition != connectionHandlerIndexTestDefinition || completion.predicate != "connected" {
		t.Fatalf("open connection handler index completion = %+v", completion)
	}
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		assertIndex := func(conn PgConn) uint32 {
			t.Helper()
			var oid uint32
			var definition, predicate string
			var valid, ready, live bool
			Raise(conn.QueryRow(ctx, `SELECT indexrelid, pg_get_indexdef(indexrelid), pg_get_expr(indpred,indrelid),
				indisvalid, indisready, indislive
				FROM pg_index WHERE indexrelid=to_regclass('public.network_client_connection_connected_handler_id')
				AND indrelid='public.network_client_connection'::regclass`).Scan(&oid, &definition, &predicate, &valid, &ready, &live))
			if definition != connectionHandlerIndexTestDefinition || predicate != "connected" || !valid || !ready || !live {
				t.Fatalf("handler index=%q predicate=%q valid=%t ready=%t live=%t", definition, predicate, valid, ready, live)
			}
			return oid
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			// Same-name residue without the open-row predicate is not completion.
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE network_client_connection (
				connection_id uuid PRIMARY KEY, client_id uuid NOT NULL, handler_id uuid NULL, connected boolean NOT NULL DEFAULT true);
				CREATE INDEX network_client_connection_connected_handler_id ON network_client_connection(handler_id)`))
			var residue uint32
			Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.network_client_connection_connected_handler_id')::oid`).Scan(&residue))
			Raise(executeOnlineSqlMigrationOnConn(ctx, conn, migration))
			built := assertIndex(conn)
			if built == residue {
				t.Fatal("same-name residue without the open-row predicate was admitted")
			}
			Raise(executeOnlineSqlMigrationOnConn(ctx, conn, migration))
			if replayed := assertIndex(conn); replayed != built {
				t.Fatalf("completed index rebuilt on replay: oid %d became %d", built, replayed)
			}
		}, OptReadWrite(), OptNoRetry())
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, migration.auditSql))
		})
		MaintenanceDb(ctx, func(conn PgConn) { assertIndex(conn) }, OptReadOnly(), OptNoRetry())
	})
}
