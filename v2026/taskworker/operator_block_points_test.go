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
// provider earning boundary the weekly payout run plans only contracts that
// closed before it, and a plan applies points only inside a subsidy epoch,
// which cannot form past the boundary. So the traffic of every completed
// operator block after it earned no points. This runs the weekly payout task
// in the production taskworker, with every task it queues, and requires
// points for the traffic of the completed block it awards, linked to a
// payment that stores that operator block, with the traffic's sweeps left
// for the SN pool and no USDC paid for them.
func TestWeeklyPayoutRunAwardsPostCutoffOperatorBlockPoints(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "operator-block-points.example")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)

		now := server.NowUtc()
		current := int64(model.SubnetBlockNumber(now))
		blockStart := func(block int64) time.Time {
			return model.SubnetBlockGenesis.Add(time.Duration(block-1) * model.SubnetBlockDuration)
		}
		// Traffic in the last two completed blocks: whichever is the latest
		// completed when the run reads the calendar has traffic. The boundary
		// is inside the block before them.
		trafficBlocks := []int64{current - 2, current - 1}
		cutoff := blockStart(current - 2).Add(-2 * 24 * time.Hour)
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
			closed := blockStart(block).Add(time.Hour)
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

		// the weekly payout run, due now, and every task it queues
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		task.ScheduleTask(work.Payout, &work.SchedulePayoutArgs{}, clientSession, task.RunOnce("payout"), task.RunAt(now.Add(-time.Minute)))
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
			t.Fatalf("the weekly run gave the traffic of completed operator blocks %v past the boundary no points", trafficBlocks)
		}
		for _, block := range blocks {
			if !slices.Contains(trafficBlocks, block) {
				t.Fatalf("a point does not store an operator block with the traffic: %v", blocks)
			}
		}
		if paidUsdc != 0 || assigned != 0 {
			t.Fatalf("traffic past the boundary was paid in USDC (%d) or its sweeps assigned (%d)", paidUsdc, assigned)
		}
	})
}
