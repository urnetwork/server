package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Counts the exact census statements one owned settlement sends, alone or
// queued in a batch, and runs beforeCensus first, while that settlement still
// holds its financial locks. Settlement admits transaction ownership only on
// the server's own transaction (ddad361c), so this binds to its connection
// rather than wrapping it.
func coldSettlementCensusHook(exact *atomic.Int64, beforeCensus func()) *legacyStatementHook {
	return &legacyStatementHook{before: func(_ context.Context, sql string, queued []string) {
		if queued == nil {
			queued = []string{sql}
		}
		for _, statement := range queued {
			if statement == netEscrowReservationPageSQL {
				exact.Add(1)
				if beforeCensus != nil {
					beforeCensus()
				}
			}
		}
	}}
}

// A cold cache is optional settlement bookkeeping: the exact locked escrow and
// both close reports already authorize the debit. Prove the actual cold census
// seam owns a contested balance on the baseline, then require no such seam.
// After commit the mirror refresh censuses a grant only within
// netEscrowRefreshCensusRowBound live legacy rows (086180a7, 1d9b237d): within
// it the refresh publishes the exact surviving reservation and warms the cache;
// past it the refresh defers, leaving the published mirror and the cold or
// stale cache as they were.
func TestNetEscrowColdSettlementKeepsCensusOutsideFinancialLocks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		for _, test := range []struct {
			history int
			stale   bool
		}{
			{history: 10001},
			{history: 10001, stale: true},
			// The refresh follows its close's metadata, so it counts only the
			// seeded history and stays within the bound.
			{history: netEscrowRefreshCensusRowBound - 1},
			{history: netEscrowRefreshCensusRowBound - 1, stale: true},
		} {
			refreshed := test.history <= netEscrowRefreshCensusRowBound
			f := seedAdmissionCacheHistory(t, ctx, test.history, 100)
			contract, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
			settlementCacheCloseReports(ctx, contract.ContractId)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=8 WHERE contract_id=$1`, contract.ContractId))
				if test.stale {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=balance_byte_count+4 WHERE balance_id=$1 AND contract_id=md5($1::uuid::text||'-cache-contract-1')::uuid`, f.balanceId))
				} else {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
				}
			})
			mirrorBefore := Testing_NetEscrowByteCount(ctx, f.balanceId)
			var reads atomic.Int64
			blockedDuringCensus := 0
			probeBalance := func(wantBlocked bool) {
				conn := acquireContractLifecycleTestConnection(t, ctx)
				defer conn.Release()
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(context.Background())
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='75ms'`))
				_, err = tx.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId)
				var pgErr *pgconn.PgError
				blocked := errors.As(err, &pgErr) && pgErr.Code == "55P03"
				if blocked != wantBlocked || (!blocked && err != nil) {
					t.Fatalf("balance contention control: blocked=%t want=%t err=%v", blocked, wantBlocked, err)
				}
				if blocked {
					blockedDuringCensus++
				}
			}
			// Start observation before the probe borrows any connection.
			stopHooks := startLegacyStatementHooks(t, ctx)
			defer stopHooks()
			var posts []func() any
			server.Tx(ctx, func(tx server.PgTx) {
				var closed bool
				var err error
				hooked, unhook := withLegacyStatementHooks(ctx, tx, coldSettlementCensusHook(&reads, func() { probeBalance(true) }))
				defer unhook()
				posts, closed, err = settleEscrowInTx(hooked, tx, contract.ContractId, ContractOutcomeSettled)
				server.Raise(err)
				if !closed {
					t.Fatal("cold settlement did not claim outcome")
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			stopHooks()
			probeBalance(false)
			var credit ByteCount
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&credit))
			})
			// The seeded credit less the eight bytes both parties reported.
			if wantCredit := ByteCount(test.history + 100 - 8); credit != wantCredit {
				t.Fatalf("history=%d cold settlement credit=%d want%d", test.history, credit, wantCredit)
			}
			t.Logf("history=%d stale=%t financial_census=%d observed_balance_waits_at_census=%d", test.history, test.stale, reads.Load(), blockedDuringCensus)
			if reads.Load() != 0 {
				t.Errorf("history=%d stale=%t cold settlement performed %d exact history scans while holding financial locks", test.history, test.stale, reads.Load())
			}
			server.RunPosts(ctx, posts...)
			want := ByteCount(test.history)
			if test.stale {
				want += 4
			}
			if !refreshed {
				want = mirrorBefore
			}
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != want {
				t.Fatalf("history=%d stale=%t mirror=%d want%d", test.history, test.stale, got, want)
			}
			got, ok := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]
			if refreshed && (!ok || got.reserved != want) {
				t.Fatalf("history=%d stale=%t committed mirror did not leave exact current reservation authority", test.history, test.stale)
			}
			if !refreshed && ok {
				t.Fatalf("history=%d stale=%t deferred refresh published cache authority", test.history, test.stale)
			}
			server.RunPosts(ctx, posts...)
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != want {
				t.Fatalf("history=%d stale=%t replay changed surviving reservations", test.history, test.stale)
			}
		}
	})
}

func TestNetEscrowColdSettlementRollbackLostPostsAndReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 3, 100)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
		settlementCacheCloseReports(ctx, contract.ContractId)
		ids := []server.Id{f.balanceId}
		cold := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
			})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=8 WHERE contract_id=$1`, contract.ContractId))
		})
		cold()
		var abandoned []func() any
		var closed bool
		var err error
		rollback := errors.New("synthetic cold settlement rollback")
		func() {
			defer func() {
				if failure := recover(); failure != rollback {
					t.Fatalf("cold settlement rollback error_type=%T, want exact synthetic rollback", failure)
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				abandoned, closed, err = settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
				server.Raise(err)
				if !closed || len(abandoned) == 0 || len(readCachedNetEscrowSnapshots(ctx, tx, ids)) != 0 {
					t.Fatal("cold transaction invented a cached prediction")
				}
				panic(rollback)
			}, server.TxReadCommitted, server.OptNoRetry())
			t.Fatal("synthetic rollback unexpectedly committed")
		}()
		server.RunPosts(ctx, abandoned...)
		if got := settlementCacheSnapshot(ctx, ids)[f.balanceId].reserved; got != 20 {
			t.Fatal("abandoned post released uncommitted reservation", got)
		}
		var credit ByteCount
		var outcome *ContractOutcome
		check := func(want ByteCount, terminal bool) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&credit))
				server.Raise(conn.QueryRow(ctx, `SELECT outcome FROM transfer_contract WHERE contract_id=$1`, contract.ContractId).Scan(&outcome))
			})
			if credit != want || (outcome != nil) != terminal {
				t.Fatalf("credit=%d terminal=%t want%d/%t", credit, outcome != nil, want, terminal)
			}
		}
		check(103, false)
		cold()
		var committed []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			committed, closed, err = settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
		}, server.TxReadCommitted, server.OptNoRetry())
		check(95, true)
		if len(settlementCacheSnapshot(ctx, ids)) != 0 {
			t.Fatal("cold commit fabricated cache authority")
		}
		// Drop every post and the commit response. Admission still obtains its
		// exact committed authority; a retry cannot debit or release twice.
		if got, scans := lockedAdmissionCacheTestRead(ctx, f); got.reserved != 3 || scans != 1 {
			t.Fatalf("lost-post fallback reserved=%d scans=%d want3/1", got.reserved, scans)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			posts, again, e := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(e)
			if again || len(posts) != 0 {
				t.Fatal("ambiguous commit replay reclaimed settlement")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		server.RunPosts(ctx, committed...)
		server.RunPosts(ctx, abandoned...)
		check(95, true)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 3 {
			t.Fatal("replay changed surviving reservation", got)
		}
	})
}

func TestNetEscrowCommittedCacheLegacyInvalidationAndOlderPublication(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 3, 100)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
		ids := []server.Id{f.balanceId}
		old := openEscrowReservedForBalances(ctx, ids)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=21 WHERE contract_id=$1`, contract.ContractId))
		})
		cacheCommittedNetEscrowSnapshots(ctx, old)
		if len(settlementCacheSnapshot(ctx, ids)) != 0 {
			t.Fatal("stale committed census borrowed a later legacy revision")
		}
		fresh := readMirrorNetEscrowSnapshots(ctx, ids)
		if fresh[f.balanceId].reserved != 24 {
			t.Fatal("legacy exact fallback lost reservation")
		}
		cacheCommittedNetEscrowSnapshots(ctx, old)
		if got := settlementCacheSnapshot(ctx, ids)[f.balanceId]; got.revision != fresh[f.balanceId].revision || got.reserved != 24 {
			t.Fatal("older delayed publication replaced newer durable cache")
		}
		// A legacy write can be uncommitted when publication observes the old
		// revision. Once that write commits, the cache must become a miss.
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.Background())
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=25 WHERE contract_id=$1`, contract.ContractId))
		cacheCommittedNetEscrowSnapshots(ctx, fresh)
		server.Raise(tx.Commit(ctx))
		if len(settlementCacheSnapshot(ctx, ids)) != 0 {
			t.Fatal("uncommitted legacy mutation remained falsely cached after commit")
		}
		if got := readMirrorNetEscrowSnapshots(ctx, ids)[f.balanceId].reserved; got != 28 {
			t.Fatal("second legacy fallback", got)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId))
		})
		cacheCommittedNetEscrowSnapshots(ctx, fresh)
		if len(settlementCacheSnapshot(ctx, ids)) != 0 {
			t.Fatal("deleted balance acquired cache authority")
		}
	})
}

func TestNetEscrowColdSettlementMirrorSurvivesMetadataFailure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 3, 100)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 17)
		settlementCacheCloseReports(ctx, contract.ContractId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
		})
		var posts []func() any
		// The settlement owner admits transaction ownership only on the
		// server's own read-committed no-retry transaction (ddad361c).
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			posts, _, err = settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
		}, server.TxReadCommitted, server.OptNoRetry())
		// The owning metadata transaction fails and rolls back, rather than
		// stranding the committed mirror behind a lost sibling callback.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION reject_test_settled_metadata() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.settled AND NOT OLD.settled THEN RAISE EXCEPTION 'injected metadata failure'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER reject_test_settled_metadata BEFORE UPDATE ON transfer_escrow FOR EACH ROW EXECUTE FUNCTION reject_test_settled_metadata()`))
		})
		server.RunPosts(ctx, posts...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 3 {
			t.Fatal("metadata error stranded mirror", got)
		}
		if got := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 3 {
			t.Fatal("metadata error invented cache", got)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER reject_test_settled_metadata ON transfer_escrow; DROP FUNCTION reject_test_settled_metadata()`))
		})
		server.RunPosts(ctx, posts...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 3 {
			t.Fatal("metadata repair replay changed reservation", got)
		}
	})
}
