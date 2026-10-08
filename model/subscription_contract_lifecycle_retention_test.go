// Retention is terminal for unresolved custody, while already settled history
// and protected candidates must never manufacture another close event.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Real opens include zero-byte contracts. A disputed, unilateral, and untouched
// unresolved row each closes on deletion; a prior outcome and a protected young
// candidate cannot add a close. Small pages force multiple maintenance commits.
func TestContractLifecycleCountersRetentionClosesOnlyDeletedUnresolved(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		before := readContractLifecycleCounterTestSnapshot(t)
		ids := make([]server.Id, 5)
		for index := range ids {
			var err error
			ids[index], err = CreateContractNoEscrow(ctx, fixture.sourceNetworkId, fixture.sourceId,
				fixture.destinationNetworkId, fixture.destinationId, 0)
			server.Raise(err)
		}
		SetContractDispute(ctx, ids[1], true)
		server.Raise(CloseContract(ctx, ids[2], fixture.sourceId, 0, false))
		server.Raise(CloseContract(ctx, ids[3], fixture.sourceId, 0, false))
		server.Raise(CloseContract(ctx, ids[3], fixture.destinationId, 0, false))
		requireContractLifecycleCounterTestDelta(t, before, 5, 1)
		now := server.NowUtc()
		old := now.Add(-400 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract
 SET create_time=$2,close_time=$2,reap_time=$2 WHERE contract_id=ANY($1)`, ids[:4], old))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET reap_time=$2 WHERE contract_id=$1`, ids[4], old))
		})
		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 2)
		server.Db(ctx, func(conn server.PgConn) {
			var remaining, protected, reports, escrows, sweeps int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),
 count(*) FILTER(WHERE contract_id=$2 AND outcome IS NULL AND reap_time IS NULL),
 (SELECT count(*) FROM contract_close WHERE contract_id=ANY($1)),
 (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1)),
 (SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))
 FROM transfer_contract WHERE contract_id=ANY($1)`, ids, ids[4]).Scan(&remaining, &protected, &reports, &escrows, &sweeps))
			if remaining != 1 || protected != 1 || reports != 0 || escrows != 0 || sweeps != 0 {
				t.Fatal("retention did not preserve exactly its protected candidate")
			}
		})
		requireContractLifecycleCounterTestDelta(t, before, 5, 4)
		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 2)
		requireContractLifecycleCounterTestDelta(t, before, 5, 4)
	})
}

// A deferred trigger rejects the real maintenance commit after DELETE returned
// and the owner registered its events. The retained row and counters must agree
// after rollback, successful recovery, and an empty replay.
func TestContractLifecycleCountersRetentionCommitRollback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		before := readContractLifecycleCounterTestSnapshot(t)
		id, err := CreateContractNoEscrow(ctx, fixture.sourceNetworkId, fixture.sourceId,
			fixture.destinationNetworkId, fixture.destinationId, 0)
		server.Raise(err)
		now := server.NowUtc()
		old := now.Add(-400 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract
 SET create_time=$2,close_time=$2,reap_time=$2 WHERE contract_id=$1`, id, old))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION contract_counter_retention_test_rollback()
 RETURNS trigger LANGUAGE plpgsql AS $counter$
 BEGIN RAISE EXCEPTION USING ERRCODE='40001',MESSAGE='synthetic retention commit rollback'; END
 $counter$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE CONSTRAINT TRIGGER contract_counter_retention_test_rollback
 AFTER DELETE ON transfer_contract DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION contract_counter_retention_test_rollback()`))
		})
		var observedErr error
		server.HandleError(func() {
			removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 1)
		}, func(err error) { observedErr = err })
		var pgErr *pgconn.PgError
		if !errors.As(observedErr, &pgErr) || pgErr.Code != "40001" || pgErr.Message != "synthetic retention commit rollback" {
			t.Fatal("retention did not reach its forced commit rollback", observedErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract
 WHERE contract_id=$1 AND outcome IS NULL AND reap_time=$2)`, id, old).Scan(&retained))
			if !retained {
				t.Fatal("failed maintenance commit deleted unresolved custody")
			}
		})
		requireContractLifecycleCounterTestDelta(t, before, 1, 0)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER contract_counter_retention_test_rollback ON transfer_contract`))
			server.RaisePgResult(tx.Exec(ctx, `DROP FUNCTION contract_counter_retention_test_rollback()`))
		})
		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 1)
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1)`, id).Scan(&retained))
			if retained {
				t.Fatal("maintenance recovery did not delete unresolved custody")
			}
		})
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 1)
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
	})
}

// The reaper reads its candidate before an outcome commits and demonstrably
// waits on that owner's row lock. DELETE must classify the actual deleted row,
// so the winning outcome and subsequent history deletion close only once.
func TestContractLifecycleCountersRetentionWaitsForConcurrentOutcome(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		var reaperDone chan any
		defer func() {
			cancel()
			if reaperDone != nil {
				<-reaperDone
			}
		}()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		before := readContractLifecycleCounterTestSnapshot(t)
		id, err := CreateContractNoEscrow(ctx, fixture.sourceNetworkId, fixture.sourceId,
			fixture.destinationNetworkId, fixture.destinationId, 0)
		server.Raise(err)
		now := server.NowUtc()
		old := now.Add(-400 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract
 SET create_time=$2,close_time=$2,reap_time=$2 WHERE contract_id=$1`, id, old))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
 (contract_id,party,used_transfer_byte_count,checkpoint)
 VALUES($1,'source',0,false),($1,'destination',0,false)`, id))
		})
		server.Tx(ctx, func(tx server.PgTx) {
			claimed, err := claimContractOutcomeInTx(ctx, tx, id, ContractOutcomeSettled)
			server.Raise(err)
			if !claimed {
				t.Fatal("concurrent fixture failed to acquire its actual outcome")
			}
			requireContractLifecycleCounterTestDelta(t, before, 1, 0)
			reaperDone = make(chan any, 1)
			go func() {
				reaperDone <- server.HandleError(func() {
					removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 1)
				})
			}()
			requireContractLifecycleBlockedBy(t, ctx, tx, contractLifecycleTestBackendPid(t, ctx, tx))
		}, server.TxReadCommitted, server.OptNoRetry())
		reaperErr := <-reaperDone
		reaperDone = nil
		if reaperErr != nil {
			t.Fatal("retention failed after the winning outcome committed", reaperErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1)`, id).Scan(&retained))
			if retained {
				t.Fatal("retention did not delete the newly terminal history")
			}
		})
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
	})
}
