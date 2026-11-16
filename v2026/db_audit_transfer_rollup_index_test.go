package server

import (
	"context"
	"testing"
	"time"
)

// Replay the registered online migration over same-name residue and then through
// the transactional schema-audit path. Both must produce the same usable index.
func TestTransferAuditClosedNullDayMigrationReplay(t *testing.T) {
	index := migrationIndex(t, "transfer_contract_audit_closed_null_day")
	if index+1 != 810 {
		t.Fatalf("closed-NULL audit index migration = %d, want 810", index+1)
	}
	migration, ok := migrations[index].(*OnlineSqlMigration)
	if !ok || len(migration.productionSqlSteps()) != 2 {
		t.Fatal("closed-NULL audit index needs separate recovery and online-create steps")
	}
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		const expected = "CREATE INDEX transfer_contract_audit_closed_null_day ON public.transfer_contract USING btree (close_time, contract_id) WHERE ((outcome IS NULL) AND (close_time IS NOT NULL))"
		assertIndex := func(conn PgConn) {
			t.Helper()
			var definition string
			var valid, ready, live bool
			Raise(conn.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid), indisvalid, indisready, indislive
				FROM pg_index WHERE indexrelid=to_regclass('public.transfer_contract_audit_closed_null_day')
				AND indrelid='public.transfer_contract'::regclass`).Scan(&definition, &valid, &ready, &live))
			if definition != expected || !valid || !ready || !live {
				t.Fatalf("audit index=%q valid=%t ready=%t live=%t", definition, valid, ready, live)
			}
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE transfer_contract (
				contract_id uuid PRIMARY KEY, close_time timestamp, outcome varchar(32));
				CREATE INDEX transfer_contract_audit_closed_null_day ON transfer_contract(contract_id)`))
			for replay := 0; replay < 2; replay++ {
				Raise(executeOnlineSqlMigration(ctx, migration, func(ctx context.Context, sql string) error {
					_, err := conn.Exec(ctx, sql)
					return err
				}))
				assertIndex(conn)
			}
		}, OptReadWrite(), OptNoRetry())
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, migration.auditSql))
		})
		MaintenanceDb(ctx, assertIndex, OptReadOnly(), OptNoRetry())
	})
}
