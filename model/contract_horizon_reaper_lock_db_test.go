package model

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// A contract another transaction holds, or one whose sweep a payout writer
// holds, is skipped as busy rather than waited on, so the reaper adds no lock
// queue behind a writer; the rest of the batch goes, and a later pass takes
// the skipped contracts once they are free.
func TestContractHorizonSkipsBusyContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		paymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * 24 * time.Hour))})
		heldContract := newHorizonTestContract(now.Add(-120*24*time.Hour), "canceled")
		heldSweep := newHorizonTestContract(now.Add(-120*24*time.Hour+time.Second), "settled")
		heldSweep.paymentId = &paymentId
		free := newHorizonTestContract(now.Add(-120*24*time.Hour+2*time.Second), "canceled")
		insertHorizonTestContracts(t, ctx, f, heldContract, heldSweep, free)

		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.WithoutCancel(ctx))
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, heldContract.contractId))
		// as the payout planner holds a sweep it assigns
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1 FOR UPDATE`, heldSweep.contractId))

		settings := horizonTestSettings()
		settings.LockTimeout = 200 * time.Millisecond
		report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Protected[contractHorizonProtectedBusy] != 2 || report.Eligible != 1 {
			t.Fatalf("held contracts were waited on or deleted: %+v %v", report, err)
		}
		if rows, _ := countHorizonTestRows(t, ctx, heldContract.contractId); rows != [6]int{1, 2, 1, 0, 1, 2} {
			t.Fatal("the held contract lost rows", rows)
		}
		if rows, _ := countHorizonTestRows(t, ctx, heldSweep.contractId); rows != [6]int{1, 2, 1, 1, 1, 2} {
			t.Fatal("the contract with a held sweep lost rows", rows)
		}
		if rows, _ := countHorizonTestRows(t, ctx, free.contractId); rows != [6]int{} {
			t.Fatal("the free contract was kept", rows)
		}

		server.Raise(held.Rollback(ctx))
		report, _, err = RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Eligible != 2 || report.Protected[contractHorizonProtectedBusy] != 0 {
			t.Fatalf("a later pass did not take the released contracts: %+v %v", report, err)
		}
	})
}
