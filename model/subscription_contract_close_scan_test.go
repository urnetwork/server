// Synchronous scans prove terminal effects without a running task worker.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Two pages include legacy, expired and future rows. The observer sees only
// committed terminal contracts; a failed item cannot stop unrelated progress.
func TestSynchronousContractScanVerifiesClosuresAndContinuesFailure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newFreeExpiryOwnerFixture(ctx)
		ids := []server.Id{}
		for range 5 {
			id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
			server.Raise(err)
			ids = append(ids, id)
		}
		at := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=ANY($1)`, ids, at.Add(-time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=$1`, ids[1]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, ids[4], at.Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_refuse_deadline() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF OLD.transfer_byte_count=101 AND NEW.outcome IS NOT NULL THEN RAISE EXCEPTION 'synthetic deadline failure'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER synthetic_refuse_deadline BEFORE UPDATE OF outcome ON transfer_contract FOR EACH ROW EXECUTE FUNCTION synthetic_refuse_deadline()`))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET transfer_byte_count=101 WHERE contract_id=$1`, ids[0]))
		})
		observed, failed := 0, 0
		result, err := CloseOpenContractsSynchronously(ctx, ContractClosureScanOptions{At: at, PageSize: 2}, func(closed *ContractDeadlineReconciliation, err error) error {
			observed++
			if err != nil {
				failed++
				return nil
			}
			if _, terminal := GetContractClose(ctx, closed.ContractId); !terminal {
				t.Fatal("observer received an uncommitted or open contract")
			}
			return nil
		})
		if err == nil || result.Visited != 5 || result.Closed != 3 || result.Failed != 1 || result.Deferred != 1 || observed != 4 || failed != 1 {
			t.Fatal("synchronous scan lost verified progress or swallowed a failure", result, observed, failed, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var pending int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task`).Scan(&pending))
			if pending != 0 {
				t.Fatal("synchronous scan published tasks", pending)
			}
		})
	})
}

// Cancellation at an observed commit must stop before the next item, even when
// the complete next page is already in memory.
func TestSynchronousContractScanCancellationStopsBetweenContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		base := WithProviderWorkSessionSource(t.Context(), nil)
		f := newFreeExpiryOwnerFixture(base)
		for range 3 {
			id, err := CreateContractNoEscrow(base, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
			server.Raise(err)
			server.Tx(base, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(base, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=$1`, id))
			})
		}
		ctx, cancel := context.WithCancel(base)
		defer cancel()
		result, err := CloseOpenContractsSynchronously(ctx, ContractClosureScanOptions{At: server.NowUtc(), PageSize: 3}, func(closed *ContractDeadlineReconciliation, err error) error {
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) || result.Visited != 1 || result.Closed != 1 {
			t.Fatal("scan continued after cancellation or lost its committed prefix", result, err)
		}
		server.Db(base, func(conn server.PgConn) {
			var open int
			server.Raise(conn.QueryRow(base, `SELECT count(*) FROM transfer_contract WHERE outcome IS NULL`).Scan(&open))
			if open != 2 {
				t.Fatal("canceled scan touched another contract", open)
			}
		})
	})
}
