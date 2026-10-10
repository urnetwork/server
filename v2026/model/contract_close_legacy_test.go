// Legacy compatibility treats ambiguous increments as lower bounds without
// weakening identified report custody, finality, or transaction ownership.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Equal identified checkpoints are independent work; an identity stripped by
// another delivery route must not cause that same work to be counted again.
func TestContractCloseLegacyMixedCheckpointOrders(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		type delivery struct {
			identity int
			bytes    ByteCount
			added    ByteCount
		}
		for _, deliveries := range [][]delivery{
			{{bytes: 20, added: 20}, {bytes: 20}, {identity: 1, bytes: 20}, {identity: 1, bytes: 20}, {identity: 2, bytes: 20, added: 20}},
			{{identity: 1, bytes: 20, added: 20}, {bytes: 20}, {identity: 1, bytes: 20}, {identity: 2, bytes: 20, added: 20}, {bytes: 20}},
			{{bytes: 30, added: 30}, {bytes: 20}, {identity: 1, bytes: 20}, {identity: 2, bytes: 20, added: 10}, {bytes: 35}, {bytes: 50, added: 10}},
		} {
			f := newCloseReportFixture(t)
			identities := map[int]server.Id{}
			var total ByteCount
			for _, delivery := range deliveries {
				var added ByteCount
				var err error
				if delivery.identity == 0 {
					added, err = CloseContractUsage(f.ctx, f.contractId, f.sourceId, delivery.bytes, true)
				} else {
					if identities[delivery.identity] == (server.Id{}) {
						identities[delivery.identity] = server.NewId()
					}
					added, err = CloseContractWithReportUsage(f.ctx, ContractCloseReport{
						ReportId: identities[delivery.identity], ContractId: f.contractId, ClientId: f.sourceId,
						AckedByteCount: delivery.bytes, Checkpoint: true})
				}
				if err != nil || added != delivery.added {
					t.Fatalf("mixed delivery %+v added=%d err=%v", delivery, added, err)
				}
				total += delivery.added
				assertCloseReportCounts(t, f.ctx, f.contractId, len(identities), total)
			}
			for index, want := range []ByteCount{7, 0} {
				added, err := CloseContractUsage(f.ctx, f.contractId, f.sourceId, 7, false)
				if err != nil || added != want {
					t.Fatal("legacy final/retry changed its committed delta", index, added, err)
				}
			}
			// Both receipt owners must refuse new work after party finality.
			if added, err := CloseContractUsage(f.ctx, f.contractId, f.sourceId, 1000, true); added != 0 || err != nil {
				t.Fatal("late legacy checkpoint changed final usage", added, err)
			}
			if applied, err := CloseContractReport(f.ctx, f.contractId, f.sourceId, 1000, true, server.NewId()); applied || err != nil {
				t.Fatal("late rolling checkpoint changed final usage", applied, err)
			}
			if applied, err := CloseContractWithReport(f.ctx, f.report()); applied || !errors.Is(err, ErrContractCloseReportClosed) {
				t.Fatal("late original gained admission", applied, err)
			}
			assertCloseReportCounts(t, f.ctx, f.contractId, len(identities), total+7)
			requireCloseReportState(t, f.ctx, f.contractId, len(identities), int(total+7))
		}
	})
}

// Upgrading must preserve the existing aggregate, including history that has
// no receipts. New retries cannot add to that unknown historical floor.
func TestContractCloseLegacyUpgradeRetainsExistingFloor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',80,true)`, f.contractId))
		})
		if added, err := CloseContractUsage(f.ctx, f.contractId, f.sourceId, 20, true); added != 0 || err != nil {
			t.Fatal("upgrade repeated historical usage", added, err)
		}
		for index := range 5 {
			want := ByteCount(0)
			if index == 4 {
				want = 20
			}
			if added, err := CloseContractWithReportUsage(f.ctx, f.report()); added != want || err != nil {
				t.Fatal("identified work lost its legacy floor", index, added, err)
			}
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 5, 100)
	})
}

// The first post-upgrade delivery can carry an identity even though an older
// writer counted its earlier id-less delivery. Bootstrap before adding it.
func TestContractCloseLegacyUpgradeIdentifiedRetryKeepsHistoricalFloor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',80,true)`, f.contractId))
		})
		for index := range 5 {
			want := ByteCount(0)
			if index == 4 {
				want = 20
			}
			if added, err := CloseContractWithReportUsage(f.ctx, f.report()); added != want || err != nil {
				t.Fatal("upgrade added an identified retry to ambiguous historical work", index, added, err)
			}
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 5, 100)
	})
}

// A rolling older binary updates the aggregate and optional receipt without
// the new checkpoint totals. Detect that stale projection on the next report.
func TestContractCloseLegacyRollingWriterRefreshesCheckpointTotals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, olderIdentified := range []bool{false, true} {
			f := newCloseReportFixture(t)
			if added, err := CloseContractWithReportUsage(f.ctx, f.report()); added != 20 || err != nil {
				t.Fatal("first current report failed", added, err)
			}
			server.Tx(f.ctx, func(tx server.PgTx) {
				// This is the previous writer's exact aggregate/receipt effect.
				server.RaisePgResult(tx.Exec(f.ctx, `UPDATE contract_close SET used_transfer_byte_count=used_transfer_byte_count+20
					WHERE contract_id=$1 AND party='source' AND checkpoint`, f.contractId))
				if olderIdentified {
					server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO contract_close_report
						(contract_id,party,report_id,used_transfer_byte_count,checkpoint) VALUES($1,'source',$2,20,true)`, f.contractId, server.NewId()))
				}
			})
			firstAdded := ByteCount(0)
			if olderIdentified {
				firstAdded = 20
			}
			for _, want := range []ByteCount{firstAdded, 20} {
				if added, err := CloseContractWithReportUsage(f.ctx, f.report()); added != want || err != nil {
					t.Fatal("rolling old writer left stale checkpoint accounting", olderIdentified, added, want, err)
				}
			}
			assertCloseReportCounts(t, f.ctx, f.contractId, 3, 60+firstAdded)
			var conservative bool
			server.Db(f.ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(f.ctx, `SELECT legacy_checkpoint_byte_count IS NOT NULL
					FROM contract_close WHERE contract_id=$1 AND party='source'`, f.contractId).Scan(&conservative))
			})
			if conservative == olderIdentified {
				t.Fatal("rolling identity evidence lost its exact/conservative distinction", conservative, olderIdentified)
			}
		}
	})
}

// The lower bound rolls back with its transaction. Once committed, losing the
// reply and replaying the operation cannot add bytes or invent report evidence.
func TestContractCloseLegacyRollbackAndLostReply(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		for _, commit := range []bool{false, true} {
			server.Db(f.ctx, func(conn server.PgConn) {
				tx, err := conn.BeginTx(f.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				server.Raise(err)
				defer rollbackCloseReportTestTransaction(f.ctx, tx)
				applied, terminal, added, err := applyContractCloseReportUsageInTx(f.ctx, tx, f.contractId, f.sourceId, 20, true, nil, nil)
				if !applied || terminal || added != 20 || err != nil {
					t.Fatal("legacy transaction did not admit its first bound", applied, terminal, added, err)
				}
				if commit {
					server.Raise(tx.Commit(f.ctx))
				}
			})
			if !commit {
				assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
			}
		}
		if added, err := CloseContractUsage(f.ctx, f.contractId, f.sourceId, 20, true); added != 0 || err != nil {
			t.Fatal("lost-reply retry repeated committed legacy work", added, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 20)
	})
}

// Force the second delivery to wait on the first uncommitted contract lock.
// Database blocking, not scheduling delay, establishes the replay race.
func TestContractCloseLegacyConcurrentRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, time.Minute)
		defer cancel()
		type result struct {
			added ByteCount
			err   error
		}
		ready, finished := make(chan int, 1), make(chan result, 1)
		joined := make(chan struct{})
		started := false
		defer func() {
			cancel()
			if started {
				select {
				case <-joined:
				case <-time.After(time.Minute):
					t.Error("legacy contender did not join canceled cleanup")
				}
			}
		}()
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer rollbackCloseReportTestTransaction(ctx, tx)
			var ownerPid int
			server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPid))
			_, _, added, err := applyContractCloseReportUsageInTx(ctx, tx, f.contractId, f.sourceId, 20, true, nil, nil)
			if err != nil || added != 20 {
				t.Fatal("first legacy owner failed", added, err)
			}
			started = true
			go func() {
				defer close(joined)
				var observed result
				server.HandleError(func() {
					server.Db(ctx, func(other server.PgConn) {
						otherTx, err := other.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
						server.Raise(err)
						defer rollbackCloseReportTestTransaction(ctx, otherTx)
						var pid int
						server.Raise(otherTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
						ready <- pid
						_, _, observed.added, observed.err = applyContractCloseReportUsageInTx(ctx, otherTx, f.contractId, f.sourceId, 20, true, nil, nil)
						if observed.err == nil {
							observed.err = otherTx.Commit(ctx)
						}
					})
				}, func(err error) { observed.err = err })
				finished <- observed
			}()
			select {
			case pid := <-ready:
				waitCloseReportDatabaseConflict(t, ctx, tx, pid, ownerPid)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			server.Raise(tx.Commit(ctx))
		})
		select {
		case observed := <-finished:
			if observed.err != nil || observed.added != 0 {
				t.Fatal("waiting retry added legacy bytes twice", observed)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 20)
	})
}

// Conservative undercounting is not evidence of a dispute. A lower bound
// exceeding an exact peer still is; do not simply disable disputes for legacy.
func TestContractCloseLegacyLowerBoundDisputeDirection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		const mib ByteCount = 1024 * 1024
		for _, legacyEscrow := range []bool{false, true} {
			for _, sourceLower := range []bool{false, true} {
				for _, legacyLower := range []bool{false, true} {
					f := asyncPayoutRecoveryFixture(t, ctx)
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=$2::bigint,
							balance_byte_count=$2::bigint,net_revenue_nano_cents=$2::bigint*2 WHERE balance_id=$1`, f.balanceId, 128*mib))
					})
					var id server.Id
					if legacyEscrow {
						contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100*mib)
						server.RunPosts(ctx, posts...)
						id = contract.ContractId
					} else {
						id = createRedisAdmissionTest(ctx, f, 100*mib).ContractId
					}
					lower, higher := f.destinationId, f.sourceId
					if sourceLower {
						lower, higher = higher, lower
					}
					for _, client := range []server.Id{lower, higher} {
						count := 32 * mib
						if client == higher {
							count = 96 * mib
						}
						if (client == lower) == legacyLower {
							for range 3 {
								server.Raise(CloseContract(ctx, id, client, count, true))
							}
						} else {
							_, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: id,
								ClientId: client, AckedByteCount: count, Checkpoint: true})
							server.Raise(err)
						}
						server.Raise(CloseContract(ctx, id, client, 0, false))
					}
					if legacyEscrow && legacyLower {
						completed, busy, _, err := flushLegacySettlement(ctx, id)
						if !completed || busy || err != nil {
							t.Fatal("compatible legacy lower bound did not settle", completed, busy, err)
						}
					}
					var terminal, disputed, beforeExpiration bool
					var payout ByteCount
					server.Db(ctx, func(conn server.PgConn) {
						server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL,dispute,expiration_time > now(),
							COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1),0)
							FROM transfer_contract WHERE contract_id=$1`, id).Scan(&terminal, &disputed, &beforeExpiration, &payout))
					})
					wantPayout := ByteCount(0)
					if legacyLower {
						wantPayout = 64 * mib
					}
					if terminal != legacyLower || disputed == legacyLower || !beforeExpiration || payout != wantPayout {
						t.Fatalf("lower-bound disagreement misclassified: source_lower=%t legacy_lower=%t terminal=%t disputed=%t payout=%d", sourceLower, legacyLower, terminal, disputed, payout)
					}
				}
			}
		}
	})
}

// Metrics must count a committed report once even if the separate settlement
// returns an error. Its retry resumes settlement without contributing bytes.
func TestContractCloseUsageDeltaSurvivesSettlementError(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, identified := range []bool{false, true} {
			f := asyncPayoutRecoveryFixture(t, ctx)
			id := createRedisAdmissionTest(ctx, f, 600).ContractId
			server.Raise(CloseContract(ctx, id, f.sourceId, 500, false))
			report := ContractCloseReport{ReportId: server.NewId(), ContractId: id, ClientId: f.destinationId, AckedByteCount: 900}
			for _, want := range []ByteCount{900, 0} {
				var added ByteCount
				var err error
				if identified {
					added, err = CloseContractWithReportUsage(ctx, report)
				} else {
					added, err = CloseContractUsage(ctx, id, f.destinationId, 900, false)
				}
				if added != want || !errors.Is(err, errContractInsufficientEscrow) {
					t.Fatal("settlement failure lost or repeated the committed delta", added, want, err)
				}
			}
		}
	})
}
