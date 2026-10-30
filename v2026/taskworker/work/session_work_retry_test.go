// A persistently failing session member must not park its shard for an hour.
package work

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// A malformed member stays in one shard's due set, so every run fails. The
// real registered target keeps the explicit failure and count, and retries at
// the shard's own 30-second cadence instead of ordinary hour-long backoff.
func TestMaintainNetworkSessionsFailureRetriesAtShardCadence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const shard = 7
		const malformed = "synthetic-malformed-member"
		// The shard's due set; its slot form matches the session index owner.
		indexKey := fmt.Sprintf("{nsi_%02d}z", shard)
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.ZAdd(ctx, indexKey, redis.Z{Score: 0, Member: malformed}).Err()
		}))
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { scheduleNetworkSessionShard(owner, tx, shard) })
		target := NewMaintainNetworkSessionsTaskTarget()
		var id server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE function_name=$1`, target.TargetFunctionName()).Scan(&id))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error_count=16,run_at=$2,release_time=$2 WHERE task_id=$1`,
				id, time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)))
		})
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != id {
			t.Fatal("the failing shard hid its failure or lost its pending owner", finished, retried, posts, err)
		}
		var diagnostic string
		var errorCount int
		var runAt, released time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT coalesce(reschedule_error,''),reschedule_error_count,run_at,release_time FROM pending_task WHERE task_id=$1`, id).
				Scan(&diagnostic, &errorCount, &runAt, &released))
		})
		if errorCount != 17 || !strings.Contains(diagnostic, malformed) {
			t.Fatal("the shard failure lost its explicit text or count", errorCount, diagnostic)
		}
		if delay := runAt.Sub(released); delay <= 0 || maintainNetworkSessionsErrorRetryCap < delay {
			t.Fatal("one failing member parked the shard past its cadence", delay)
		}
	})
}
