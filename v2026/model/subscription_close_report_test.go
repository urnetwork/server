package model

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func requireCloseReportState(t testing.TB, ctx context.Context, id server.Id, reports, bytes int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var gotReports, gotBytes int
		server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM contract_close_report WHERE contract_id=$1),
 COALESCE((SELECT sum(used_transfer_byte_count) FROM contract_close WHERE contract_id=$1),0)`, id).Scan(&gotReports, &gotBytes))
		if gotReports != reports || gotBytes != bytes {
			t.Fatal("logical receipt and close amount diverged")
		}
	})
}

func TestCloseReportRollbackCommitLossAndFinalReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		e, posts := createNetEscrowOrderingTestContract(ctx, f, 200)
		server.RunPosts(ctx, posts...)
		id := server.NewId()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		for _, commit := range []bool{false, true} {
			tx, err := conn.Begin(ctx)
			server.Raise(err)
			applied, terminal, err := applyContractCloseReportInTx(ctx, tx, e.ContractId, f.destinationId, 100, true, &id)
			if err != nil || !applied || terminal {
				t.Fatal("report transaction did not claim its increment")
			}
			if commit {
				server.Raise(tx.Commit(ctx))
			} else {
				server.Raise(tx.Rollback(ctx))
				requireCloseReportState(t, ctx, e.ContractId, 0, 0)
			}
		}
		// The commit owner exits before settlement or any response is delivered.
		// Its retry resumes the real public operation with an exact durable ID.
		applied, err := CloseContractReport(ctx, e.ContractId, f.destinationId, 100, true, id)
		if err != nil || applied {
			t.Fatal("committed report replay repeated consumption")
		}
		requireCloseReportState(t, ctx, e.ContractId, 1, 100)
		if applied, err := CloseContractReport(ctx, e.ContractId, f.destinationId, 101, true, id); err == nil || applied {
			t.Fatal("same ID accepted different amount")
		}
		if applied, err := CloseContractReport(ctx, e.ContractId, f.destinationId, 100, false, id); err == nil || applied {
			t.Fatal("same ID accepted different finality")
		}
		if applied, err := CloseContractReport(ctx, e.ContractId, server.NewId(), 100, true, id); err == nil || applied {
			t.Fatal("non-party borrowed report receipt")
		}
		if applied, err := CloseContractReport(ctx, e.ContractId, f.destinationId, 100, true, server.Id{}); err == nil || applied {
			t.Fatal("zero report identity accepted")
		}
		// Equal-sized legitimate reports remain distinct.
		applied, err = CloseContractReport(ctx, e.ContractId, f.destinationId, 100, true, server.NewId())
		if err != nil || !applied {
			t.Fatal("distinct equal-sized checkpoint was lost")
		}
		final := server.NewId()
		applied, err = CloseContractReport(ctx, e.ContractId, f.destinationId, 0, false, final)
		if err != nil || !applied {
			t.Fatal("final report was lost")
		}
		sourceFinal := server.NewId()
		applied, err = CloseContractReport(ctx, e.ContractId, f.sourceId, 200, false, sourceFinal)
		if err != nil || !applied {
			t.Fatal("source final report was lost")
		}
		requireCloseReportState(t, ctx, e.ContractId, 4, 400)
		complete, busy, _, err := flushLegacySettlement(ctx, e.ContractId)
		if err != nil || !complete || busy {
			t.Fatal("idempotent reports did not settle")
		}
		requireLegacySettlementTestState(t, ctx, f, e.ContractId, false, true, 800, 0)
		requireLegacyProviderDurability(t, ctx, f, e.ContractId, 200)
		for _, r := range []struct {
			client     server.Id
			count      ByteCount
			checkpoint bool
			id         server.Id
		}{{f.destinationId, 100, true, id}, {f.destinationId, 0, false, final}, {f.sourceId, 200, false, sourceFinal}} {
			applied, err := CloseContractReport(ctx, e.ContractId, r.client, r.count, r.checkpoint, r.id)
			if err != nil || applied {
				t.Fatal("terminal report replay was not acknowledged inertly")
			}
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		var canceledErr error
		server.HandleError(func() {
			_, canceledErr = CloseContractReport(canceled, e.ContractId, f.destinationId, 1, true, server.NewId())
		}, func(err error) { canceledErr = err })
		if canceledErr == nil {
			t.Fatal("canceled report owner completed")
		}
		requireCloseReportState(t, ctx, e.ContractId, 4, 400)
		requireLegacyProviderDurability(t, ctx, f, e.ContractId, 200)
	})
}

func TestCloseReportReceiptFailureRollsBackIncrement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		e, posts := createNetEscrowOrderingTestContract(ctx, f, 200)
		server.RunPosts(ctx, posts...)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_close_report_fault() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN RAISE EXCEPTION 'synthetic report receipt failure' USING ERRCODE='23514'; END $f$;
 CREATE TRIGGER test_close_report_fault BEFORE INSERT ON contract_close_report FOR EACH ROW EXECUTE FUNCTION test_close_report_fault();`))
		})
		id := server.NewId()
		var failure error
		server.HandleError(func() { _, failure = CloseContractReport(ctx, e.ContractId, f.destinationId, 100, true, id) }, func(err error) { failure = err })
		if failure == nil {
			t.Fatal("receipt fault did not reach transaction owner")
		}
		requireCloseReportState(t, ctx, e.ContractId, 0, 0)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER test_close_report_fault ON contract_close_report; DROP FUNCTION test_close_report_fault();`))
		})
		applied, err := CloseContractReport(ctx, e.ContractId, f.destinationId, 100, true, id)
		if err != nil || !applied {
			t.Fatal("receipt retry could not claim rolled-back operation")
		}
		requireCloseReportState(t, ctx, e.ContractId, 1, 100)
	})
}

func TestCloseReportConcurrentReplayAndSamePayerIsolation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		var contracts []server.Id
		for range 128 {
			e, posts := createNetEscrowOrderingTestContract(ctx, f, 1)
			server.RunPosts(ctx, posts...)
			contracts = append(contracts, e.ContractId)
		}
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.Background())
		server.RaisePgResult(tx.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		var claims, failures atomic.Int32
		for _, contract := range contracts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				server.HandleError(func() {
					applied, err := CloseContractReport(bounded, contract, f.destinationId, 1, true, server.NewId())
					if err != nil || !applied {
						failures.Add(1)
					} else {
						claims.Add(1)
					}
				}, func(error) { failures.Add(1) })
			}()
		}
		wg.Wait()
		if claims.Load() != 128 || failures.Load() != 0 {
			t.Fatal("independent contracts waited on a shared payer lock")
		}
		server.Raise(tx.Rollback(ctx))
		id := server.NewId()
		claims.Store(0)
		// One additional equal-sized checkpoint, concurrently replayed 32 times.
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				server.HandleError(func() {
					applied, err := CloseContractReport(ctx, contracts[0], f.destinationId, 1, true, id)
					if err != nil {
						failures.Add(1)
					} else if applied {
						claims.Add(1)
					}
				}, func(error) { failures.Add(1) })
			}()
		}
		wg.Wait()
		if claims.Load() != 1 || failures.Load() != 0 {
			t.Fatal("concurrent logical replay did not claim exactly once")
		}
		requireCloseReportState(t, ctx, contracts[0], 2, 2)
	})
}
