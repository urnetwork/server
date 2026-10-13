package server

import (
	"context"
	"testing"
	"time"
)

const auditDeviceEventDeviceIndexTestDefinition = "CREATE INDEX audit_device_event_device_id_event_time ON public.audit_device_event USING btree (device_id, event_time, event_id) INCLUDE (event_type)"

// Replay the registered online migration over same-name residue, again over its
// completed index, over a build made out of band, and through the transactional
// schema-audit path. Each must leave the exact usable non-partial index; a
// completed one keeps its identity instead of being rebuilt.
func TestAuditDeviceEventDeviceIndexMigrationReplay(t *testing.T) {
	index := migrationIndex(t, "audit_device_event_device_id_event_time")
	if index+1 != 830 {
		t.Fatalf("device-leading audit device event index migration = %d, want 830", index+1)
	}
	migration, ok := migrations[index].(*OnlineSqlMigration)
	if !ok || len(migration.productionSqlSteps()) != 2 {
		t.Fatal("device-leading audit device event index needs separate recovery and online-create steps")
	}
	completion := migration.completion
	if completion == nil || completion.version != 830 || completion.table != "audit_device_event" ||
		completion.index != "audit_device_event_device_id_event_time" ||
		completion.definition != auditDeviceEventDeviceIndexTestDefinition || completion.predicate != "" {
		t.Fatalf("device-leading audit device event index completion = %+v", completion)
	}
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		assertIndex := func(conn PgConn) uint32 {
			t.Helper()
			var oid uint32
			var definition string
			var predicate *string
			var valid, ready, live bool
			Raise(conn.QueryRow(ctx, `SELECT indexrelid, pg_get_indexdef(indexrelid), pg_get_expr(indpred,indrelid),
				indisvalid, indisready, indislive
				FROM pg_index WHERE indexrelid=to_regclass('public.audit_device_event_device_id_event_time')
				AND indrelid='public.audit_device_event'::regclass`).Scan(&oid, &definition, &predicate, &valid, &ready, &live))
			if definition != auditDeviceEventDeviceIndexTestDefinition || predicate != nil || !valid || !ready || !live {
				t.Fatalf("device index=%q predicate=%v valid=%t ready=%t live=%t", definition, predicate, valid, ready, live)
			}
			return oid
		}
		indexOid := func(conn PgConn) uint32 {
			t.Helper()
			var oid uint32
			Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.audit_device_event_device_id_event_time')::oid`).Scan(&oid))
			return oid
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			// Same-name residue without the covered event type is not completion.
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE audit_device_event (
				event_id uuid NOT NULL, event_time timestamp NOT NULL, device_id uuid NOT NULL,
				event_type varchar(64) NOT NULL, event_details text NULL,
				PRIMARY KEY (event_time, device_id, event_id));
				CREATE INDEX audit_device_event_device_id_event_time ON audit_device_event(device_id, event_time, event_id)`))
			residue := indexOid(conn)
			Raise(executeOnlineSqlMigrationOnConn(ctx, conn, migration))
			built := assertIndex(conn)
			if built == residue {
				t.Fatal("same-name residue without the covered event type was admitted")
			}
			Raise(executeOnlineSqlMigrationOnConn(ctx, conn, migration))
			if replayed := assertIndex(conn); replayed != built {
				t.Fatalf("completed index rebuilt on replay: oid %d became %d", built, replayed)
			}

			// An operator may build the index ahead of the migration.
			RaisePgResult(conn.Exec(ctx, `DROP INDEX audit_device_event_device_id_event_time`))
			RaisePgResult(conn.Exec(ctx, `CREATE INDEX CONCURRENTLY audit_device_event_device_id_event_time
				ON audit_device_event (device_id, event_time, event_id) INCLUDE (event_type)`))
			precreated := assertIndex(conn)
			Raise(executeOnlineSqlMigrationOnConn(ctx, conn, migration))
			if kept := assertIndex(conn); kept != precreated {
				t.Fatalf("pre-created index rebuilt: oid %d became %d", precreated, kept)
			}
			RaisePgResult(conn.Exec(ctx, `DROP INDEX audit_device_event_device_id_event_time`))
		}, OptReadWrite(), OptNoRetry())
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, migration.auditSql))
		})
		MaintenanceDb(ctx, func(conn PgConn) { assertIndex(conn) }, OptReadOnly(), OptNoRetry())
	})
}
