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
		var maxTimeSeconds int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT count(*), min(args_json), min(run_max_time_seconds)
				FROM pending_task
				WHERE function_name = $1
			`, "github.com/urnetwork/server/v2026/taskworker/work.CloseExpiredContracts")
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count, &argsJson, &maxTimeSeconds))
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
		if maxTimeSeconds != 1800 {
			t.Fatalf("scheduled max time = %ds, want 1800s", maxTimeSeconds)
		}
	})
}

func TestCloseExpiredContractsBatchCheckpointsBeforeDeadline(t *testing.T) {
	if closeExpiredContractsMaxCount != 25_000 {
		t.Fatalf("close cohort = %d, want 25000", closeExpiredContractsMaxCount)
	}
	if closeExpiredContractsParallel != 92 {
		t.Fatalf("close parallelism = %d, want 92", closeExpiredContractsParallel)
	}

	// The 100,003-row production cohort that timed out is now split into five
	// independently acknowledged task executions instead of one all-or-timeout
	// scheduler boundary.
	productionCohort := 100_003
	batches := (productionCohort + closeExpiredContractsMaxCount - 1) / closeExpiredContractsMaxCount
	if batches != 5 {
		t.Fatalf("checkpoint batches = %d, want 5", batches)
	}

	immediateThreshold := int64(closeExpiredContractsMaxCount / (4 * DefaultCloseExpiredContractsBlockSize))
	if closeExpiredContractsFull(immediateThreshold - 1) {
		t.Fatalf("%d closes unexpectedly marked the cohort full", immediateThreshold-1)
	}
	if !closeExpiredContractsFull(immediateThreshold) {
		t.Fatalf("%d closes did not request an immediate successor", immediateThreshold)
	}
}
