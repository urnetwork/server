// Rolling release writers share the published receipt and accounting owner.
// Optional stronger evidence never backfills a fact the old writer did not keep.
package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Count the original published receipts independently of companion evidence.
func assertCloseReportLegacyReceipts(t testing.TB, ctx context.Context, contractId server.Id, expected int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var actual int
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_close_report WHERE contract_id=$1`, contractId).Scan(&actual))
		if actual != expected {
			t.Fatal("rolling original receipt census differs", actual, expected)
		}
	})
}

// A pre-upgrade receipt is sufficient for deduplication, never full evidence.
func TestContractCloseUpgradeLegacyReceiptDoesNotRecountOrBackfill(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		if applied, err := CloseContractReport(f.ctx, report.ContractId, report.ClientId, report.AckedByteCount, report.Checkpoint, report.ReportId); !applied || err != nil {
			t.Fatal("published writer did not admit the original report", applied, err)
		}
		for _, unacked := range []uint64{report.UnackedByteCount, ^uint64(0)} {
			report.UnackedByteCount = unacked
			if applied, err := CloseContractWithReport(f.ctx, report); applied || err != nil {
				t.Fatal("upgraded retry recounted or rejected an exact old receipt", applied, err)
			}
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 20)
		assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 1)
		for _, change := range []func(*ContractCloseReport){
			func(r *ContractCloseReport) { r.AckedByteCount++ },
			func(r *ContractCloseReport) { r.Checkpoint = false },
		} {
			changed := report
			change(&changed)
			if applied, err := CloseContractWithReport(f.ctx, changed); applied || !errors.Is(err, ErrContractCloseReportConflict) {
				t.Fatal("old receipt disagreement lost its immutable accounting identity", applied, err)
			}
		}
		report.ReportId = server.NewId()
		if applied, err := CloseContractWithReport(f.ctx, report); !applied || err != nil {
			t.Fatal("legacy unknown evidence stopped independent new work", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 40)
		assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 2)
	})
}

// The stronger writer must publish the legacy receipt in the same transaction.
func TestContractCloseUpgradeNewReceiptDeduplicatesOldWriter(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		if applied, err := CloseContractWithReport(f.ctx, report); !applied || err != nil {
			t.Fatal(err)
		}
		if applied, err := CloseContractReport(f.ctx, report.ContractId, report.ClientId, report.AckedByteCount, report.Checkpoint, report.ReportId); applied || err != nil {
			t.Fatal("old writer recounted new writer's original report", applied, err)
		}
		if applied, err := CloseContractReport(f.ctx, report.ContractId, report.ClientId, report.AckedByteCount+1, report.Checkpoint, report.ReportId); applied || err == nil {
			t.Fatal("old writer bypassed the original receipt content", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
		assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 1)
	})
}

// Failure at legacy receipt publication rolls back the already reserved companion.
func TestContractCloseUpgradeReceiptFailureRollsBackBothCustodyRecords(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE FUNCTION synthetic_rolling_receipt_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic rolling receipt rollback'; END $$;
 CREATE TRIGGER synthetic_rolling_receipt_abort BEFORE INSERT ON contract_close_report FOR EACH ROW EXECUTE FUNCTION synthetic_rolling_receipt_abort()`))
		})
		var refusal error
		server.HandleError(func() { _, refusal = CloseContractWithReport(f.ctx, report) }, func(err error) { refusal = err })
		if refusal == nil || !strings.Contains(refusal.Error(), "synthetic rolling receipt rollback") {
			t.Fatal("new writer did not reach published receipt rollback", refusal)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
		assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 0)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `DROP TRIGGER synthetic_rolling_receipt_abort ON contract_close_report; DROP FUNCTION synthetic_rolling_receipt_abort()`))
		})
		if applied, err := CloseContractWithReport(f.ctx, report); !applied || err != nil {
			t.Fatal("rolled-back new report could not retry", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
		assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 1)
	})
}

// Hold the first actual row owner until the other writer is observed waiting.
func exerciseCloseReportRollingWriters(t testing.TB, oldFirst bool) {
	t.Helper()
	f := newCloseReportFixture(t)
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Minute)
	defer cancel()
	report := f.report()
	type result struct {
		applied bool
		err     error
	}
	ready := make(chan int, 1)
	finished := make(chan result, 1)
	started, joined := false, false
	defer func() {
		cancel()
		if started && !joined {
			select {
			case <-finished:
			case <-time.After(10 * time.Second):
				t.Error("rolling contender did not join after owner cancellation")
			}
		}
	}()
	apply := func(tx server.PgTx, old bool) (bool, error) {
		if old {
			applied, _, err := applyContractCloseReportInTx(ctx, tx, report.ContractId, report.ClientId, report.AckedByteCount, report.Checkpoint, &report.ReportId)
			return applied, err
		}
		return closeContractReportInTx(ctx, tx, report)
	}
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, tx)
		var ownerPid int
		server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPid))
		if applied, err := apply(tx, oldFirst); !applied || err != nil {
			t.Fatal("first rolling writer did not admit its report", applied, err)
		}
		started = true
		go func() {
			var value result
			server.HandleError(func() {
				server.Db(ctx, func(other server.PgConn) {
					otherTx, err := other.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
					server.Raise(err)
					defer rollbackCloseReportTestTransaction(ctx, otherTx)
					var pid int
					server.Raise(otherTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
					ready <- pid
					value.applied, value.err = apply(otherTx, !oldFirst)
					if value.err == nil {
						value.err = otherTx.Commit(ctx)
					}
				})
			}, func(err error) { value.err = err })
			finished <- value
		}()
		var waitingPid int
		select {
		case waitingPid = <-ready:
		case <-ctx.Done():
			t.Fatal("rolling contender did not enter its transaction", ctx.Err())
		}
		waitCloseReportDatabaseConflict(t, ctx, tx, waitingPid, ownerPid)
		server.Raise(tx.Commit(ctx))
	})
	select {
	case value := <-finished:
		joined = true
		if value.applied || value.err != nil {
			t.Fatal("contended rolling retry did not converge without new bytes", value)
		}
	case <-ctx.Done():
		t.Fatal("rolling contender did not join", ctx.Err())
	}
	evidenceCount := 1
	if oldFirst {
		evidenceCount = 0
	}
	assertCloseReportCounts(t, ctx, f.contractId, evidenceCount, 20)
	assertCloseReportLegacyReceipts(t, ctx, f.contractId, 1)
}

// An old owner commits before the new contender may observe its exact receipt.
func TestContractCloseUpgradeConcurrentOldThenNewCommitsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseCloseReportRollingWriters(t, true) })
}

// The reverse order publishes both receipts before the old writer can recount.
func TestContractCloseUpgradeConcurrentNewThenOldCommitsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseCloseReportRollingWriters(t, false) })
}
