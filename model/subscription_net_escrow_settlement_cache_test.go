package model

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
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

// Each hook runs after the snapshot read and before its revision transition:
// the claim hook before the outcome claim, the metadata hook before the first
// batch. A second real connection commits a legacy write while settlement is
// open. Each runs once.
type settlementCacheMutation struct {
	beforeClaim    func()
	beforeMetadata func()
}

// Runs the matching callback before the real statement is sent.
func (self *settlementCacheMutation) hook() *legacyStatementHook {
	return &legacyStatementHook{before: func(_ context.Context, sql string, queued []string) {
		if queued == nil && self.beforeClaim != nil && strings.Contains(sql, "UPDATE transfer_contract") && strings.Contains(sql, "RETURNING close_time") {
			hook := self.beforeClaim
			self.beforeClaim = nil
			hook()
		}
		if queued != nil && self.beforeMetadata != nil {
			hook := self.beforeMetadata
			self.beforeMetadata = nil
			hook()
		}
	}}
}

// Counts the exact reservation census statements a settlement's own owned
// transaction sends, alone or queued in a batch.
func settlementCacheCensusCounter(count *atomic.Int64) *legacyStatementHook {
	return &legacyStatementHook{before: func(_ context.Context, sql string, queued []string) {
		if queued == nil {
			queued = []string{sql}
		}
		for _, statement := range queued {
			if statement == netEscrowReservationPageSQL {
				count.Add(1)
			}
		}
	}}
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
			stopHooks := startLegacyStatementHooks(t, ctx)
			defer stopHooks()
			server.Tx(ctx, func(tx server.PgTx) {
				mutation := &settlementCacheMutation{}
				if stage == "outcome" {
					mutation.beforeClaim = legacy
				}
				var err error
				var closed bool
				hooked, unhook := withLegacyStatementHooks(ctx, tx, mutation.hook())
				defer unhook()
				posts, closed, err = settleEscrowInTx(hooked, tx, contract.ContractId, ContractOutcomeSettled)
				server.Raise(err)
				if !closed {
					t.Fatal("outcome did not commit")
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			if stage == "metadata" {
				server.Tx(ctx, func(tx server.PgTx) {
					mutation := &settlementCacheMutation{beforeMetadata: legacy}
					hooked, unhook := withLegacyStatementHooks(ctx, tx, mutation.hook())
					defer unhook()
					settleEscrowMetadataInTx(hooked, tx, contract.ContractId, server.NowUtc(), map[server.Id]sweepPayout{f.balanceId: {escrowBalanceByteCount: 17}})
				}, server.TxReadCommitted, server.OptNoRetry())
			}
			stopHooks()
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
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		server.Tx(ctx, func(tx server.PgTx) {
			claim := &settlementCacheMutation{beforeClaim: legacy}
			hooked, unhook := withLegacyStatementHooks(ctx, tx, claim.hook())
			defer unhook()
			_, closed, err := settleEscrowInTx(hooked, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("outcome not claimed")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		server.Tx(ctx, func(tx server.PgTx) {
			metadata := &settlementCacheMutation{beforeMetadata: legacy}
			hooked, unhook := withLegacyStatementHooks(ctx, tx, metadata.hook())
			defer unhook()
			settleEscrowMetadataInTx(hooked, tx, contract.ContractId, server.NowUtc(), map[server.Id]sweepPayout{f.balanceId: {escrowBalanceByteCount: 17}})
		}, server.TxReadCommitted, server.OptNoRetry())
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
		// Each owner gets a real owned server transaction whose end the test chooses.
		tx, finish := beginRareGrantTestTx(t, ctx)
		defer finish(false)
		abandoned, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
		server.Raise(err)
		if !closed || readCachedNetEscrowSnapshots(ctx, tx, ids)[f.balanceId].reserved != 3 {
			t.Fatal("uncommitted settlement did not publish its exact local delta")
		}
		// A concurrent reader still sees the complete previous authority.
		if got := settlementCacheSnapshot(ctx, ids)[f.balanceId]; got.revision != before.revision || got.reserved != before.reserved {
			t.Fatal("uncommitted cache escaped")
		}
		server.Raise(finish(false))
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
		}, server.TxReadCommitted, server.OptNoRetry())
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
		}, server.TxReadCommitted, server.OptNoRetry())
		// A metadata transaction rollback must restore its preceding snapshot.
		tx, finish = beginRareGrantTestTx(t, ctx)
		defer finish(false)
		settleEscrowMetadataInTx(ctx, tx, contract.ContractId, server.NowUtc(), map[server.Id]sweepPayout{f.balanceId: {escrowBalanceByteCount: 17}})
		server.Raise(finish(false))
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
// A cold mirror refresh censuses a grant only within netEscrowRefreshCensusRowBound
// live legacy rows (086180a7, 1d9b237d): within it the first close reloads the
// cache and the rest reuse it; past it every close defers and leaves the
// published mirror as it was, as TestNetEscrowRefreshCacheConcurrentMirrorCost
// expects of the same refresh.
func TestNetEscrowSettlementCacheTwentyCloseCost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		const closeCount = 20
		for _, test := range []struct {
			name    string
			history int
			cold    bool
			// Mirror refresh snapshots across the twenty closes.
			reloaded float64
			reused   float64
			deferred float64
		}{
			{name: "warm", history: 10001, reused: closeCount},
			// The first cold refresh also counts the other staged contracts'
			// live rows; this history keeps that census within the bound.
			{name: "cold_within_bound", history: netEscrowRefreshCensusRowBound - closeCount, cold: true, reloaded: 1, reused: closeCount - 1},
			{name: "cold_over_bound", history: 10001, cold: true, deferred: closeCount},
		} {
			f := seedAdmissionCacheHistory(t, ctx, test.history, 100)
			contracts := make([]server.Id, closeCount)
			for i := range contracts {
				contract, _ := createNetEscrowOrderingTestContract(ctx, f, 1)
				contracts[i] = contract.ContractId
				settlementCacheCloseReports(ctx, contract.ContractId)
			}
			if test.cold {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
				})
			}
			beforeReload := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded"))
			beforeReuse := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reused"))
			beforeDeferred := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("deferred"))
			mirrorBefore := Testing_NetEscrowByteCount(ctx, f.balanceId)
			var mainReads atomic.Int64
			census := settlementCacheCensusCounter(&mainReads)
			stopHooks := startLegacyStatementHooks(t, ctx)
			defer stopHooks()
			began := time.Now()
			for _, id := range contracts {
				var posts []func() any
				server.Tx(ctx, func(tx server.PgTx) {
					var err error
					var closed bool
					hooked, unhook := withLegacyStatementHooks(ctx, tx, census)
					defer unhook()
					posts, closed, err = settleEscrowInTx(hooked, tx, id, ContractOutcomeSettled)
					server.Raise(err)
					if !closed {
						t.Fatal("staged contract was not claimed")
					}
				}, server.TxReadCommitted, server.OptNoRetry())
				server.RunPosts(ctx, posts...)
			}
			elapsed := time.Since(began)
			stopHooks()
			reloaded := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reloaded")) - beforeReload
			reused := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("reused")) - beforeReuse
			deferred := testutil.ToFloat64(netEscrowRefreshSnapshots.WithLabelValues("deferred")) - beforeDeferred
			// A deferred refresh leaves the published mirror as it was.
			wantMirror := ByteCount(test.history)
			if 0 < test.deferred {
				wantMirror = mirrorBefore
			}
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != wantMirror {
				t.Fatalf("%s surviving mirrored reservation=%d want%d", test.name, got, wantMirror)
			}
			t.Logf("%s closes20 no_overtaking_admission main_census_statements=%d mirror_census_statements=%v mirror_reused=%v mirror_deferred=%v elapsed=%s", test.name, mainReads.Load(), reloaded, reused, deferred, elapsed)
			if mainReads.Load() != 0 || reloaded != test.reloaded || reused != test.reused || deferred != test.deferred {
				t.Errorf("%s main_exact=%d mirror_exact=%v reused=%v deferred=%v want0/%v/%v/%v", test.name, mainReads.Load(), reloaded, reused, deferred, test.reloaded, test.reused, test.deferred)
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
