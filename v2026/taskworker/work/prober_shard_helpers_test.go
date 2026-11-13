// Reads of the prober shard registry the probes' shard tests share.
package work

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The registry state of one prober shard epoch, "" for none.
func proberShardTestState(t testing.TB, ctx context.Context, key model.ProberShardKey) string {
	t.Helper()
	state := ""
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT state FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`, key.TaskId, key.Epoch)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&state))
			}
		})
	})
	return state
}

// The network a prober shard epoch owns.
func proberShardTestNetworkId(t testing.TB, ctx context.Context, key model.ProberShardKey) server.Id {
	t.Helper()
	var networkId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`, key.TaskId, key.Epoch).Scan(&networkId))
	})
	return networkId
}

// Drains and reaps a shard epoch a test left behind.
func proberShardTestCleanup(t testing.TB, ctx context.Context, key model.ProberShardKey) {
	t.Helper()
	if err := model.DrainProberShard(ctx, key); err != nil {
		t.Fatal(err)
	}
	if deleted, err := model.ReapProberShard(ctx, key); err != nil || !deleted {
		t.Fatalf("shard %d epoch %s was not reaped: %t, %v", key.ShardIndex, key.Epoch, deleted, err)
	}
}
