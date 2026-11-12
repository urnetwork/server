package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A point row as an operator block's award writes it: a payment under the
// block's plan id and the point under the same plan and payment, created when
// the award ran.
func testingInsertOperatorBlockPoint(t testing.TB, ctx context.Context, networkId server.Id, block int64, nanoPoints NanoPoints, written time.Time) {
	t.Helper()
	planId := OperatorBlockPointsPlanId(block)
	paymentId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_payment (
				payment_id, payment_plan_id, network_id, wallet_id,
				payout_byte_count, payout_nano_cents, min_sweep_time,
				create_time, block_rollup_complete, canceled
			) VALUES ($1, $2, $3, NULL, 1, 0, $4, $5, true, true)
		`, paymentId, planId, networkId, operatorBlockStart(block).UTC(), written.UTC()))
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_point (
				account_point_id, network_id, event, point_value,
				payment_plan_id, account_payment_id, create_time
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, server.NewId(), networkId, AccountPointEventPayout, nanoPoints, planId, paymentId, written.UTC()))
	})
}

// A point row of anything else: a payout plan's (planned is set) or one with
// no plan, created at its time.
func testingInsertPlainPoint(t testing.TB, ctx context.Context, networkId server.Id, planned bool, nanoPoints NanoPoints, created time.Time) {
	t.Helper()
	var planId *server.Id
	if planned {
		id := server.NewId()
		planId = &id
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_point (account_point_id, network_id, event, point_value, payment_plan_id, create_time)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, server.NewId(), networkId, AccountPointEventPayout, nanoPoints, planId, created.UTC()))
	})
}

// The root cause: an epoch's points were the point rows created in its
// window, and an operator block's points are created when its award runs. On
// Main the backfill wrote blocks 15, 16 and 17 (2026-10-04 to 10-25) on
// 2026-10-30 02:36Z, so all three fell in epoch 3 and epochs 1 and 2 showed
// none of them. Each block now counts in the epoch that contains its end
// (owner, 2026-11-12), in GET /account/epochs and in the epoch earnings email;
// every other point row still counts by its create time.
func TestEpochPointsCountOperatorBlocksInTheEpochOfTheBlockEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		point := NanoPoints(1_000_000_000)

		// Main's epochs 1 to 3 of deployment 964, each 50,400 blocks of 12 s
		epoch1Start := time.Date(2026, 10, 9, 18, 26, 28, 0, time.UTC)
		week := 7 * 24 * time.Hour
		epochs := [][2]time.Time{
			{epoch1Start, epoch1Start.Add(week)},
			{epoch1Start.Add(week), epoch1Start.Add(2 * week)},
			{epoch1Start.Add(2 * week), epoch1Start.Add(3 * week)},
		}
		// the backfill's write time, inside epoch 3
		written := time.Date(2026, 10, 30, 2, 36, 15, 0, time.UTC)
		if written.Before(epochs[2][0]) || !written.Before(epochs[2][1]) {
			t.Fatalf("the write time %s is not in epoch 3", written)
		}

		networkA, networkB := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkA, "epoch-points-a", server.NewId())
		Testing_CreateNetwork(ctx, networkB, "epoch-points-b", server.NewId())
		// blocks 15, 16 and 17 end on 10-11, 10-18 and 10-25: in epochs 1, 2 and 3
		testingInsertOperatorBlockPoint(t, ctx, networkA, 15, 10*point, written)
		testingInsertOperatorBlockPoint(t, ctx, networkA, 16, 20*point, written)
		testingInsertOperatorBlockPoint(t, ctx, networkA, 17, 30*point, written)
		testingInsertOperatorBlockPoint(t, ctx, networkB, 16, 7*point, written)
		// a payout plan's points, and points without a plan, count when written
		testingInsertPlainPoint(t, ctx, networkA, true, 5*point, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
		testingInsertPlainPoint(t, ctx, networkA, false, 1*point, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))

		wantA := []NanoPoints{(10 + 5) * point, (20 + 1) * point, 30 * point}
		wantB := []NanoPoints{0, 7 * point, 0}
		for i, window := range epochs {
			if got := GetAccountNanoPointsInWindow(ctx, networkA, window[0], window[1]); got != wantA[i] {
				t.Fatalf("epoch %d: network A has %d nano points, want %d", i+1, got, wantA[i])
			}
			if got := GetAccountNanoPointsInWindow(ctx, networkB, window[0], window[1]); got != wantB[i] {
				t.Fatalf("epoch %d: network B has %d nano points, want %d", i+1, got, wantB[i])
			}
			all := GetNetworkNanoPointsInWindow(ctx, window[0], window[1])
			if all[networkA] != wantA[i] || all[networkB] != wantB[i] {
				t.Fatalf("epoch %d: the epoch email reads A %d and B %d nano points, want %d and %d", i+1, all[networkA], all[networkB], wantA[i], wantB[i])
			}
			if _, ok := all[networkB]; ok != (0 < wantB[i]) {
				t.Fatalf("epoch %d: network B listed=%t with %d nano points", i+1, ok, wantB[i])
			}
		}

		// a block counts once: its end is in exactly one of two adjacent windows
		block16End := operatorBlockStart(16).Add(SubnetBlockDuration)
		before := GetAccountNanoPointsInWindow(ctx, networkB, block16End.Add(-time.Hour), block16End)
		from := GetAccountNanoPointsInWindow(ctx, networkB, block16End, block16End.Add(time.Hour))
		if before != 0 || from != 7*point {
			t.Fatalf("block 16 at its end: %d before and %d from, want 0 and %d", before, from, 7*point)
		}
	})
}
