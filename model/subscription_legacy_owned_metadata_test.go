// Inline metadata reuses only financial authority held in the same transaction.
package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Cold and warm owners finish both revision transitions without repeated
// ownership reads. Unusual reservation sets retain the complete old path.
func TestLegacySettlementOwnedMetadataCoverageAndSnapshots(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		for _, mode := range []string{"warm", "cold", "multigrant", "zero", "settled", "mixed", "missing_unused"} {
			f, id := legacySettlementTestIntent(t, ctx)
			ids := []server.Id{f.balanceId}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000,end_time=$2 WHERE balance_id=$1`, f.balanceId, server.NowUtc().Add(time.Hour)))
				if mode == "warm" || mode == "cold" {
					return
				}
				other := server.NewId()
				if mode != "missing_unused" {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
                    VALUES($1,$2,$3,$4,1000,1000,2000)`, other, f.sourceNetworkId, server.NowUtc(), server.NowUtc().Add(2*time.Hour)))
					ids = append(ids, other)
				}
				amount := 50
				if mode == "zero" {
					amount = 0
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,settled,redis_reserved)
                    VALUES($1,$2,$3,$4,$5)`, id, other, amount, mode == "settled", mode == "mixed"))
			})
			refreshNetEscrow(ctx, ids)
			before := openEscrowReservedForBalances(ctx, ids)
			if mode == "cold" {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=ANY($1)`, ids))
				})
			}
			trace := &legacyGrantOwnerDiagnosticTx{afterGrant: func() {}}
			var posts []func() any
			server.Tx(ctx, func(tx server.PgTx) {
				trace.PgTx = tx
				var completed, busy bool
				var err error
				posts, completed, busy, _, err = flushLegacySettlementInTx(ctx, trace, id)
				server.Raise(err)
				if !completed || busy {
					t.Fatal(mode, "did not complete")
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			wantFallback := mode == "zero" || mode == "settled" || mode == "mixed"
			metadataReads := 0
			for _, statement := range trace.statements {
				if strings.Contains(statement.sql, "SELECT outcome IS NOT NULL") {
					metadataReads++
				}
			}
			if (metadataReads == 1) != wantFallback || metadataReads > 1 {
				t.Fatalf("%s metadata ownership rereads=%d, fallback=%t", mode, metadataReads, wantFallback)
			}
			after := openEscrowReservedForBalances(ctx, ids)
			if after[f.balanceId].reserved != 0 || after[f.balanceId].revision != before[f.balanceId].revision+2 {
				t.Fatal(mode, "did not preserve both exact transitions", before[f.balanceId], after[f.balanceId])
			}
			cache, present := settlementCacheSnapshot(ctx, ids)[f.balanceId]
			if mode == "cold" {
				if present {
					t.Fatal("cold inline owner invented a cache before committed mirror work")
				}
			} else if !present || cache.revision != after[f.balanceId].revision || cache.reserved != 0 {
				t.Fatal(mode, "warm snapshot lost its exact second transition", cache, after[f.balanceId])
			}
			if mode == "multigrant" {
				other := ids[1]
				if after[other].reserved != 0 || after[other].revision != before[other].revision+2 || settlementCacheSnapshot(ctx, ids)[other].revision != after[other].revision {
					t.Fatal("unused second grant lost its full reservation release")
				}
			}
			requireLegacyOwnedMetadataFinancialState(t, ctx, f, id, ids)
			server.RunPosts(ctx, posts...)
			server.RunPosts(ctx, posts...)
			if mode == "cold" {
				if len(settlementCacheSnapshot(ctx, ids)) != 0 {
					t.Fatal("cold foreground posts performed the deferred census")
				}
				// A durable owner, rather than a replayed foreground callback,
				// repairs the cold mirror. Its replay stays absolute.
				legacyMirrorTestRun(t, ctx, f.balanceId)
				legacyMirrorTestRun(t, ctx, f.balanceId)
			}
			for _, balanceId := range ids {
				requireLegacyOwnedMetadataRedis(t, ctx, balanceId, 0)
			}
			completed, _, _, err := flushLegacySettlement(ctx, id)
			if err != nil || completed {
				t.Fatal(mode, "replay reclaimed financial settlement", err)
			}
			requireLegacyOwnedMetadataFinancialState(t, ctx, f, id, ids)
			t.Logf("mode=%s fallback=%t owned_sql_calls=%d", mode, wantFallback, len(trace.statements))
		}
	})
}

// An unfenced writer on another contract invalidates the captured amount.
// Neither inline publication may borrow its new revision to rescue stale data.
func TestLegacySettlementOwnedMetadataRejectsChangedRevision(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		neighbor, posts := createNetEscrowOrderingTestContract(ctx, f, 5)
		server.RunPosts(ctx, posts...)
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		before := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]
		mutated := false
		server.Tx(ctx, func(tx server.PgTx) {
			wrapped := &settlementCacheMutationTx{PgTx: tx, beforeClaim: func() {
				server.Tx(ctx, func(other server.PgTx) {
					server.RaisePgResult(other.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=9 WHERE contract_id=$1`, neighbor.ContractId))
				}, server.TxReadCommitted, server.OptNoRetry())
				mutated = true
			}}
			var completed, busy bool
			var err error
			posts, completed, busy, _, err = flushLegacySettlementInTx(ctx, wrapped, id)
			server.Raise(err)
			if !completed || busy || !mutated {
				t.Fatal("interleaving did not complete")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if _, present := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]; present {
			t.Fatal("inline metadata published a stale amount under a newer revision")
		}
		after := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId]
		if after.reserved != 9 || after.revision != before.revision+3 {
			t.Fatal("interleaved authority differs", before, after)
		}
		server.RunPosts(ctx, posts...)
		server.RunPosts(ctx, posts...)
		if len(settlementCacheSnapshot(ctx, []server.Id{f.balanceId})) != 0 {
			t.Fatal("invalidated foreground posts performed the deferred census")
		}
		legacyMirrorTestRun(t, ctx, f.balanceId)
		legacyMirrorTestRun(t, ctx, f.balanceId)
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 9)
		if cache := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]; cache.revision != after.revision || cache.reserved != 9 {
			t.Fatal("committed mirror did not repair the invalidated cache", cache, after)
		}
	})
}

// Rollback abandons both inline predictions; an acknowledged or lost-reply
// commit keeps all financial and metadata writes without relying on its posts.
func TestLegacySettlementOwnedMetadataRollbackAndLostReply(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		ids := []server.Id{f.balanceId}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		refreshNetEscrow(ctx, ids)
		before := settlementCacheSnapshot(ctx, ids)[f.balanceId]
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.Background())
		abandoned, completed, busy, _, err := flushLegacySettlementInTx(ctx, tx, id)
		server.Raise(err)
		if !completed || busy {
			t.Fatal("rollback owner failed")
		}
		server.Raise(tx.Rollback(ctx))
		server.RunPosts(ctx, abandoned...)
		if got := settlementCacheSnapshot(ctx, ids)[f.balanceId]; got.revision != before.revision || got.reserved != 100 {
			t.Fatal("abandoned prediction escaped", before, got)
		}
		requireLegacyProviderDurability(t, ctx, f, id, 0)
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 100)
		tx, err = conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.Background())
		posts, completed, busy, _, err := flushLegacySettlementInTx(ctx, tx, id)
		server.Raise(err)
		if !completed || busy {
			t.Fatal("commit owner failed")
		}
		server.Raise(tx.Commit(ctx))
		requireLegacyOwnedMetadataFinancialState(t, ctx, f, id, ids)
		if got := settlementCacheSnapshot(ctx, ids)[f.balanceId]; got.revision != before.revision+2 || got.reserved != 0 {
			t.Fatal("committed financial metadata depends on a post", before, got)
		}
		completed, _, _, err = flushLegacySettlement(ctx, id)
		if err != nil || completed {
			t.Fatal("lost reply repeated settlement", completed, err)
		}
		server.RunPosts(ctx, abandoned...)
		server.RunPosts(ctx, posts...)
		requireLegacyOwnedMetadataFinancialState(t, ctx, f, id, ids)
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 0)
	})
}

// Check actual escrow metadata, payer consumption and durable monetary payout.
func requireLegacyOwnedMetadataFinancialState(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, ids []server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var credit ByteCount
		var settled, terminal, pending bool
		server.Raise(conn.QueryRow(ctx, `SELECT
            (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
            (SELECT bool_and(settled AND settle_time IS NOT NULL AND payout_byte_count=CASE WHEN balance_id=$2 THEN 11 ELSE 0 END) FROM transfer_escrow WHERE contract_id=$1 AND balance_id=ANY($3)),
            (SELECT outcome='settled' FROM transfer_contract WHERE contract_id=$1),
            EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)`, id, f.balanceId, ids).Scan(&credit, &settled, &terminal, &pending))
		if credit != 989 || !settled || !terminal || pending {
			t.Fatal("inline financial metadata differs", credit, settled, terminal, pending)
		}
	})
	requireLegacyProviderDurability(t, ctx, f, id, 11, 11)
}

// A Redis read failure is never accepted as a zero reservation.
func requireLegacyOwnedMetadataRedis(t testing.TB, ctx context.Context, id server.Id, want ByteCount) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		got, err := r.Get(ctx, netEscrowKey(id)).Int64()
		if want == 0 && errors.Is(err, redis.Nil) {
			return
		}
		if err != nil || got != want {
			t.Fatalf("explicit Redis reservation read=%d err=%v, want=%d", got, err, want)
		}
	})
}
