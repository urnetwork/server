// Exercise versioned snapshots against real PostgreSQL and Redis. Explicit
// retained snapshots and transaction boundaries force each ordering.
package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// A page fixed before creation must not erase the committed reservation, even
// when the new mirror is already visible before the old page reaches Redis.
func TestNetEscrowStalePagePreservesNewCreation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		ids := []server.Id{f.balanceId}
		stale := openEscrowReservedForBalances(ctx, ids)
		_, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		for _, apply := range []bool{false, true} {
			drift := reconcileNetEscrowBatch(ctx, stale, ids, apply)
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 || drift[f.balanceId] != 0 {
				t.Fatalf("stale page apply=%t: mirror=%d drift=%d", apply, got, drift[f.balanceId])
			}
		}
	})
}

// Zero amounts delete only the counter, retaining the revision fence. A late
// positive snapshot cannot recreate it after settlement or billing retention.
func TestNetEscrowZeroSnapshotRetainsRevisionFence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		ids := []server.Id{f.balanceId}
		stale := openEscrowReservedForBalances(ctx, ids)
		settlePosts := settleNetEscrowOrderingTestContract(ctx, contract.ContractId)
		// The settlement post is lost; reconciliation must repair its mirror.
		ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		server.RunPosts(ctx, settlePosts...)
		reconcileNetEscrowBatch(ctx, stale, ids, true)
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Get(ctx, netEscrowKey(f.balanceId)).Err(); err != redis.Nil {
				t.Fatalf("settled counter exists: %v", err)
			}
			fence, err := r.Get(ctx, netEscrowRevisionKey(f.balanceId)).Result()
			server.Raise(err)
			if !strings.HasSuffix(fence, ":0") || r.TTL(ctx, netEscrowRevisionKey(f.balanceId)).Val() != -1 {
				t.Fatalf("zero revision fence must remain without expiry: %q", fence)
			}
		})
	})
}

// Counter expiry cannot erase the ordering fence, and replaying the current
// source repairs loss without allowing a retained older zero page to win.
func TestNetEscrowCounterLossPreservesFenceAndRepairs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		ids := []server.Id{f.balanceId}
		stale := openEscrowReservedForBalances(ctx, ids)
		_, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		current := openEscrowReservedForBalances(ctx, ids)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.PExpireAt(ctx, netEscrowKey(f.balanceId), time.Unix(0, 0)).Err())
		})
		reconcileNetEscrowBatch(ctx, stale, ids, true)
		reconcileNetEscrowBatch(ctx, current, ids, true)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatalf("repaired expired counter = %d, want 17", got)
		}
	})
}

// A rolled-back reservation update never consumes a durable revision. A
// subsequent committed retry publishes its amount exactly once.
func TestNetEscrowRevisionRollsBackWithReservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		ids := []server.Id{f.balanceId}
		before := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.Begin(ctx)
			server.Raise(err)
			defer tx.Rollback(ctx)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=23 WHERE contract_id=$1`, contract.ContractId))
			observed := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
			if observed.revision != before.revision || observed.reserved != 17 {
				t.Fatalf("uncommitted state escaped: %+v, before %+v", observed, before)
			}
			server.Raise(tx.Rollback(ctx))
			server.RaisePgResult(conn.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=23 WHERE contract_id=$1`, contract.ContractId))
		})
		after := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
		if after.revision != before.revision+1 || after.reserved != 23 {
			t.Fatalf("committed retry did not advance once: %+v, before %+v", after, before)
		}
		refreshNetEscrow(ctx, ids)
		refreshNetEscrow(ctx, ids)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 23 {
			t.Fatalf("retried reservation mirror=%d, want 23", got)
		}
	})
}

// Direct transition mutations cover every source predicate, including paths
// outside the normal create/settle API. The amount and revision must move in
// the same snapshot; the original payer balance remains the lookup identity.
func TestNetEscrowRevisionTriggerCoverage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, test := range []struct {
			name string
			sql  string
			want ByteCount
		}{
			{name: "escrow bytes", sql: `UPDATE transfer_escrow SET balance_byte_count=23 WHERE contract_id=$1`, want: 23},
			{name: "escrow partial index", sql: `UPDATE transfer_escrow SET settled=true WHERE contract_id=$1`, want: 0},
			{name: "escrow deletion", sql: `DELETE FROM transfer_escrow WHERE contract_id=$1`, want: 0},
			{name: "contract deletion", sql: `DELETE FROM transfer_contract WHERE contract_id=$1`, want: 0},
			{name: "quarantine outcome", sql: `UPDATE transfer_contract SET outcome='canceled', close_time=now() WHERE contract_id=$1`, want: 0},
			{name: "balance deletion", sql: `DELETE FROM transfer_balance WHERE balance_id IN (SELECT balance_id FROM transfer_escrow WHERE contract_id=$1)`, want: 0},
		} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			contract, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
			server.RunPosts(ctx, posts...)
			ids := []server.Id{f.balanceId}
			before := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, test.sql, contract.ContractId))
			})
			after := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
			if after.revision <= before.revision || after.reserved != test.want {
				t.Fatalf("%s: before=%+v, after=%+v, want bytes=%d", test.name, before, after, test.want)
			}
			releaseNetEscrowForContract(ctx, contract.ContractId)
			refreshNetEscrow(ctx, ids)
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != test.want {
				t.Fatalf("%s: mirror=%d, want %d", test.name, got, test.want)
			}
		}
	})
}

// Escrow can predate its contract; later insertion changes the join predicate.
// Moving the escrow row must fence both the old and new balance identities.
func TestNetEscrowRevisionContractInsertAndEscrowMove(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := newNetEscrowOrderingTestFixture(t, ctx)
		contractId := server.NewId()
		ids := []server.Id{f.balanceId, neighbor.balanceId}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,17)`, contractId, f.balanceId))
		})
		orphan := openEscrowReservedForBalances(ctx, ids)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count) VALUES($1,$2,$3,$4,$5,17)`,
				contractId, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
		})
		inserted := openEscrowReservedForBalances(ctx, ids)
		if inserted[f.balanceId].revision <= orphan[f.balanceId].revision || inserted[f.balanceId].reserved != 17 {
			t.Fatalf("contract insertion did not fence reservation: before=%+v after=%+v", orphan, inserted)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE transfer_escrow SET balance_id=$2 WHERE contract_id=$1`, contractId, neighbor.balanceId))
		})
		moved := openEscrowReservedForBalances(ctx, ids)
		if moved[f.balanceId].reserved != 0 || moved[neighbor.balanceId].reserved != 17 ||
			moved[f.balanceId].revision <= inserted[f.balanceId].revision || moved[neighbor.balanceId].revision <= inserted[neighbor.balanceId].revision {
			t.Fatalf("escrow move did not fence both balances: before=%+v after=%+v", inserted, moved)
		}
	})
}

// Revision comparisons stay exact above Lua's integer precision boundary.
// Conflicting source amounts at one revision are integrity failures, whereas
// ordinary counter corruption at that revision remains repairable.
func TestNetEscrowSnapshotFencePrecisionAndConflict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		id := server.NewId()
		server.Redis(ctx, func(r server.RedisClient) {
			latest := netEscrowSnapshot{revision: 1<<53 + 1, reserved: 1<<53 + 1}
			server.Raise(applyNetEscrowSnapshot(ctx, r, id, latest, true).Err())
			server.Raise(applyNetEscrowSnapshot(ctx, r, id, netEscrowSnapshot{revision: 1 << 53, reserved: 0}, true).Err())
			if got := r.Get(ctx, netEscrowKey(id)).Val(); got != fmt.Sprint(latest.reserved) {
				t.Fatalf("rounded counter/revision: %q", got)
			}
			conflict := latest
			conflict.reserved = 9
			if err := applyNetEscrowSnapshot(ctx, r, id, conflict, true).Err(); err == nil || !strings.Contains(err.Error(), "conflicting net escrow") {
				t.Fatalf("same-revision conflict accepted: %v", err)
			}
			server.Raise(r.Set(ctx, netEscrowKey(id), 42, time.Hour).Err())
			server.Raise(applyNetEscrowSnapshot(ctx, r, id, latest, true).Err())
			if got := r.Get(ctx, netEscrowKey(id)).Val(); got != fmt.Sprint(latest.reserved) {
				t.Fatalf("counter corruption was not repaired: %q", got)
			}
		})
	})
}

// Repeated correct snapshots leave the precise counter expiry untouched.
func TestNetEscrowReconcileSkipsInBandMirrorWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		id := server.NewId()
		snapshot := netEscrowSnapshot{revision: 1, reserved: 80}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(applyNetEscrowSnapshot(ctx, r, id, snapshot, true).Err())
			server.Raise(r.Expire(ctx, netEscrowKey(id), 30*time.Minute).Err())
			server.Raise(applyNetEscrowSnapshot(ctx, r, id, snapshot, true).Err())
			if ttl := r.TTL(ctx, netEscrowKey(id)).Val(); ttl <= 0 || time.Hour <= ttl {
				t.Fatalf("matching snapshot refreshed original expiry: %s", ttl)
			}
		})
	})
}

// A deleted balance and a reused identity cannot reset ordering history; the
// database refuses direct revision rollback and source-table truncate as well.
func TestNetEscrowRevisionRetainsDeletedBalanceIdentity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		_, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		ids := []server.Id{f.balanceId}
		before := openEscrowReservedForBalances(ctx, ids)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId))
		})
		removed := openEscrowReservedForBalances(ctx, ids)
		reconcileNetEscrowBatch(ctx, removed, ids, true)
		reconcileNetEscrowBatch(ctx, before, ids, true)
		if removed[f.balanceId].revision <= before[f.balanceId].revision || Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
			t.Fatalf("deleted balance lost tombstone: before=%+v removed=%+v", before, removed)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO transfer_balance(balance_id,network_id,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
				VALUES ($1,$2,now()+interval '1 hour',1000,1000,0)`, f.balanceId, f.sourceNetworkId))
			for _, query := range []string{
				`DELETE FROM transfer_balance_net_escrow_revision`,
				`UPDATE transfer_balance_net_escrow_revision SET revision=revision-1`,
				`TRUNCATE transfer_balance_net_escrow_revision`,
				`TRUNCATE transfer_balance`,
				`TRUNCATE transfer_escrow`,
			} {
				if _, err := conn.Exec(ctx, query); err == nil {
					t.Fatalf("reservation fence accepted destructive statement: %s", query)
				}
			}
		})
		restored := openEscrowReservedForBalances(ctx, ids)
		if restored[f.balanceId].revision <= removed[f.balanceId].revision || restored[f.balanceId].reserved != 17 {
			t.Fatalf("reused balance identity did not advance: removed=%+v restored=%+v", removed, restored)
		}
		reconcileNetEscrowBatch(ctx, restored, ids, true)
		reconcileNetEscrowBatch(ctx, removed, ids, true)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatalf("old tombstone erased reused balance reservation: %d", got)
		}
	})
}

// Repair an absent expiry without extending a healthy shorter one. Permanent
// ordering fences are intentional; permanent reservation counters are not.
func TestNetEscrowSnapshotRepairsLostCounterExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		id := server.NewId()
		snapshot := netEscrowSnapshot{revision: 1, reserved: 80}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(applyNetEscrowSnapshot(ctx, r, id, snapshot, true).Err())
			server.Raise(r.Persist(ctx, netEscrowKey(id)).Err())
			server.Raise(applyNetEscrowSnapshot(ctx, r, id, snapshot, true).Err())
			if ttl := r.TTL(ctx, netEscrowKey(id)).Val(); ttl <= 0 || netEscrowFallbackTtl < ttl {
				t.Fatalf("permanent counter expiry not repaired: %s", ttl)
			}
		})
	})
}

// A zero-byte anchor changes neither reservation amount nor ordering history.
// Preserve the cache no-op in both an unused and an already-reserved balance.
func TestNetEscrowZeroByteContractDoesNotPublish(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		ids := []server.Id{f.balanceId}
		for _, reserved := range []ByteCount{0, 17} {
			if reserved > 0 {
				_, posts := createNetEscrowOrderingTestContract(ctx, f, reserved)
				server.RunPosts(ctx, posts...)
			}
			before := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
			contract, posts := createNetEscrowOrderingTestContract(ctx, f, 0)
			if len(posts) != 0 {
				t.Fatalf("zero-byte create queued %d posts", len(posts))
			}
			server.RunPosts(ctx, settleNetEscrowOrderingTestContract(ctx, contract.ContractId)...)
			after := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
			if after.revision != before.revision || after.reserved != reserved {
				t.Fatalf("zero-byte lifecycle changed reservation revision: before=%+v after=%+v", before, after)
			}
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != reserved {
				t.Fatalf("zero-byte lifecycle changed mirror=%d, want %d", got, reserved)
			}
			if reserved == 0 {
				server.Redis(ctx, func(r server.RedisClient) {
					if got := r.Exists(ctx, netEscrowKey(f.balanceId), netEscrowRevisionKey(f.balanceId)).Val(); got != 0 {
						t.Fatalf("zero-byte lifecycle created %d cache keys", got)
					}
				})
			}
		}
	})
}
