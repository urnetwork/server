// The maintenance task chain keeps an epoch that a drain or the load guard
// stopped, instead of advancing past the tables it never reached.
package work

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A drain cancels the task while a table rebuild waits. The post must schedule
// the same epoch again, without the interrupted table among its done tables.
// Before the fix the interrupted body returned success with nothing recorded
// and the post scheduled the next epoch, so every table the run had not reached
// waited another full cycle and the interrupted table's copies were stranded.
// Uses only the task body and post, so it runs unchanged on the old code.
func TestDbMaintenanceTaskContinuesAnInterruptedEpoch(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_task_interrupted"
		hash := fnv.New64()
		_, _ = hash.Write([]byte(tableName))
		var generation [8]byte
		binary.BigEndian.PutUint64(generation[:], 0)
		_, _ = hash.Write(generation[:])
		epoch := hash.Sum64() % server.DbReindexEpochs
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_task_interrupted (id bigint PRIMARY KEY);
				INSERT INTO synthetic_task_interrupted SELECT generate_series(1, 100)
			`))
		})

		// a writer holds the table so its rebuild waits after creating copies
		holder, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		holderTx, err := holder.Begin(ctx)
		if err != nil {
			holder.Release()
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = holderTx.Rollback(context.WithoutCancel(ctx))
				holder.Release()
			}
		}
		defer release()
		server.RaisePgResult(holderTx.Exec(ctx, `LOCK TABLE synthetic_task_interrupted IN ROW EXCLUSIVE MODE`))

		runCtx, runCancel := context.WithCancel(ctx)
		defer runCancel()
		owner := session.NewLocalClientSession(runCtx, "", nil)
		defer owner.Cancel()
		args := &DbMaintenanceArgs{Epoch: epoch}
		type bodyReturn struct {
			result *DbMaintenanceResult
			err    error
		}
		returned := make(chan bodyReturn, 1)
		go func() {
			var r bodyReturn
			server.HandleError(func() {
				r.result, r.err = DbMaintenance(args, owner)
			}, func(err error) {
				r.err = err
			})
			returned <- r
		}()

		// barrier: the table's rebuild (its progress may name the toast relation) waits on a lock
		awaitBuild := func(blocked bool) int32 {
			deadline := time.Now().Add(60 * time.Second)
			for {
				var pids []int32
				server.Db(ctx, func(conn server.PgConn) {
					result, err := conn.Query(ctx, `
						SELECT progress.pid
						FROM pg_stat_progress_create_index progress
						INNER JOIN pg_stat_activity activity ON activity.pid = progress.pid
						INNER JOIN pg_class table_class ON table_class.oid = 'public.synthetic_task_interrupted'::regclass
						WHERE progress.datname = current_database()
						  AND progress.relid IN (table_class.oid, table_class.reltoastrelid)
						  AND activity.wait_event_type = 'Lock'
					`)
					server.WithPgResult(result, err, func() {
						for result.Next() {
							var pid int32
							server.Raise(result.Scan(&pid))
							pids = append(pids, pid)
						}
					})
				})
				if blocked == (len(pids) == 1) {
					if blocked {
						return pids[0]
					}
					return 0
				}
				if time.Now().After(deadline) {
					t.Fatalf("rebuild blocked=%t not observed (found %v)", blocked, pids)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		awaitBuild(true)
		// what a taskworker drain does to the in-flight task
		runCancel()
		awaitBuild(false)
		release()
		var body bodyReturn
		select {
		case body = <-returned:
		case <-time.After(60 * time.Second):
			t.Fatal("interrupted maintenance body did not return")
		}
		if body.err != nil {
			t.Fatal("interrupted maintenance body failed", body.err)
		}

		postOwner := session.NewLocalClientSession(ctx, "", nil)
		defer postOwner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(DbMaintenancePost(args, body.result, postOwner, tx))
		})

		var argsJson string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT args_json FROM pending_task WHERE function_name = $1`,
				task.NewTaskTarget(DbMaintenance).TargetFunctionName(),
			).Scan(&argsJson))
		})
		var next struct {
			Epoch      uint64   `json:"epoch"`
			DoneTables []string `json:"done_tables"`
		}
		server.Raise(json.Unmarshal([]byte(argsJson), &next))
		if next.Epoch != epoch || slices.Contains(next.DoneTables, tableName) {
			t.Fatalf("next maintenance task %s after an interrupted epoch %d, want the same epoch with %s still to do", argsJson, epoch, tableName)
		}
	})
}
