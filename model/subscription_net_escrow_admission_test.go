// Admission consumes database-owned credit; cache publication is not a second
// financial commit. Retain real callbacks to force each missed-post boundary.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
)

// While the first creator's transaction is open, a competitor is refused before
// it reads any credit. The pre-fix writer admitted stale credit before waiting
// on its revision insert; since 403b9c55 the legacy allocator admits every
// eligible grant's owner first, so the competitor is refused instead of
// waiting on the first creator's rows.
func TestNetEscrowAdmissionSerializesCompetingCreators(t *testing.T) {
	testNetEscrowAdmissionWait(t, false, false)
}

// A losing transaction leaves neither a reservation nor a phantom debit.
func TestNetEscrowAdmissionWaiterUsesRolledBackCredit(t *testing.T) {
	testNetEscrowAdmissionWait(t, true, false)
}

// Bounded prober discovery must be refused before its durable reservation
// census. The first creator deliberately commits without publishing its cache post.
func TestDynamicProberGrantSerializesCompetingCreators(t *testing.T) {
	testNetEscrowAdmissionWait(t, false, true)
}

// A speculative prober window must observe credit released by a rolled-back
// creator, without needing a cache repair or falling through to another grant.
func TestDynamicProberGrantWaiterUsesRolledBackCredit(t *testing.T) {
	testNetEscrowAdmissionWait(t, true, true)
}

// Hold the first creator's no-retry transaction open at a barrier, refuse a
// competitor inside it, then retry the competitor after commit or rollback.
func testNetEscrowAdmissionWait(t *testing.T, rollback, internalProber bool) {
	t.Helper()
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		if internalProber {
			setDynamicProberIdentityForTest(t, ctx, escrowSelectionTestClients{
				payerNetworkId: f.sourceNetworkId, payerId: f.sourceId,
			})
			server.Tx(ctx, func(tx server.PgTx) {
				// Keep exactly 1000 spendable bytes while qualifying as a partly
				// spent internal top-up. Both creators target this fast window.
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance
					SET start_balance_byte_count=$2 WHERE balance_id=$1`, f.balanceId, ProberTransferBalanceTopUp))
			}, server.TxReadCommitted)
		}
		create := func(tx server.PgTx) (*TransferEscrow, error) {
			contract, _, err := createTransferEscrowInTx(ctx, tx,
				f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
				f.sourceNetworkId, 600, nil)
			return contract, err
		}
		compete := func() (contract *TransferEscrow, err error) {
			recovered := server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					contract, err = create(tx)
				}, server.TxReadCommitted, server.OptNoRetry())
			})
			if recovered != nil {
				contract, err = nil, fmt.Errorf("admission panic: %v", recovered)
				if recoveredErr, ok := recovered.(error); ok {
					err = recoveredErr
				}
			}
			return
		}
		errRollback := errors.New("synthetic first creator rollback")
		held := make(chan struct{})
		finish := make(chan struct{})
		firstDone := make(chan any, 1)
		go func() {
			firstDone <- server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					if _, err := create(tx); err != nil {
						server.Raise(err)
					}
					close(held)
					select {
					case <-finish:
					case <-ctx.Done():
						server.Raise(ctx.Err())
					}
					if rollback {
						server.Raise(errRollback)
					}
				}, server.TxReadCommitted, server.OptNoRetry())
			})
		}()
		select {
		case <-held:
		case recovered := <-firstDone:
			t.Fatalf("first creator did not reserve: %v", recovered)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if contract, err := compete(); contract != nil || !errors.Is(err, errTransferBalanceOwnershipBusy) {
			close(finish)
			<-firstDone
			t.Fatalf("competing creator entered while the first owned the grant: contract=%+v error=%v", contract, err)
		}
		close(finish)
		recovered := <-firstDone
		if recoveredErr, _ := recovered.(error); (rollback && !errors.Is(recoveredErr, errRollback)) || (!rollback && recovered != nil) {
			t.Fatalf("first creator outcome=%v, rollback=%t", recovered, rollback)
		}
		contract, err := compete()
		if rollback {
			if err != nil || contract == nil {
				t.Fatalf("rollback did not release credit: contract=%+v error=%v", contract, err)
			}
		} else if contract != nil || err == nil || !strings.Contains(err.Error(), "Insufficient balance") {
			t.Fatalf("competing creator re-admitted committed credit: contract=%+v error=%v", contract, err)
		}
		if got := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 600 {
			t.Fatalf("durable reserved credit=%d, want exactly one 600-byte reservation", got)
		}
	})
}

func TestNetEscrowAdmissionSurvivesMissingCreatePost(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		createNetEscrowOrderingTestContract(ctx, f, 600)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 0 {
			t.Fatalf("missed post fixture unexpectedly published %d bytes", got)
		}
		var admitted *TransferEscrow
		var admissionErr error
		server.Tx(ctx, func(tx server.PgTx) {
			admitted, _, admissionErr = createTransferEscrowInTx(ctx, tx,
				f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
				f.sourceNetworkId, 600, nil)
		}, server.TxReadCommitted, server.OptNoRetry())
		if admissionErr == nil || admitted != nil || !strings.Contains(admissionErr.Error(), "Insufficient balance") {
			t.Fatalf("missing cache admitted the same credit twice: contract=%+v error=%v", admitted, admissionErr)
		}
		if got := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 600 {
			t.Fatalf("failed admission changed durable reservation to %d", got)
		}
	})
}

// Terminal outcome and debit must commit together. Reconciliation can correctly
// remove a closed reservation even if every original settlement post is lost.
func TestNetEscrowAdmissionSurvivesMissingSettlementPost(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 700)
		server.RunPosts(ctx, posts...)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',600,$2,false),($1,'destination',600,$2,false)`, contract.ContractId, server.NowUtc()))
			_, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("settlement did not claim terminal outcome")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		var balance ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&balance))
		})
		if balance != 400 {
			t.Errorf("terminal outcome committed without its debit: remaining=%d want=400", balance)
		}
		ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		var admitted *TransferEscrow
		var admissionErr error
		server.Tx(ctx, func(tx server.PgTx) {
			admitted, _, admissionErr = createTransferEscrowInTx(ctx, tx,
				f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
				f.sourceNetworkId, 600, nil)
		}, server.TxReadCommitted, server.OptNoRetry())
		if admissionErr == nil || admitted != nil {
			t.Fatalf("lost settlement post re-admitted spent credit: contract=%+v error=%v", admitted, admissionErr)
		}
	})
}

// Cache restores, stale overestimates and malformed counters cannot either
// authorize reserved credit or deny free database-owned credit.
func TestNetEscrowAdmissionIgnoresRestoredCache(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, value := range []string{"0", "-600", "9223372036854775807", "corrupt"} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			createNetEscrowOrderingTestContract(ctx, f, 600)
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, netEscrowKey(f.balanceId), value, 0).Err())
			})
			createNetEscrowOrderingTestContract(ctx, f, 400)
			var admitted *TransferEscrow
			var err error
			server.Tx(ctx, func(tx server.PgTx) {
				admitted, _, err = createTransferEscrowInTx(ctx, tx,
					f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
					f.sourceNetworkId, 1, nil)
			}, server.TxReadCommitted, server.OptNoRetry())
			if admitted != nil || err == nil || !strings.Contains(err.Error(), "Insufficient balance") {
				t.Fatalf("cache %q authorized nonexistent credit: admitted=%+v error=%v", value, admitted, err)
			}
			if reserved := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; reserved != 1000 {
				t.Fatalf("cache %q changed admitted total to %d", value, reserved)
			}
		}
	})
}

// A retry after both writes rolls back the outcome and debit together; the
// terminal claim also prevents a later settlement call from charging twice.
// Settlement admits its grant owners (403b9c55), so it runs only in a no-retry
// transaction and its caller retries with a fresh one, as modeled here.
func TestNetEscrowSettlementRetryDebitsOnce(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 700)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',600,$2,false),($1,'destination',600,$2,false)`, contract.ContractId, server.NowUtc()))
		}, server.TxReadCommitted)
		readSettlement := func() (outcome *string, balance ByteCount) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT
					(SELECT outcome FROM transfer_contract WHERE contract_id=$1),
					(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)`,
					contract.ContractId, f.balanceId).Scan(&outcome, &balance))
			})
			return
		}
		for attempt := 1; attempt <= 2; attempt++ {
			recovered := server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					_, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
					server.Raise(err)
					if !closed {
						t.Fatal("retry lost terminal outcome ownership")
					}
					if attempt == 1 {
						server.Raise(&pgconn.PgError{Code: "40001", Message: "synthetic retry after outcome and debit"})
					}
				}, server.TxReadCommitted, server.OptNoRetry())
			})
			if attempt == 1 {
				if outcome, balance := readSettlement(); recovered == nil || outcome != nil || balance != 1000 {
					t.Fatalf("failed attempt kept its outcome or debit: recovered=%v balance=%d", recovered, balance)
				}
			} else if recovered != nil {
				t.Fatalf("retried settlement failed: %v", recovered)
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			posts, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if closed || len(posts) != 0 {
				t.Fatal("replay reclaimed terminal settlement")
			}
			var balance ByteCount
			server.Raise(tx.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&balance))
			if balance != 400 {
				t.Fatalf("retry/replay debit balance=%d, want 400", balance)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
	})
}

// A negative request must not manufacture negative reserved credit; an empty
// request retains the existing anchor without consuming or releasing bytes.
func TestNetEscrowAdmissionRejectsNegativePreservesZero(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			contract, posts, err := createTransferEscrowInTx(ctx, tx,
				f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
				f.sourceNetworkId, -1, nil)
			if err == nil || contract != nil || len(posts) != 0 {
				t.Fatal("negative request created a reservation")
			}
		}, server.TxReadCommitted)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 0)
		if len(posts) != 0 || len(contract.Balances) != 1 || contract.Balances[0].BalanceId != f.balanceId || contract.Balances[0].BalanceByteCount != 0 {
			t.Fatalf("empty contract lost its zero-credit anchor: %+v", contract)
		}
		if count := contractLifecycleTestCount(t, ctx, f.sourceId, f.destinationId); count != 1 {
			t.Fatalf("negative request persisted a contract: count=%d", count)
		}
	})
}
