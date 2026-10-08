// Exercise real lifecycle owners across commit retries and competing closes.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// A deferred trigger rejects the first actual INSERT commit. Its sequence is
// nontransactional, so the next fresh no-retry transaction can commit. The
// test retries only the proven rollback, matching the creation owner's fence.
func TestContractLifecycleCountersCreateCommitRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE contract_counter_retry_test_attempt`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION contract_counter_retry_test_rollback()
 RETURNS trigger LANGUAGE plpgsql AS $counter$
 BEGIN
  IF nextval('contract_counter_retry_test_attempt')=1 THEN
   RAISE EXCEPTION USING ERRCODE='40001',MESSAGE='synthetic lifecycle commit retry';
  END IF;
  RETURN NEW;
 END
 $counter$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE CONSTRAINT TRIGGER contract_counter_retry_test_rollback
 AFTER INSERT ON transfer_contract DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION contract_counter_retry_test_rollback()`))
		})
		for _, backend := range []string{"legacy", "redis", "no_escrow"} {
			fixture := newNetEscrowOrderingTestFixture(t, ctx)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `ALTER SEQUENCE contract_counter_retry_test_attempt RESTART WITH 1`))
			})
			request := ctx
			if backend == "redis" {
				request = withRedisContractAdmission(ctx)
			}
			before := readContractLifecycleCounterTestSnapshot(t)
			var attemptedIds []server.Id
			for attempt := range 2 {
				var attemptErr error
				server.HandleError(func() {
					server.Tx(request, func(tx server.PgTx) {
						var id server.Id
						if backend == "no_escrow" {
							var err error
							id, _, err = createContractNoEscrowInTx(request, tx, fixture.sourceNetworkId, fixture.sourceId,
								fixture.destinationNetworkId, fixture.destinationId, 100, true)
							server.Raise(err)
						} else {
							contract, _, err := createTransferEscrowInTx(request, tx, fixture.sourceNetworkId, fixture.sourceId,
								fixture.destinationNetworkId, fixture.destinationId, fixture.sourceNetworkId, 100, nil)
							server.Raise(err)
							id = contract.ContractId
						}
						attemptedIds = append(attemptedIds, id)
						requireContractLifecycleCounterTestDelta(t, before, 0, 0)
					}, server.TxReadCommitted, server.OptNoRetry())
				}, func(err error) { attemptErr = err })
				if attempt == 0 {
					var pgErr *pgconn.PgError
					if len(attemptedIds) != 1 || !errors.As(attemptErr, &pgErr) || pgErr.Code != "40001" {
						t.Fatalf("%s first commit did not reach its deferred rollback: %v", backend, attemptErr)
					}
					requireContractLifecycleCounterTestDelta(t, before, 0, 0)
					server.Db(ctx, func(conn server.PgConn) {
						var contracts, escrows int
						server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM transfer_contract WHERE contract_id=$1),
 (SELECT count(*) FROM transfer_escrow WHERE contract_id=$1)`, attemptedIds[0]).Scan(&contracts, &escrows))
						if contracts != 0 || escrows != 0 {
							t.Fatal("failed commit retained SQL custody", contracts, escrows)
						}
					})
				} else if attemptErr != nil {
					t.Fatalf("%s retry did not commit: %v", backend, attemptErr)
				}
			}
			if len(attemptedIds) != 2 {
				t.Fatalf("%s creation attempts=%d, want one failed commit and one successful commit", backend, len(attemptedIds))
			}
			server.Db(ctx, func(conn server.PgConn) {
				var commits, contracts, escrows, marked int
				var committedId server.Id
				server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT last_value FROM contract_counter_retry_test_attempt),count(*),
 (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1)),
 (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND redis_reserved)
 FROM transfer_contract WHERE contract_id=ANY($1)`, attemptedIds).Scan(&commits, &contracts, &escrows, &marked))
				server.Raise(conn.QueryRow(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=ANY($1)`, attemptedIds).Scan(&committedId))
				wantEscrows, wantMarked := 1, 0
				if backend == "no_escrow" {
					wantEscrows = 0
				} else if backend == "redis" {
					wantMarked = 1
				}
				if commits != 2 || contracts != 1 || escrows != wantEscrows || marked != wantMarked || committedId != attemptedIds[1] {
					t.Fatalf("%s retry retained incorrect custody: commits=%d contracts=%d escrows=%d marked=%d", backend, commits, contracts, escrows, marked)
				}
			})
			requireContractLifecycleCounterTestDelta(t, before, 1, 0)
			if backend == "redis" {
				// Redis token replay and explicit projection repair cannot publish
				// another open after PostgreSQL has acquired custody.
				ReconcileRedisContractReservation(ctx, attemptedIds[1])
				ReconcileRedisContractReservation(ctx, attemptedIds[1])
				requireContractLifecycleCounterTestDelta(t, before, 1, 0)
			}
		}
	})
}

// Hold the real outcome before commit, and prove the duplicate public close
// waits on its row. Commit or rollback chooses the sole successful close owner.
func TestContractLifecycleCountersConcurrentCloseCommitsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, rollback := range []bool{false, true} {
			func() {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				type result struct {
					applied bool
					err     error
				}
				var duplicateDone chan result
				defer func() {
					cancel()
					if duplicateDone != nil {
						<-duplicateDone
					}
				}()
				fixture := newNetEscrowOrderingTestFixture(t, ctx)
				before := readContractLifecycleCounterTestSnapshot(t)
				id, err := CreateContractNoEscrow(ctx, fixture.sourceNetworkId, fixture.sourceId,
					fixture.destinationNetworkId, fixture.destinationId, 100)
				server.Raise(err)
				applied, err := CloseContractReport(ctx, id, fixture.sourceId, 11, false, server.NewId())
				if err != nil || !applied {
					t.Fatal("first party report failed", applied, err)
				}
				reportId := server.NewId()
				rollbackErr := errors.New("synthetic concurrent close rollback")
				var ownerErr error
				server.HandleError(func() {
					server.Tx(ctx, func(tx server.PgTx) {
						applied, terminalReplay, err := applyContractCloseReportInTx(ctx, tx, id, fixture.destinationId, 11, false, &reportId)
						if err != nil || !applied || terminalReplay {
							t.Fatal("outcome owner failed to apply its report", applied, terminalReplay, err)
						}
						claimed, err := claimContractOutcomeInTx(ctx, tx, id, ContractOutcomeSettled)
						if err != nil || !claimed {
							t.Fatal("outcome owner failed to claim the transition", claimed, err)
						}
						requireContractLifecycleCounterTestDelta(t, before, 1, 0)
						duplicateDone = make(chan result, 1)
						go func() {
							got := result{}
							server.HandleError(func() {
								var err error
								got.applied, err = CloseContractReport(ctx, id, fixture.destinationId, 11, false, reportId)
								server.Raise(err)
							}, func(err error) { got.err = err })
							duplicateDone <- got
						}()
						requireContractLifecycleBlockedBy(t, ctx, tx, contractLifecycleTestBackendPid(t, ctx, tx))
						if rollback {
							server.Raise(rollbackErr)
						}
					}, server.TxReadCommitted, server.OptNoRetry())
				}, func(err error) { ownerErr = err })
				if (rollback && !errors.Is(ownerErr, rollbackErr)) || (!rollback && ownerErr != nil) {
					t.Fatal("unexpected first outcome disposition", ownerErr)
				}
				got := <-duplicateDone
				duplicateDone = nil
				if got.err != nil || got.applied != rollback {
					t.Fatal("duplicate failed to follow the first owner's commit", got.applied, got.err)
				}
				server.Db(ctx, func(conn server.PgConn) {
					var terminal bool
					var reports, receipts int
					var usage ByteCount
					server.Raise(conn.QueryRow(ctx, `SELECT outcome=$2,
 (SELECT count(*) FROM contract_close WHERE contract_id=$1),
 (SELECT sum(used_transfer_byte_count) FROM contract_close WHERE contract_id=$1),
 (SELECT count(*) FROM contract_close_report WHERE contract_id=$1)
 FROM transfer_contract WHERE contract_id=$1`, id, ContractOutcomeSettled).Scan(&terminal, &reports, &usage, &receipts))
					if !terminal || reports != 2 || usage != 22 || receipts != 2 {
						t.Fatal("competing closes changed durable reports or terminal ownership")
					}
				})
				requireContractLifecycleCounterTestDelta(t, before, 1, 1)
				applied, err = CloseContractReport(ctx, id, fixture.destinationId, 11, false, reportId)
				if err != nil || applied {
					t.Fatal("completed duplicate repeated its report", applied, err)
				}
				requireContractLifecycleCounterTestDelta(t, before, 1, 1)
			}()
		}
	})
}
