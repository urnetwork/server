package model

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

func TestPostgresLogicalBackupSnapshotGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		if PostgresLogicalBackupSnapshotActive(ctx) {
			t.Fatal("fixture unexpectedly contains a logical backup")
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `SET application_name = 'pg_dump'`))
			defer conn.Exec(ctx, `RESET application_name`)
			if PostgresLogicalBackupSnapshotActive(ctx) {
				t.Fatal("idle pg_dump connection without snapshot blocked optional work")
			}
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			server.Raise(err)
			defer tx.Rollback(ctx)
			var oid uint32
			server.Raise(tx.QueryRow(ctx, `SELECT oid FROM pg_database WHERE datname = current_database()`).Scan(&oid))
			if !PostgresLogicalBackupSnapshotActive(ctx) {
				t.Fatal("held same-database pg_dump snapshot was not detected")
			}
			var selfActive bool
			server.Raise(tx.QueryRow(ctx, postgresLogicalBackupSnapshotActiveSQL).Scan(&selfActive))
			if selfActive {
				t.Fatal("backup gate counted its own observer backend")
			}
			server.Tx(ctx, func(observer server.PgTx) {
				if reliabilityRunningPeriodicReanchorAllowed(ctx, observer) {
					t.Fatal("live catalog gate allowed optional re-anchor during backup")
				}
			}, server.TxReadCommitted)
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL application_name = 'ordinary-test-snapshot'`))
			if PostgresLogicalBackupSnapshotActive(ctx) {
				t.Fatal("ordinary read snapshot was misclassified as a logical backup")
			}
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL application_name = 'pg_dump'`))
			if !PostgresLogicalBackupSnapshotActive(ctx) {
				t.Fatal("restored backup identity lost its held snapshot")
			}
			server.Raise(tx.Commit(ctx))
			if PostgresLogicalBackupSnapshotActive(ctx) {
				t.Fatal("released backup snapshot continued blocking optional work")
			}
		})
	})
}
