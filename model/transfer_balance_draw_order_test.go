package model

// QUOTA-M2-05's draw order (DESIGN.md 20.9, D-QUOTA-9): the credit line last, then by end time, start
// time and balance id, read with each balance's pending debits and no lock.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestCompareDrawOrderBalances(t *testing.T) {
	now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
	low := server.Id{1}
	high := server.Id{2}
	for _, c := range []struct {
		a    *DrawOrderBalance
		b    *DrawOrderBalance
		want int
	}{
		// the line last, though it ends first
		{a: &DrawOrderBalance{Credit: true, EndTime: now}, b: &DrawOrderBalance{EndTime: now.Add(time.Hour)}, want: 1},
		{a: &DrawOrderBalance{EndTime: now}, b: &DrawOrderBalance{EndTime: now.Add(time.Hour)}, want: -1},
		{a: &DrawOrderBalance{EndTime: now, StartTime: now.Add(-time.Hour)}, b: &DrawOrderBalance{EndTime: now, StartTime: now.Add(-2 * time.Hour)}, want: 1},
		{a: &DrawOrderBalance{EndTime: now, BalanceId: low}, b: &DrawOrderBalance{EndTime: now, BalanceId: high}, want: -1},
		{a: &DrawOrderBalance{Credit: true, EndTime: now, BalanceId: high}, b: &DrawOrderBalance{Credit: true, EndTime: now, BalanceId: high}, want: 0},
	} {
		if got := compareDrawOrderBalances(c.a, c.b); got != c.want {
			t.Fatalf("compare %+v and %+v: %d, want %d", c.a, c.b, got, c.want)
		}
	}
	connect.AssertEqual(t, (&DrawOrderBalance{BalanceByteCount: 100, PendingByteCount: 30}).RoomByteCount(), int64(70))
	connect.AssertEqual(t, (&DrawOrderBalance{BalanceByteCount: 100, PendingByteCount: 130}).RoomByteCount(), int64(0))
	connect.AssertEqual(t, (&DrawOrderBalance{BalanceByteCount: -5}).RoomByteCount(), int64(0))
}

// A network's in-window balances in draw order, depleted ones included, an older balance's NULL
// grant_kind before the line, with each balance's unapplied transfer debits held as pending.
func TestLoadDrawOrderBalances(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		insert := func(byteCount int64, grantKind *string, startTime time.Time, endTime time.Time) server.Id {
			balanceId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, balance_byte_count, pro, grant_kind)
						VALUES ($1, $2, $3, $4, $5, 0, $5, false, $6)
					`,
					balanceId, networkId, startTime, endTime, byteCount, grantKind,
				))
			})
			return balanceId
		}
		credit := "team_credit"
		free := "free"
		line := insert(1000, &credit, now.Add(-time.Hour), now.Add(time.Hour))
		older := insert(100, nil, now.Add(-time.Hour), now.Add(3*time.Hour))
		grant := insert(100, &free, now.Add(-time.Hour), now.Add(2*time.Hour))
		depleted := insert(0, &free, now.Add(-2*time.Hour), now.Add(2*time.Hour))
		insert(100, nil, now.Add(time.Hour), now.Add(4*time.Hour))
		insert(100, nil, now.Add(-4*time.Hour), now.Add(-time.Hour))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO transfer_debit_journal (contract_id, balance_id, shard, debit_byte_count) VALUES ($1, $2, get_byte(uuid_send($2::uuid), 15) % 16, 40)`,
				server.NewId(), older,
			))
		})

		balances := LoadDrawOrderBalances(ctx, networkId, now)
		balanceIds := []server.Id{}
		for _, balance := range balances {
			balanceIds = append(balanceIds, balance.BalanceId)
		}
		connect.AssertEqual(t, balanceIds, []server.Id{depleted, grant, older, line})
		connect.AssertEqual(t, balances[2].PendingByteCount, int64(40))
		connect.AssertEqual(t, balances[2].RoomByteCount(), int64(60))
		connect.AssertEqual(t, balances[3].Credit, true)
		connect.AssertEqual(t, balances[0].RoomByteCount(), int64(0))

		var inTx []*DrawOrderBalance
		server.Tx(ctx, func(tx server.PgTx) {
			inTx = LoadDrawOrderBalancesInTx(context.Background(), tx, networkId, now)
		})
		connect.AssertEqual(t, len(inTx), 4)
	})
}
