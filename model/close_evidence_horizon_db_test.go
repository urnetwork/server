// Close evidence keeps its own window since 2026-11-23, apart from the horizon
// reaper's runtime control: the reaper's mode, rate and horizon move the
// contracts alone, and the evidence of a contract the reaper deleted, or
// keeps, goes at the evidence's window, while what the readers need stays.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func removeCloseEvidenceTestBrake(t testing.TB, ctx context.Context) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	server.Raise(server.RemoveCloseEvidenceRetentionControl(controlCtx))
}

// The reaper's control no longer moves the evidence: absent, off, in a dry
// run, or on at 18 or 14 days, the context it gives keeps the evidence 91
// days, where on at 18 days it kept 35 before; the evidence tasks' own context
// keeps the window, 7 days, beside any of them, and 91 days once the evidence
// brake is off.
func TestCloseEvidenceIgnoresTheReaperControl(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		defer removeCloseEvidenceTestBrake(t, ctx)
		removeCloseEvidenceTestBrake(t, ctx)
		const day = 24 * time.Hour
		for _, fields := range []map[string]string{
			nil,
			{ContractHorizonControlFieldMode: "off"},
			{ContractHorizonControlFieldMode: "dry_run", ContractHorizonControlFieldHorizonDays: "18"},
			{ContractHorizonControlFieldMode: "on", ContractHorizonControlFieldHorizonDays: "18"},
			{ContractHorizonControlFieldMode: "on", ContractHorizonControlFieldHorizonDays: "14"},
		} {
			removeHorizonTestControl(t, ctx)
			if fields != nil {
				setHorizonTestControl(t, ctx, fields)
			}
			reaperCtx := WithContractHorizonControl(ctx)
			if retention := server.CloseEvidenceRetention(reaperCtx); retention != server.ContractRetention {
				t.Fatalf("the reaper's control %v keeps the evidence %s", fields, retention)
			}
			if retention := server.CloseEvidenceRetention(server.WithCloseEvidenceRetentionControl(reaperCtx)); retention != 7*day {
				t.Fatalf("beside the reaper's control %v the evidence task keeps %s", fields, retention)
			}
		}
		controlCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		server.Raise(server.SetCloseEvidenceRetentionControl(controlCtx, map[string]string{server.CloseEvidenceRetentionControlFieldMode: "off"}))
		if retention := server.CloseEvidenceRetention(server.WithCloseEvidenceRetentionControl(WithContractHorizonControl(ctx))); retention != server.ContractRetention {
			t.Fatalf("with the brake off the evidence task keeps %s", retention)
		}
	})
}

// With the reaper on at the transition's 18 days, it deletes a settled
// contract 20 days old and keeps one held by its sweep's pending payment; the
// evidence task then deletes the close reports of both at its 7-day window,
// since the reaper's keep rules do not apply to evidence, and the kept
// contract stays. The reports of an open contract of the same age, and a late
// report of it accepted 10 days ago, stay, as do the reports of a settled
// contract 3 days old: admission and the zero-carry check read them. A second
// pass finds nothing.
func TestCloseEvidenceGoesAtItsWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		defer removeCloseEvidenceTestBrake(t, ctx)
		removeCloseEvidenceTestBrake(t, ctx)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc().Truncate(time.Millisecond)

		// credit-bearing and unswept, so nothing but the horizon keeps it
		reaped := newHorizonTestContract(now.Add(-20*day), "settled")
		reaped.unswept = true
		pendingPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{})
		kept := newHorizonTestContract(now.Add(-20*day+time.Minute), "settled")
		kept.paymentId = &pendingPaymentId
		open := newHorizonTestContract(now.Add(-20*day+2*time.Minute), "")
		open.closeTime = nil
		open.settled = false
		recent := newHorizonTestContract(now.Add(-3*day), "settled")
		recent.unswept = true
		insertHorizonTestContracts(t, ctx, f, reaped, kept, open, recent)

		// one report of each party per contract, accepted a minute after its
		// mint; and a late report of the open contract, 10 days ago
		insert := func(clientId server.Id, reportId server.Id, contractId server.Id, party string, acceptedAt time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report_evidence
					(client_id,report_id,contract_id,party,acked_byte_count,unacked_byte_count,checkpoint,accepted_at)
					VALUES($1,$2,$3,$4,0,0,true,$5)`,
					clientId, reportId, contractId, party, acceptedAt.UTC()))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		for _, contract := range []*horizonTestContract{reaped, kept, open, recent} {
			acceptedAt := contract.createTime.Add(time.Minute)
			insert(f.sourceId, horizonTestContractId(acceptedAt), contract.contractId, "source", acceptedAt)
			insert(f.destinationId, horizonTestContractId(acceptedAt), contract.contractId, "destination", acceptedAt)
		}
		insert(f.sourceId, horizonTestContractId(now.Add(-10*day)), open.contractId, "source", now.Add(-10*day))
		evidence := func(contract *horizonTestContract) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_close_report_evidence WHERE contract_id=$1`, contract.contractId).Scan(&count))
			})
			return
		}
		contractPresent := func(contract *horizonTestContract) bool {
			rows, _ := countHorizonTestRows(t, ctx, contract.contractId)
			return rows[0] == 1
		}
		removeEvidence := func() int64 {
			taskCtx := server.WithCloseEvidenceRetentionControl(WithContractHorizonControl(ctx))
			report, _, _ := server.RemoveExpiredCloseEvidence(taskCtx, server.NowUtc(), server.CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
			if report.Stop != server.CloseEvidenceRemovalStopDone || report.Retention != 7*day {
				t.Fatalf("the evidence pass stopped %s at a retention of %s", report.Stop, report.Retention)
			}
			return report.Removed
		}

		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldMode: "on", ContractHorizonControlFieldHorizonDays: "18"})
		if _, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings()); err != nil {
			t.Fatal(err)
		}
		if contractPresent(reaped) || !contractPresent(kept) || !contractPresent(open) || !contractPresent(recent) {
			t.Fatal("the reaper did not delete exactly the 20-day settled contract")
		}
		if evidence(reaped) != 2 {
			t.Fatal("the reaper deleted close evidence")
		}

		if removed := removeEvidence(); removed != 4 {
			t.Fatalf("the evidence pass removed %d rows, want the reaped and the kept contracts' 4", removed)
		}
		if evidence(reaped) != 0 || evidence(kept) != 0 {
			t.Fatalf("evidence past the window stayed: reaped %d, kept %d", evidence(reaped), evidence(kept))
		}
		if !contractPresent(kept) {
			t.Fatal("the evidence pass deleted the contract the reaper keeps")
		}
		// an unresolved contract's evidence and every report inside the
		// window stay
		if evidence(open) != 3 || evidence(recent) != 2 {
			t.Fatalf("evidence a reader needs was removed: open %d, recent %d", evidence(open), evidence(recent))
		}
		if removed := removeEvidence(); removed != 0 {
			t.Fatalf("a second pass removed %d rows", removed)
		}
	})
}
