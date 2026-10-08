package taskworker

// Actual workload registries must claim the owner that shard discovery creates.

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

func TestLegacyPayerDispatchAndDrainThroughBothProfiles(t *testing.T) {
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(t testing.TB) {
			ctx := model.Testing_WithLegacyPayerSettlementCollectionWindow(t.Context())
			payer, source, provider, destination := server.NewId(), server.NewId(), server.NewId(), server.NewId()
			for _, client := range []struct{ networkId, clientId server.Id }{
				{networkId: payer, clientId: source},
				{networkId: provider, clientId: destination},
			} {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`,
						client.networkId, "synthetic-payer-profile-"+client.networkId.String(), server.NewId()))
				})
				model.Testing_CreateDevice(ctx, client.networkId, server.NewId(), client.clientId, "synthetic-payer-profile", "synthetic")
			}
			model.AddBasicTransferBalance(ctx, payer, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour))
			balances := model.GetActiveTransferBalances(ctx, payer)
			if len(balances) != 1 {
				t.Fatal("synthetic profile requires one grant")
			}
			id := server.NewId()
			shard := int(id[15]) % model.LegacySettlementShardCount
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
 VALUES($1,$2,$3,$4,$5,$2,100,true)`, id, payer, source, provider, destination))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,100)`, id, balances[0].BalanceId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',11,false),($1,'destination',11,false)`, id))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,payer_network_id) VALUES($1,$2,'settled',$3)`, id, shard, payer))
			})
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			worker, err := InitTaskWorkerForProfile(ctx, nil, profile)
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Close()
			var dispatchId server.Id
			dispatchKey := task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard))
			server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(dispatchKey)}, func(tx server.PgTx) {
				dispatchId = task.ScheduleTaskInTx(tx, work.FlushLegacySettlements, &work.FlushLegacySettlementsArgs{Shard: shard}, owner,
					dispatchKey, task.RunAt(time.Unix(1, 0)), task.RequireQueueOwnership(tx))
			}, server.TxReadCommitted, server.OptNoRetry())
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 1 || finished[0] != dispatchId || len(retried)+len(posts) != 0 {
				t.Fatal("profile did not complete its real shard dispatcher", finished, retried, posts, err)
			}
			var payerTaskId server.Id
			var availableBlock int64
			server.Db(ctx, func(conn server.PgConn) {
				var unchanged bool
				server.Raise(conn.QueryRow(ctx, `SELECT task_id,available_block FROM pending_task WHERE run_once_key=$1`,
					task.RunOnce("flush_legacy_payer_settlements", payer).String()).Scan(&payerTaskId, &availableBlock))
				server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND
 (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000
 FROM transfer_contract WHERE contract_id=$1`, id, balances[0].BalanceId).Scan(&unchanged))
				if !unchanged {
					t.Fatal("dispatcher performed financial work before payer ownership")
				}
			})
			select {
			case <-time.After(time.Until(time.Unix(availableBlock, 0))):
			case <-ctx.Done():
				t.Fatal("profile's payer owner did not become claim eligible", ctx.Err())
			}
			// The recurring dispatcher can be due before the five-second
			// collection owner. Both real profile targets remain claimable.
			finished, retried, posts, err = worker.EvalTasks(2)
			if err != nil || !slices.Contains(finished, payerTaskId) || len(retried)+len(posts) != 0 {
				t.Fatal("profile omitted or could not claim its dispatched payer owner", finished, retried, posts, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT outcome='settled' FROM transfer_contract WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=989
 AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1)=11
 AND NOT EXISTS(SELECT 1 FROM pending_task WHERE task_id=$3)`, id, balances[0].BalanceId, payerTaskId).Scan(&exact))
				if !exact {
					t.Fatal("registered payer owner lost exact financial completion")
				}
			})
		})
	}
}
