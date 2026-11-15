package taskworker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// The root cause of the paused points (owner decision 2026-10-28): past the
// provider earning boundary the weekly payout run planned only contracts that
// closed before it, and a plan applies points only inside a subsidy epoch,
// which cannot form past the boundary. So the traffic of every completed
// operator block after it earned no points. The daily award task gives them.
// USDC payouts ended (owner decision 2026-11-12) and no payout run asks for an
// award any more, so this runs the award task on its own in the production
// taskworker, with every task it queues, and requires points for the traffic
// of the completed block it awards, linked to a payment that stores that
// operator block, with the traffic's sweeps left for the SN pool, no USDC paid
// for them, and the task's own next daily run scheduled.
func TestDailyAwardTaskAwardsPostCutoffOperatorBlockPoints(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "operator-block-points.example")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		// the award must carry points, and the release gate's fixture
		// subsidy.yml configures none
		subsidyConfig := model.EnvSubsidyConfig()
		previousSubsidyConfig := *subsidyConfig
		defer func() { *subsidyConfig = previousSubsidyConfig }()
		subsidyConfig.AccountPointsPerPayout = 1000000

		now := server.NowUtc()
		current := int64(model.SubnetBlockNumber(now))
		blockStart := func(block int64) time.Time {
			return model.SubnetBlockGenesis.Add(time.Duration(block-1) * model.SubnetBlockDuration)
		}
		// Traffic in the latest block due now, its last settlements in, and
		// the block before it. The boundary is inside the block before them,
		// unless that would put the latest due block's window past
		// client_reliability retention on the wall clock, where the award
		// reads it: retention is 10 days, under two blocks, so a whole block
		// stays retained only for about 3 days after it is due. Then the
		// boundary moves into that block, an hour inside retention, and the
		// traffic follows it.
		latest := current - 1
		if now.Before(blockStart(current).Add(model.OperatorBlockPointsSettleTime)) {
			latest = current - 2
		}
		trafficBlocks := []int64{latest - 1, latest}
		cutoff := blockStart(latest - 1).Add(-2 * 24 * time.Hour)
		if retainedFrom := now.Add(-model.ClientReliabilityRetention + time.Hour).Truncate(model.ReliabilityBlockDuration); blockStart(latest).Before(retainedFrom) {
			cutoff = retainedFrom
		}
		sn := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoff.Format(time.RFC3339), "0x"+strings.Repeat("11", 32))
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte(sn)))
		policy, err := server.LoadProviderPayoutTransition(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := server.PrepareProviderPayoutBoundary(ctx, policy.ConfigSha256); err != nil {
			t.Fatal(err)
		}

		payerNetwork, payer := server.NewId(), server.NewId()
		provider, providerClient := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, payerNetwork, "weekly-points-payer", server.NewId())
		model.Testing_CreateDevice(ctx, payerNetwork, server.NewId(), payer, "weekly-points-payer", "synthetic")
		model.Testing_CreateNetwork(ctx, provider, "weekly-points-provider", server.NewId())
		model.Testing_CreateDevice(ctx, provider, server.NewId(), providerClient, "weekly-points-provider", "synthetic")
		code, err := model.CreateBalanceCode(ctx, 1024*1024*1024*1024, 365*24*time.Hour, model.UsdToNanoCents(100), "weekly-points-"+server.NewId().String(), "", "")
		if err != nil {
			t.Fatal(err)
		}
		if result, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: payerNetwork}, ctx); err != nil || result == nil || result.Error != nil {
			t.Fatal("redeem", err, result)
		}
		balances := model.GetActiveTransferBalances(ctx, payerNetwork)
		if len(balances) != 1 {
			t.Fatal("missing fixture balance")
		}
		contracts := []server.Id{}
		for _, block := range trafficBlocks {
			closed := server.MaxTime(blockStart(block).Add(time.Hour), cutoff.Add(time.Hour))
			bytes := int64(1024 * 1024 * 1024)
			contractId := server.NewId()
			usage := fmt.Sprintf(`{"version":1,"byte_count":%d,"providers":[{"client_id":%q,"network_id":%q,"byte_count":%d}]}`, bytes, providerClient, provider, bytes)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9::jsonb,true)`, contractId, payerNetwork, payer, provider, providerClient, bytes, closed.Add(-time.Hour), closed, usage))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
				(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
				VALUES($1,$2,$3,$4,$5,0,$6)`, contractId, balances[0].BalanceId, provider, providerClient, bytes, closed))
			})
			contracts = append(contracts, contractId)
		}

		// the daily award task, due now, as its startup seed leaves it when the
		// taskworker started inside the latest due block
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		task.ScheduleTask(
			controller.AwardOperatorBlockPoints,
			&controller.AwardOperatorBlockPointsArgs{FromBlock: latest},
			clientSession,
			task.RunOnce("award_operator_block_points"),
			task.RunAt(now.Add(-time.Minute)),
		)
		worker := InitTaskWorker(ctx)
		defer worker.Close()
		// the worker claims in fair turns over its functions, so it runs turns
		// until no queued task is due
		for range 2000 {
			if _, _, _, err := worker.EvalTasks(10); err != nil {
				t.Fatal(err)
			}
			var due bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pending_task WHERE run_at <= $1)`, server.NowUtc()).Scan(&due))
			})
			if !due {
				break
			}
		}

		var blocks []int64
		var paidUsdc int64
		var assigned int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT COALESCE(block.block_number, 0)
				FROM account_point point
				LEFT JOIN account_payment_block block ON block.payment_id = point.account_payment_id
				WHERE point.network_id = $1 AND 0 < point.point_value
			`, provider)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var block int64
					server.Raise(result.Scan(&block))
					blocks = append(blocks, block)
				}
			})
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(SUM(payout_nano_cents), 0)::bigint FROM account_payment WHERE network_id = $1`, provider).Scan(&paidUsdc))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_escrow_sweep WHERE contract_id = ANY($1::uuid[]) AND payment_id IS NOT NULL`, contracts).Scan(&assigned))
		})
		if len(blocks) == 0 {
			t.Fatalf("the award task gave the traffic of completed operator blocks %v past the boundary no points", trafficBlocks)
		}
		for _, block := range blocks {
			if !slices.Contains(trafficBlocks, block) {
				t.Fatalf("a point does not store an operator block with the traffic: %v", blocks)
			}
		}
		if paidUsdc != 0 || assigned != 0 {
			t.Fatalf("traffic past the boundary was paid in USDC (%d) or its sweeps assigned (%d)", paidUsdc, assigned)
		}

		// the award task keeps its own daily schedule, and nothing queues a
		// payout run
		var nextAward int
		var payoutRuns int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM pending_task WHERE function_name = $1 AND $2 < run_at`,
				task.NewTaskTarget(controller.AwardOperatorBlockPoints).TargetFunctionName(), server.NowUtc()).Scan(&nextAward))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM pending_task WHERE function_name = ANY($1)`,
				[]string{task.NewTaskTarget(work.Payout).TargetFunctionName(), "bringyour.com/service/taskworker/work.Payout"}).Scan(&payoutRuns))
		})
		if nextAward != 1 {
			t.Fatalf("the award task scheduled %d next runs, want its own next daily run", nextAward)
		}
		if payoutRuns != 0 {
			t.Fatalf("%d payout runs are queued", payoutRuns)
		}
	})
}
