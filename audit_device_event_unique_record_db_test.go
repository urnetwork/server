// The record of UNIQUE (event_id)'s drop (audit_device_event_unique_record.go)
// against PostgreSQL: a fresh database has no such constraint; on Main, where
// the prepare already dropped it, the migration takes no lock on the table;
// a database that still has it drops it.
package server

import (
	"context"
	"testing"
	"time"
)

const auditDeviceEventUniqueRecordConstraintSql = `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'public.audit_device_event'::regclass AND conname = 'audit_device_event_event_id_key')`

// The migrations leave audit_device_event without the unique event id
// constraint, as Main is after its prepare, so db audit compares like with
// like.
func TestAuditDeviceEventFreshSchemaHasNoUniqueEventId(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if 0 == AuditDeviceEventUniqueRecordVersion() {
			t.Fatal("the record migration is not in the list")
		}
		if testContractPartitionScalar[bool](t, ctx, auditDeviceEventUniqueRecordConstraintSql) {
			t.Fatal("a fresh database still has UNIQUE (event_id) on audit_device_event")
		}
		if testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'public.audit_device_event'::regclass AND contype = 'u')`) {
			t.Fatal("audit_device_event has a unique constraint")
		}
	})
}

// Where the constraint is gone, as on Main, the migration reads pg_constraint
// only: it completes while another session holds a lock on the table that
// any ALTER TABLE would wait for. Where it is present, the migration drops it.
func TestAuditDeviceEventUniqueRecordMigration(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		holder, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Release()
		RaisePgResult(holder.Exec(ctx, `BEGIN`))
		RaisePgResult(holder.Exec(ctx, `LOCK TABLE audit_device_event IN ACCESS SHARE MODE`))
		start := time.Now()
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, AuditDeviceEventUniqueRecordSql))
		})
		if elapsed := time.Since(start); 4*time.Second < elapsed {
			t.Fatalf("the migration waited %s for the table's lock", elapsed)
		}
		RaisePgResult(holder.Exec(ctx, `COMMIT`))

		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `ALTER TABLE audit_device_event ADD CONSTRAINT audit_device_event_event_id_key UNIQUE (event_id)`))
		})
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, AuditDeviceEventUniqueRecordSql))
		})
		if testContractPartitionScalar[bool](t, ctx, auditDeviceEventUniqueRecordConstraintSql) {
			t.Fatal("the migration left UNIQUE (event_id) in place")
		}
	})
}
