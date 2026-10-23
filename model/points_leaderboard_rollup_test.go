package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

func insertPointsRollupPayment(t *testing.T, ctx context.Context, networkId server.Id, sweepTimes ...time.Time) server.Id {
	t.Helper()
	paymentId, planId := server.NewId(), server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_payment (
				payment_id, payment_plan_id, network_id, payout_byte_count,
				payout_nano_cents, min_sweep_time, create_time
			) VALUES ($1, $2, $3, 1, 1, $4, $5)
		`, paymentId, planId, networkId, SubnetBlockGenesis, SubnetBlockGenesis.Add(21*24*time.Hour)))
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_point (
				account_point_id, network_id, event, point_value,
				payment_plan_id, account_payment_id, create_time
			) VALUES ($1, $2, $3, 1, $4, $5, $6)
		`, server.NewId(), networkId, AccountPointEventPayout, planId, paymentId,
			SubnetBlockGenesis.Add(21*24*time.Hour+10*time.Hour)))
		contractId := server.NewId()
		closed := SubnetBlockGenesis.Add(20 * 24 * time.Hour)
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id,
				destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time
			) VALUES ($1, $2, $3, $2, $4, 1, $5, $6)
		`, contractId, networkId, server.NewId(), server.NewId(),
			SubnetBlockGenesis.Add(24*time.Hour), closed))
		for _, swept := range sweepTimes {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_escrow_sweep (
					contract_id, balance_id, network_id,
					payout_byte_count, payout_net_revenue_nano_cents,
					sweep_time, payment_id
				) VALUES ($1, $2, $3, 1, 1, $4, $5)
			`, contractId, server.NewId(), networkId, swept, paymentId))
		}
	})
	return paymentId
}

func TestAdvancePointsBlockRollupCreditsEachPaidSweepWeek(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "points_rollup_weeks", server.NewId())
		first, second := SubnetBlockGenesis.Add(3*24*time.Hour), SubnetBlockGenesis.Add(10*24*time.Hour)
		paymentId := insertPointsRollupPayment(t, ctx, networkId, first, second)
		batches, err := AdvancePointsBlockRollup(ctx, DefaultPointsBlockRollupSettings())
		connect.AssertEqual(t, err, nil)
		if batches < 1 {
			t.Fatal("rollup did not advance a pending payment")
		}
		server.Db(ctx, func(conn server.PgConn) {
			result, queryErr := conn.Query(ctx, `
				SELECT payment.block_rollup_complete, array_agg(block.block_number ORDER BY block.block_number)
				FROM account_payment AS payment
				JOIN account_payment_block AS block USING (payment_id)
				WHERE payment.payment_id = $1
				GROUP BY payment.payment_id
			`, paymentId)
			server.WithPgResult(result, queryErr, func() {
				if !result.Next() {
					t.Fatal("payment block rollup missing")
				}
				var complete bool
				var blocks []int64
				server.Raise(result.Scan(&complete, &blocks))
				if !complete || len(blocks) != 2 || blocks[0] != 1 || blocks[1] != 2 {
					t.Fatalf("rolled up complete=%t blocks=%v, want [1 2]", complete, blocks)
				}
			})
		})
	})
}

func TestAdvancePointsBlockRollupCompletesPaymentWithoutPaidSweeps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "points_rollup_empty", server.NewId())
		paymentId := insertPointsRollupPayment(t, ctx, networkId)
		batches, err := AdvancePointsBlockRollup(ctx, DefaultPointsBlockRollupSettings())
		connect.AssertEqual(t, err, nil)
		if batches != 1 {
			t.Fatalf("empty payment consumed %d batches, want one", batches)
		}
		server.Db(ctx, func(conn server.PgConn) {
			result, queryErr := conn.Query(ctx, `
				SELECT block_rollup_complete
				FROM account_payment WHERE payment_id = $1
			`, paymentId)
			server.WithPgResult(result, queryErr, func() {
				if !result.Next() {
					t.Fatal("payment missing")
				}
				var complete bool
				server.Raise(result.Scan(&complete))
				if !complete {
					t.Fatal("empty payment was not marked complete")
				}
			})
		})
		available, err := pointsBlockRollupComplete(ctx)
		connect.AssertEqual(t, err, nil)
		if !available {
			t.Fatal("empty completed payment was reported as incomplete")
		}
	})
}

// A Sunday payout pays the operator block that just closed. Its plan stores
// the payee's blocks with its points, so a leaderboard rebuild ranks Blocks
// and Streak without the history rollup, from the moment the plan commits and
// while its sweeps are still being assigned.
func TestWeeklyPayoutLeavesBlocksAndStreaksAvailable(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer pinPaymentPlanAccountPoints()()
		ctx := context.Background()
		cohort := newPaymentPlanInterruptionCohort(t, ctx)
		// the block that just closed; the last payout's subsidy epoch ended as
		// it opened
		weekStart := SubnetBlockStart(server.NowUtc()).Add(-SubnetBlockDuration)
		week := uint64(SubnetBlockNumber(weekStart))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO subsidy_payment (
					payment_plan_id, start_time, end_time,
					active_user_count, paid_user_count,
					net_payout_byte_count_paid, net_payout_byte_count_unpaid,
					net_revenue_nano_cents, net_payout_nano_cents
				) VALUES ($1, $2, $3, 0, 0, 0, 0, 0, 0)`,
				server.NewId(), weekStart.Add(-5*24*time.Hour), weekStart,
			))
		})
		cohort.insert(t, ctx, 3, weekStart.Add(time.Hour), weekStart.Add(2*24*time.Hour))
		// the payout also selects a sweep without payout bytes from the block
		// before, which is not paid traffic
		server.Tx(ctx, func(tx server.PgTx) {
			contractId := server.NewId()
			closeTime := weekStart.Add(-2 * 24 * time.Hour)
			usedByteCount := ByteCount(1024 * 1024)
			usage := &contractUsageSnapshot{Version: 1, ByteCount: usedByteCount,
				Providers: []contractProviderUsage{{ClientId: cohort.destinationId, NetworkId: cohort.destinationNetworkId, ByteCount: usedByteCount}}}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9,true)`,
				contractId, cohort.sourceNetworkId, cohort.sourceId, cohort.destinationNetworkId, cohort.destinationId,
				usedByteCount, closeTime.Add(-time.Hour), closeTime, usage))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
				(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
				VALUES($1,$2,$3,$4,0,0,$5)`, contractId, cohort.balanceId, cohort.destinationNetworkId, cohort.destinationId, closeTime))
		})
		windows := []PointsEpochWindow{
			{Epoch: week - 1, Start: weekStart.Add(-SubnetBlockDuration), End: weekStart},
			{Epoch: week, Start: weekStart, End: weekStart.Add(SubnetBlockDuration)},
		}
		assertRanked := func(when string) {
			t.Helper()
			snapshot, err := RebuildPointsLeaderboard(ctx, windows)
			if err != nil {
				t.Fatal(when, err)
			}
			if !snapshot.EpochMetricsAvailable || snapshot.LatestEpoch != week {
				t.Fatalf("%s: block metrics available=%t latest=%d, want available through %d", when, snapshot.EpochMetricsAvailable, snapshot.LatestEpoch, week)
			}
			row := GetPointsLeaderboardNetworkRow(ctx, snapshot.SnapshotId, cohort.destinationNetworkId)
			if row == nil || row.BlocksWithPoints != 1 || row.Streak != 1 || row.LongestStreak != 1 {
				t.Fatalf("%s: payee ranked %+v, want one block and a streak of one", when, row)
			}
		}

		// the payout drains after its first batch: the plan has committed and
		// its sweeps are still being assigned
		maxDuration := 20 * 24 * time.Hour
		if err := runPaymentPlanUntilDrain(t, ctx, maxDuration, 2, 1); !errors.Is(err, context.Canceled) {
			t.Fatal("payout was not drained after its first batch", err)
		}
		assertRanked("assigning")

		// the next run completes the payout, and the history rollup finds
		// nothing to do
		if _, err := PlanPaymentsWithMaxDurationLoopInTx(ctx, maxDuration, nil, nil); err != nil {
			t.Fatal("resumed payout failed", err)
		}
		if batches, err := AdvancePointsBlockRollup(ctx, DefaultPointsBlockRollupSettings()); err != nil || batches != 0 {
			t.Fatal("history rollup visited a payout's payment", batches, err)
		}
		assertRanked("paid")
	})
}
