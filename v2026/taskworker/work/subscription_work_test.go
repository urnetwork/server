package work

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func TestCloseExpiredContractsSchedulingConvergesToOneDatabaseScan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		// Simulate an old caller still attempting to seed all eight historical
		// shards. Normalization plus RunOnce must converge them to one row.
		server.Tx(ctx, func(tx server.PgTx) {
			for i := 0; i < 8; i++ {
				ScheduleCloseExpiredContracts(clientSession, tx, i, false)
			}
		})

		var count int
		var argsJson string
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT count(*), min(args_json)
				FROM pending_task
				WHERE function_name = $1
			`, "github.com/urnetwork/server/v2026/taskworker/work.CloseExpiredContracts")
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count, &argsJson))
				}
			})
		})

		if count != 1 {
			t.Fatalf("scheduled close scans = %d, want 1", count)
		}
		var args CloseExpiredContractsArgs
		if err := json.Unmarshal([]byte(argsJson), &args); err != nil {
			t.Fatalf("decode scheduled args: %v", err)
		}
		if args.BlockSize != 1 || args.BlockIndex != 0 {
			t.Fatalf("scheduled shard = %+v, want block_size=1 block_index=0", args)
		}
	})
}
