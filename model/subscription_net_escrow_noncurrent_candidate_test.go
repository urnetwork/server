package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The second pass must retain every kind of live reservation, including rows
// whose exact legacy sum is zero. Closed contracts and settled escrow are not
// witnesses even when a best-effort settled post left old rows behind.
func TestNetEscrowNoncurrentCandidateClassifiesLastWitnesses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		type scenario struct {
			name          string
			start, end    time.Time
			balance       ByteCount
			bytes         ByteCount
			settled       bool
			redisReserved bool
			dispute       bool
			outcome       any
			escrow        bool
			want          bool
		}
		scenarios := []scenario{
			{name: "expired legacy", start: now.Add(-2 * time.Hour), end: now.Add(-time.Hour), balance: 1000, bytes: 17, escrow: true, want: true},
			{name: "expired zero", start: now.Add(-2 * time.Hour), end: now.Add(-time.Hour), balance: 1000, escrow: true, want: true},
			{name: "expired native", start: now.Add(-2 * time.Hour), end: now.Add(-time.Hour), balance: 1000, bytes: 17, redisReserved: true, escrow: true, want: true},
			{name: "expired disputed", start: now.Add(-2 * time.Hour), end: now.Add(-time.Hour), balance: 1000, bytes: 23, dispute: true, escrow: true, want: true},
			{name: "closed leaked unsettled", start: now.Add(-2 * time.Hour), end: now.Add(-time.Hour), balance: 1000, bytes: 17, outcome: "canceled", escrow: true},
			{name: "settled open contract", start: now.Add(-2 * time.Hour), end: now.Add(-time.Hour), balance: 1000, bytes: 17, settled: true, escrow: true},
			{name: "expired no escrow", start: now.Add(-2 * time.Hour), end: now.Add(-time.Hour), balance: 1000},
			{name: "future", start: now.Add(time.Hour), end: now.Add(2 * time.Hour), balance: 1000, bytes: 17, escrow: true, want: true},
			{name: "inactive", start: now.Add(-time.Hour), end: now.Add(time.Hour), bytes: 17, escrow: true, want: true},
			{name: "current", start: now.Add(-time.Hour), end: now.Add(time.Hour), balance: 1000, bytes: 17, escrow: true},
		}
		want := map[server.Id]bool{}
		names := map[server.Id]string{}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, s := range scenarios {
				balanceId := server.NewId()
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
					(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
					VALUES ($1,$2,$3,$4,1000,$5,0)`, balanceId, f.sourceNetworkId, s.start, s.end, s.balance))
				if s.escrow {
					contractId := server.NewId()
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
						(contract_id,source_network_id,source_id,destination_network_id,destination_id,
						 payer_network_id,transfer_byte_count,dispute,outcome)
						VALUES ($1,$2,$3,$4,$5,$2,$6,$7,$8)`,
						contractId, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
						s.bytes, s.dispute, s.outcome))
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow
						(contract_id,balance_id,balance_byte_count,settled,redis_reserved)
						VALUES ($1,$2,$3,$4,$5)`, contractId, balanceId, s.bytes, s.settled, s.redisReserved))
				}
				if s.name != "current" {
					want[balanceId] = s.want
				}
				names[balanceId] = s.name
			}
		})
		got := map[server.Id]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, netEscrowNoncurrentOpenBalancePageSQL, now, server.Id{}, 10000)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id, networkId server.Id
					var hasOpen bool
					server.Raise(rows.Scan(&id, &networkId, &hasOpen))
					if networkId != f.sourceNetworkId {
						t.Fatal("candidate changed network ownership")
					}
					got[id] = hasOpen
				}
			})
		})
		if len(got) != len(want) {
			t.Fatalf("noncurrent candidates=%d, want %d", len(got), len(want))
		}
		for id, expected := range want {
			actual, ok := got[id]
			if !ok || actual != expected {
				t.Fatalf("%s candidate present=%t live=%t, want live=%t", names[id], ok, actual, expected)
			}
		}
	})
}

// An empty page of noncurrent candidates must still advance the keyset cursor.
// The open witness lies after 10,000 historical balances with no escrow.
func TestNetEscrowNoncurrentCandidateAdvancesPastEmptyPage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		witnessId, err := server.ParseId("ffffffff-ffff-4fff-8fff-ffffffffffff")
		server.Raise(err)
		contractId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
				SELECT md5('noncurrent-empty-page-'||n)::uuid,$1,now()-interval '2 hours',now()-interval '1 hour',1000,1000,0
				FROM generate_series(1,10000) n`, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
				VALUES ($1,$2,now()-interval '2 hours',now()-interval '1 hour',1000,1000,0)`, witnessId, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 payer_network_id,transfer_byte_count,dispute)
				VALUES ($1,$2,$3,$4,$5,$2,17,false)`,
				contractId, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow
				(contract_id,balance_id,balance_byte_count,settled,redis_reserved)
				VALUES ($1,$2,17,false,false)`, contractId, witnessId))
		})
		drift, count := ReconcileNetEscrow(ctx, false)
		if count != 2 || drift[f.sourceNetworkId] != -17 {
			t.Fatalf("late witness dry run count=%d drift=%d, want count=2 drift=-17", count, drift[f.sourceNetworkId])
		}
		drift, count = ReconcileNetEscrow(ctx, true)
		if count != 2 || drift[f.sourceNetworkId] != -17 {
			t.Fatalf("late witness repair count=%d drift=%d, want count=2 drift=-17", count, drift[f.sourceNetworkId])
		}
		if got := Testing_NetEscrowByteCount(ctx, witnessId); got != 17 {
			t.Fatalf("late witness mirror=%d, want 17", got)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var balance, reserved ByteCount
			var settled bool
			var outcome *string
			server.Raise(conn.QueryRow(ctx, `SELECT b.balance_byte_count,e.balance_byte_count,e.settled,c.outcome
				FROM transfer_balance b JOIN transfer_escrow e USING (balance_id)
				JOIN transfer_contract c USING (contract_id) WHERE b.balance_id=$1`, witnessId).
				Scan(&balance, &reserved, &settled, &outcome))
			if balance != 1000 || reserved != 17 || settled || outcome != nil {
				t.Fatal("reconciliation changed durable reservation or billing balance")
			}
		})
	})
}
