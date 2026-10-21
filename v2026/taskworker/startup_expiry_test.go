// Startup-seeded expiry covers both stored accounting backends and live peers.
package taskworker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// Production startup must recover the whole pre-existing expiry population.
func TestTaskworkerStartupExpiresLegacyAndRedisContracts(t *testing.T) {
	testTaskworkerStartupExpiresContracts(t, WorkloadProfileProduction)
}

// The scoped operator retains exactly the same transfer expiry obligation.
func TestTaskworkerSubnetStartupExpiresLegacyAndRedisContracts(t *testing.T) {
	testTaskworkerStartupExpiresContracts(t, WorkloadProfileSubnetOperator)
}

// Explicit persisted deadlines force expiry despite freshly acknowledged
// checkpoints. The actual startup row is claimed through the ordinary worker;
// no wall-clock wait, helper-only selector or real-world identity is a fixture.
func testTaskworkerStartupExpiresContracts(t *testing.T, profile WorkloadProfile) {
	t.Setenv("WARP_DOMAIN", "startup-expiry.example")
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		ctx = model.WithProviderWorkSessionSource(ctx, nil)
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
		sourceId, destinationId := server.NewId(), server.NewId()
		for _, client := range []struct{ networkId, clientId server.Id }{
			{networkId: sourceNetworkId, clientId: sourceId},
			{networkId: destinationNetworkId, clientId: destinationId},
		} {
			model.Testing_CreateNetwork(ctx, client.networkId, "synthetic-startup-expiry-"+client.networkId.String(), server.NewId())
			model.Testing_CreateDevice(ctx, client.networkId, server.NewId(), client.clientId, "synthetic-expiry-client", "synthetic")
		}
		model.AddBasicTransferBalance(ctx, sourceNetworkId, 1000, server.NowUtc(), server.NowUtc().Add(24*time.Hour))
		balances := model.GetActiveTransferBalances(ctx, sourceNetworkId)
		if len(balances) != 1 {
			t.Fatal("expiry fixture must have one independent payer grant")
		}
		balanceId := balances[0].BalanceId
		legacyExpiredId, legacyLiveId, legacyNullId := server.NewId(), server.NewId(), server.NewId()
		now := server.NowUtc().Truncate(time.Millisecond)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, id := range []server.Id{legacyExpiredId, legacyLiveId, legacyNullId} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
					transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
					VALUES($1,$2,$3,$4,$5,$2,100,true,$6,$7)`,
					id, sourceNetworkId, sourceId, destinationNetworkId, destinationId, now, now.Add(model.DefaultContractExpiration)))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
					VALUES($1,$2,100)`, id, balanceId))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		// Preserve the stored legacy reservation before issuing Redis contracts
		// from the same grant; the mirror is not a second reservation authority.
		model.ReconcileNetEscrowForNetwork(ctx, sourceNetworkId, true)
		redisExpired, err := model.CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 100)
		server.Raise(err)
		redisLive, err := model.CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 100)
		server.Raise(err)
		allIds := []server.Id{legacyExpiredId, legacyLiveId, legacyNullId, redisExpired.ContractId, redisLive.ContractId}
		for _, id := range allIds {
			server.Raise(model.CloseContract(ctx, id, sourceId, 17, true))
			server.Raise(model.CloseContract(ctx, id, destinationId, 17, true))
		}
		expiredIds := []server.Id{legacyExpiredId, redisExpired.ContractId}
		liveIds := []server.Id{legacyLiveId, legacyNullId, redisLive.ContractId}
		deadline := now.Add(-time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3
				WHERE contract_id=ANY($1)`, expiredIds, deadline.Add(-model.DefaultContractExpiration), deadline))
			// A fresh NULL neighbor stays within the fallback lifetime. Old
			// NULL checkpoints now retire at creation plus 60 minutes too.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL
				WHERE contract_id=$1`, legacyNullId, now.Add(-time.Minute)))
			// Cold historical reservations must still reach Redis once the
			// expired closes commit.
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, balanceId))
		}, server.TxReadCommitted, server.OptNoRetry())
		if model.DefaultContractExpiration != 60*time.Minute || model.Testing_NetEscrowByteCount(ctx, balanceId) != 500 {
			t.Fatal("expiry fixture lost the immutable hour or mixed reservation balance")
		}

		for range 2 {
			server.Raise(initTaskScheduleForProfile(ctx, profile))
		}
		closeTarget := task.NewTaskTargetWithPost(work.CloseExpiredContracts, work.CloseExpiredContractsPost)
		mirrorTarget := model.NewLegacyNetEscrowMirrorTaskTarget()
		registered, err := InitTaskWorkerForProfile(ctx, nil, profile)
		server.Raise(err)
		if !registered.HasTarget(closeTarget.TargetFunctionName()) || !registered.HasTarget(mirrorTarget.TargetFunctionName()) {
			registered.Close()
			t.Fatal("startup omitted expiry or its legacy reservation mirror target")
		}
		registered.Close()
		var closeTaskId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			var count int
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1`, closeTarget.TargetFunctionName()).Scan(&count))
			if count != 1 {
				t.Fatal("repeated startup did not coalesce the whole expiry population", count)
			}
			var runAt time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT task_id,run_at FROM pending_task WHERE function_name=$1`, closeTarget.TargetFunctionName()).Scan(&closeTaskId, &runAt))
			if runAt.After(server.NowUtc()) {
				t.Fatal("startup postponed an already-expired population")
			}
			// Cross the queue's integer eligibility block explicitly, without a
			// sleep deciding whether an already-due startup task is claimable.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE task_id=$1`, closeTaskId, time.Unix(1, 0).UTC()))
		}, server.TxReadCommitted, server.OptNoRetry())
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(closeTarget)
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != closeTaskId || len(retried)+len(posts) != 0 {
			t.Fatal("startup-seeded expiry did not complete its bounded page", finished, retried, posts, err)
		}
		// Deadline reconciliation settles an expired legacy contract and retires
		// its intent in one transaction. Redis retains its debit journal.
		server.Db(ctx, func(conn server.PgConn) {
			var pending int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)`, expiredIds).Scan(&pending))
			if pending != 0 {
				t.Fatal("startup expiry left an expired legacy intent", pending)
			}
		})
		legacy, err := model.FlushLegacySettlements(ctx, int(legacyExpiredId[15])%model.LegacySettlementShardCount, nil, 32)
		if err != nil || legacy.Visited != 0 {
			t.Fatal("startup expiry left a legacy financial continuation", legacy, err)
		}
		debit, err := model.FlushTransferDebits(ctx, int(balanceId[15])%model.TransferDebitShardCount, nil, 1)
		if err != nil || debit.Failed != 0 {
			t.Fatal("startup expiry lost the Redis financial continuation", debit, err)
		}
		for _, id := range expiredIds {
			close, terminal := model.GetContractClose(ctx, id)
			if !terminal || close.Outcome != model.ContractOutcomeSettled {
				t.Fatal("fresh checkpoints prevented max-lifetime retirement")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var raw []byte
				server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&raw))
				var proof struct {
					ByteCount int `json:"byte_count"`
					Expiry    *struct {
						Reports map[string]struct {
							ByteCount  int  `json:"byte_count"`
							Checkpoint bool `json:"checkpoint"`
						} `json:"reports"`
					} `json:"expiry"`
				}
				if json.Unmarshal(raw, &proof) != nil || proof.ByteCount != 17 || proof.Expiry == nil || len(proof.Expiry.Reports) != 2 {
					t.Fatal("expiry lost independently retained delivered usage")
				}
				for _, report := range proof.Expiry.Reports {
					if report.ByteCount != 17 || !report.Checkpoint {
						t.Fatal("expiry replaced an authenticated original checkpoint")
					}
				}
			})
		}
		for _, id := range liveIds {
			if _, terminal := model.GetContractClose(ctx, id); terminal {
				t.Fatal("startup expired a live or NULL-deadline checkpoint peer")
			}
		}
		requireAccounting := func(stage string, wantProjected, wantAvailable model.ByteCount) {
			t.Helper()
			var credit, reserved model.ByteCount
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count,
					(SELECT COALESCE(sum(escrow.balance_byte_count),0) FROM transfer_escrow escrow
					JOIN transfer_contract contract USING(contract_id)
					WHERE escrow.balance_id=$1 AND contract.outcome IS NULL)
					FROM transfer_balance WHERE balance_id=$1`, balanceId).Scan(&credit, &reserved))
				for _, id := range expiredIds {
					var settled bool
					var payout model.ByteCount
					server.Raise(conn.QueryRow(ctx, `SELECT settled,payout_byte_count FROM transfer_escrow
						WHERE contract_id=$1 AND balance_id=$2`, id, balanceId).Scan(&settled, &payout))
					if !settled || payout != 17 {
						t.Fatal("startup expiry changed a backend's exact delivered debit", stage, settled, payout)
					}
				}
			})
			projected := model.Testing_NetEscrowByteCount(ctx, balanceId)
			available := model.GetActiveTransferBalanceByteCount(ctx, sourceNetworkId)
			t.Logf("startup expiry accounting %s: credit=%d durable_reserved=%d projected_reserved=%d available=%d", stage, credit, reserved, projected, available)
			if credit != 966 || reserved != 300 || projected != wantProjected || available != wantAvailable {
				t.Fatal("startup expiry changed live reservations or debited delivered work twice", stage, credit, reserved, projected, available)
			}
		}
		// Each owner debits 17 bytes. The deadline close's own refresh releases
		// both expired reservations from the cold projection after its commit.
		requireAccounting("after expiry", 300, 666)
		// A second process starts after both commits. Its new immediate sweep
		// cannot repeat either backend's debit or erase the three live peers.
		server.Raise(initTaskScheduleForProfile(ctx, profile))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE function_name=$1`, closeTarget.TargetFunctionName(), time.Unix(1, 0).UTC()))
		}, server.TxReadCommitted, server.OptNoRetry())
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried)+len(posts) != 0 {
			t.Fatal("repeated startup did not preserve completed accounting", finished, retried, posts, err)
		}
		requireAccounting("after repeated startup", 300, 666)
	})
}
