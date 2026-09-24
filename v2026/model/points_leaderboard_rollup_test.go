package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
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
		batches, err := AdvancePointsBlockRollup(ctx)
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
		batches, err := AdvancePointsBlockRollup(ctx)
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
