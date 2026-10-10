// Run the maintenance task on a small synthetic schema, including its real SQL.
package work

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Use the production hash rule to put this table in the current epoch. A
// changed index OID proves the body reindexed it; reltuples proves ANALYZE ran.
func TestDbMaintenanceTaskRunsItsSelectedEpochAndRejectsCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		hash := fnv.New64()
		_, _ = hash.Write([]byte("synthetic_task_maintenance"))
		var generation [8]byte
		binary.BigEndian.PutUint64(generation[:], 0)
		_, _ = hash.Write(generation[:])
		epoch := hash.Sum64() % server.DbReindexEpochs
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE TABLE synthetic_task_maintenance(id bigint PRIMARY KEY);
				INSERT INTO synthetic_task_maintenance SELECT generate_series(1,100);
				CREATE TABLE pending_task(id bigint);
				CREATE TABLE transfer_contract(id bigint);
				CREATE INDEX transfer_contract_open_partial_create_time ON transfer_contract(id);
				CREATE INDEX transfer_contract_pair_open_create_time ON transfer_contract(id);
				CREATE INDEX transfer_contract_open_destination_partial ON transfer_contract(id)`))
		})
		var before uint32
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT 'synthetic_task_maintenance_pkey'::regclass::oid`).Scan(&before))
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		if _, err := DbMaintenance(&DbMaintenanceArgs{Epoch: epoch}, owner); err != nil {
			t.Fatal("maintenance body failed", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var after uint32
			var rows int
			var estimate float64
			server.Raise(conn.QueryRow(ctx, `SELECT 'synthetic_task_maintenance_pkey'::regclass::oid,
				(SELECT count(*) FROM synthetic_task_maintenance),
				(SELECT reltuples FROM pg_class WHERE oid='synthetic_task_maintenance'::regclass)`).Scan(&after, &rows, &estimate))
			if before == after || rows != 100 || estimate != 100 {
				t.Fatal("maintenance lost data or skipped its epoch", before, after, rows, estimate)
			}
		})
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		queued := &task.Task{TaskId: server.NewId(), ArgsJson: `{}`, ClientAddress: ""}
		if _, _, err := task.NewTaskTarget(DbMaintenance).RunSpecific(canceled, queued); err == nil {
			t.Fatal("canceled maintenance was acknowledged")
		}
	})
}
