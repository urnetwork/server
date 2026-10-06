package model

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
)

func settlementCacheSnapshot(ctx context.Context, ids []server.Id) map[server.Id]netEscrowSnapshot {
	var result map[server.Id]netEscrowSnapshot
	server.Db(ctx, func(conn server.PgConn) { result = readCachedNetEscrowSnapshots(ctx, conn, ids) })
	return result
}

func settlementCacheCloseReports(ctx context.Context, contractId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
		 VALUES($1,'source',0,$2,false),($1,'destination',0,$2,false)`, contractId, server.NowUtc()))
	})
}

// Each hook runs after the snapshot read and before its revision transition.
// A second real connection commits a legacy write while settlement is open.
type settlementCacheMutationTx struct {
	server.PgTx
	beforeClaim    func()
	beforeMetadata func()
}

func (tx *settlementCacheMutationTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "provider_usage = $4") && tx.beforeClaim != nil {
		hook := tx.beforeClaim
		tx.beforeClaim = nil
		hook()
	}
	return tx.PgTx.Exec(ctx, sql, args...)
}

func (tx *settlementCacheMutationTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if tx.beforeMetadata != nil {
		hook := tx.beforeMetadata
		tx.beforeMetadata = nil
		hook()
	}
	return tx.PgTx.SendBatch(ctx, batch)
}

func TestNetEscrowSettlementCacheLegacyMutationInvalidatesPrediction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		for _, stage := range []string{"outcome", "metadata"} {
			f := seedAdmissionCacheHistory(t, ctx, 3, 100)
			contract, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
			settlementCacheCloseReports(ctx, contract.ContractId)
			mutated := false
			legacy := func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=balance_byte_count+4
					 WHERE balance_id=$1 AND contract_id=md5($1::uuid::text||'-cache-contract-1')::uuid`, f.balanceId))
				}, server.TxReadCommitted, server.OptNoRetry())
				mutated = true
			}
			var posts []func() any
			server.Tx(ctx, func(tx server.PgTx) {
				wrapped := &settlementCacheMutationTx{PgTx: tx}
				if stage == "outcome" {
					wrapped.beforeClaim = legacy
				}
				var err error
				var closed bool
				posts, closed, err = settleEscrowInTx(ctx, wrapped, contract.ContractId, ContractOutcomeSettled)
				server.Raise(err)
				if !closed {
					t.Fatal("outcome did not commit")
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			if stage == "metadata" {
				server.Tx(ctx, func(tx server.PgTx) {
					wrapped := &settlementCacheMutationTx{PgTx: tx, beforeMetadata: legacy}
					settleEscrowMetadataInTx(ctx, wrapped, contract.ContractId, server.NowUtc(), map[server.Id]sweepPayout{f.balanceId: {escrowBalanceByteCount: 17}})
				}, server.TxReadCommitted, server.OptNoRetry())
			}
			if !mutated {
				t.Fatal("interleaving hook did not run")
			}
			if _, ok := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]; ok {
				t.Fatalf("%s borrowed an unrelated revision for its old amount", stage)
			}
			before := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded"))
			server.RunPosts(ctx, posts...)
			if n := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded")) - before; n != 1 {
				t.Fatalf("%s mirror did not use exact fallback: %v", stage, n)
			}
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 7 {
				t.Fatalf("%s mirror=%d want7", stage, got)
			}
			// This legacy hook invalidates a previously warm settlement. Its
			// independent metadata/mirror posts may leave either a current warmup
			// or a revision miss; admission must be exact in both schedules.
			if got, n := lockedAdmissionCacheTestRead(ctx, f); got.reserved != 7 || n > 1 {
				t.Fatalf("%s next admission cache amount=%d census=%d want7/at-most1", stage, got.reserved, n)
			}
			server.RunPosts(ctx, posts...)
			if got := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 7 {
				t.Fatalf("%s replay changed newly repaired cache: %d", stage, got)
			}
		}
	})
}

// Holding a tuple is necessary: its positive contribution must not change
// between the snapshot read and either revision transition.
func TestNetEscrowSettlementCacheLocksExactContribution(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 3, 100)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
		settlementCacheCloseReports(ctx, contract.ContractId)
		blocked := 0
		legacy := func() {
			conn := acquireContractLifecycleTestConnection(t, ctx)
			defer conn.Release()
			tx, err := conn.Begin(ctx)
			server.Raise(err)
			defer tx.Rollback(context.Background())
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`))
			_, err = tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=99 WHERE contract_id=$1 AND balance_id=$2`, contract.ContractId, f.balanceId)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("target contribution was mutable under snapshot owner: %v", err)
			}
			blocked++
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, closed, err := settleEscrowInTx(ctx, &settlementCacheMutationTx{PgTx: tx, beforeClaim: legacy}, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("outcome not claimed")
			}
		}, server.TxReadCommitted)
		server.Tx(ctx, func(tx server.PgTx) {
			settleEscrowMetadataInTx(ctx, &settlementCacheMutationTx{PgTx: tx, beforeMetadata: legacy}, contract.ContractId, server.NowUtc(), map[server.Id]sweepPayout{f.balanceId: {escrowBalanceByteCount: 17}})
		}, server.TxReadCommitted)
		if got := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 3 || blocked != 2 {
			t.Fatalf("reserved=%d blocked_mutations=%d want3/2", got, blocked)
		}
	})
}

func TestNetEscrowSettlementCacheRollbackAndAmbiguousCommit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 3, 100)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
		settlementCacheCloseReports(ctx, contract.ContractId)
		ids := []server.Id{f.balanceId}
		before := settlementCacheSnapshot(ctx, ids)[f.balanceId]
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.Background())
		abandoned, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
		server.Raise(err)
		if !closed || readCachedNetEscrowSnapshots(ctx, tx, ids)[f.balanceId].reserved != 3 {
			t.Fatal("uncommitted settlement did not publish its exact local delta")
		}
		// A concurrent reader still sees the complete previous authority.
		if got := settlementCacheSnapshot(ctx, ids)[f.balanceId]; got.revision != before.revision || got.reserved != before.reserved {
			t.Fatal("uncommitted cache escaped")
		}
		server.Raise(tx.Rollback(ctx))
		server.RunPosts(ctx, abandoned...)
		var settled bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT settled FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2`, contract.ContractId, f.balanceId).Scan(&settled))
		})
		if got := settlementCacheSnapshot(ctx, ids)[f.balanceId]; settled || got.revision != before.revision || got.reserved != 20 {
			t.Fatal("abandoned posts changed open escrow or committed cache")
		}
		// Commit while deliberately dropping the response and all posts. A caller
		// retry cannot claim twice, and cache correctness needs no replay.
		var committedPosts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			committedPosts, closed, err = settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
		}, server.TxReadCommitted)
		committed := settlementCacheSnapshot(ctx, ids)[f.balanceId]
		if !closed || committed.reserved != 3 || committed.revision != before.revision+1 {
			t.Fatal("outcome commit depends on its metadata replay")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			posts, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if closed || len(posts) != 0 {
				t.Fatal("ambiguous-commit retry reclaimed settlement")
			}
		}, server.TxReadCommitted)
		// A metadata transaction rollback must restore its preceding snapshot.
		tx, err = conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.Background())
		settleEscrowMetadataInTx(ctx, tx, contract.ContractId, server.NowUtc(), map[server.Id]sweepPayout{f.balanceId: {escrowBalanceByteCount: 17}})
		server.Raise(tx.Rollback(ctx))
		if got := settlementCacheSnapshot(ctx, ids)[f.balanceId]; got.revision != committed.revision || got.reserved != 3 {
			t.Fatal("metadata rollback lost preceding exact cache")
		}
		server.RunPosts(ctx, committedPosts...)
		server.RunPosts(ctx, abandoned...)
		if got := settlementCacheSnapshot(ctx, ids)[f.balanceId]; got.revision != committed.revision+1 || got.reserved != 3 {
			t.Fatal("delayed/replayed metadata did not use fresh committed authority")
		}
	})
}

func TestNetEscrowSettlementCacheZeroSettledAndMissingRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		for _, change := range []string{"zero", "settled", "missing_escrow", "missing_balance", "missing_contract"} {
			f := seedAdmissionCacheHistory(t, ctx, 3, 100)
			contract, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
			server.Tx(ctx, func(tx server.PgTx) {
				switch change {
				case "zero":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=0 WHERE contract_id=$1`, contract.ContractId))
				case "settled":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled=true WHERE contract_id=$1`, contract.ContractId))
				case "missing_escrow":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow WHERE contract_id=$1`, contract.ContractId))
				case "missing_balance":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId))
				case "missing_contract":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, contract.ContractId))
				}
			})
			if change == "missing_contract" {
				// A stale metadata callback cannot change orphaned escrow.
				server.Tx(ctx, func(tx server.PgTx) {
					settleEscrowMetadataInTx(ctx, tx, contract.ContractId, server.NowUtc(), map[server.Id]sweepPayout{f.balanceId: {escrowBalanceByteCount: 17}})
				})
				var settled bool
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT settled FROM transfer_escrow WHERE contract_id=$1`, contract.ContractId).Scan(&settled))
				})
				if settled {
					t.Fatal("missing contract metadata changed orphaned escrow")
				}
				continue
			}
			ids := []server.Id{f.balanceId}
			if change != "missing_balance" {
				lockedAdmissionCacheTestRead(ctx, f)
			}
			before, found := settlementCacheSnapshot(ctx, ids)[f.balanceId]
			posts := settleNetEscrowOrderingTestContract(ctx, contract.ContractId)
			server.RunPosts(ctx, posts...)
			server.RunPosts(ctx, posts...)
			after, ok := settlementCacheSnapshot(ctx, ids)[f.balanceId]
			if ok != found || (ok && (after.revision != before.revision || after.reserved != 3)) {
				t.Fatalf("%s invented a reservation/revision delta", change)
			}
			want := ByteCount(3)
			if change == "missing_balance" {
				want = 0
			}
			if exact := openEscrowReservedForBalances(ctx, ids)[f.balanceId].reserved; exact != want {
				t.Fatalf("%s exact reservation=%d want%d", change, exact, want)
			}
		}
	})
}

// Stage all contracts first: no overtaking admission can repair a settlement's
// cache. Count actual main-transaction census statements and one-balance mirror
// reloads, including the cold-cache cost that would otherwise be hidden.
func TestNetEscrowSettlementCacheTwentyCloseCost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		for _, cold := range []bool{false, true} {
			f := seedAdmissionCacheHistory(t, ctx, 10001, 100)
			contracts := make([]server.Id, 20)
			for i := range contracts {
				contract, _ := createNetEscrowOrderingTestContract(ctx, f, 1)
				contracts[i] = contract.ContractId
				settlementCacheCloseReports(ctx, contract.ContractId)
			}
			if cold {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
				})
			}
			beforeReload := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded"))
			beforeReuse := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reused"))
			var mainReads atomic.Int64
			began := time.Now()
			for _, id := range contracts {
				var posts []func() any
				server.Tx(ctx, func(tx server.PgTx) {
					var err error
					var closed bool
					posts, closed, err = settleEscrowInTx(ctx, admissionCacheTestTx{PgTx: tx, exact: &mainReads}, id, ContractOutcomeSettled)
					server.Raise(err)
					if !closed {
						t.Fatal("staged contract was not claimed")
					}
				}, server.TxReadCommitted)
				server.RunPosts(ctx, posts...)
			}
			elapsed := time.Since(began)
			reloaded := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded")) - beforeReload
			reused := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reused")) - beforeReuse
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 10001 {
				t.Fatalf("surviving mirrored reservation=%d want10001", got)
			}
			wantMirror := float64(0)
			if cold {
				wantMirror = 1
			}
			t.Logf("cold_cache=%t closes20 no_overtaking_admission main_census_statements=%d mirror_census_statements=%v mirror_reused=%v elapsed=%s", cold, mainReads.Load(), reloaded, reused, elapsed)
			if mainReads.Load() != 0 || reloaded != wantMirror || reused != 20-wantMirror {
				t.Errorf("cold=%t main_exact=%d mirror_exact=%v reused=%v want0/%v/%v", cold, mainReads.Load(), reloaded, reused, wantMirror, 20-wantMirror)
			}
		}
	})
}

// Outcome and metadata transitions each advance the durable revision. Both
// transitions must retain the exact surviving reservation without new creation.
func TestNetEscrowSettlementPreservesSnapshotAcrossPosts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 10001, 100)
		first, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
		_, _ = createNetEscrowOrderingTestContract(ctx, f, 23)
		ids := []server.Id{f.balanceId}
		before := settlementCacheSnapshot(ctx, ids)[f.balanceId]
		posts := settleNetEscrowOrderingTestContract(ctx, first.ContractId)
		after, ok := settlementCacheSnapshot(ctx, ids)[f.balanceId]
		if !ok || after.reserved != 10024 || after.revision != before.revision+1 {
			t.Fatalf("outcome lost exact snapshot: found=%t reserved=%d revision_delta=%d", ok, after.reserved, after.revision-before.revision)
		}
		server.RunPosts(ctx, posts...)
		done, ok := settlementCacheSnapshot(ctx, ids)[f.balanceId]
		if !ok || done.reserved != 10024 || done.revision != before.revision+2 {
			t.Fatalf("metadata lost exact snapshot: found=%t reserved=%d revision_delta=%d", ok, done.reserved, done.revision-before.revision)
		}
		server.RunPosts(ctx, posts...)
		replay, ok := settlementCacheSnapshot(ctx, ids)[f.balanceId]
		if !ok || replay.reserved != done.reserved || replay.revision != done.revision {
			t.Fatal("metadata replay changed exact reservation")
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 10024 {
			t.Fatalf("mirror=%d want10024", got)
		}
	})
}
