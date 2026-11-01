// Close evidence follows the horizon reaper's runtime control: the evidence of
// a contract the reaper deleted goes with it at the same horizon, and what the
// readers need stays.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The control sets the horizon for the reaper and the evidence alike. Without
// it both keep 91 days, so nothing here is touched. At horizonDays 45 the
// reaper deletes a settled contract 60 days old, which leaves its close
// reports without a contract, and the evidence pass then deletes them; the
// reports of an open contract of the same age, and of a settled contract 30
// days old, stay, as does an old contract's report accepted 30 days ago. A
// control that cannot be read keeps 91 days.
func TestCloseEvidenceGoesWithTheReapedContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc().Truncate(time.Millisecond)

		// credit-bearing and unswept, so nothing but the horizon keeps it
		reaped := newHorizonTestContract(now.Add(-60*day), "settled")
		reaped.unswept = true
		open := newHorizonTestContract(now.Add(-60*day+time.Minute), "")
		open.closeTime = nil
		open.settled = false
		young := newHorizonTestContract(now.Add(-30*day), "settled")
		young.unswept = true
		insertHorizonTestContracts(t, ctx, f, reaped, open, young)

		// one report of each party per contract, accepted a minute after its
		// mint; and a late report of the open contract, 30 days ago
		type report struct {
			clientId   server.Id
			reportId   server.Id
			contractId server.Id
			party      string
			acceptedAt time.Time
		}
		reports := map[server.Id][]report{}
		for _, contract := range []*horizonTestContract{reaped, open, young} {
			for _, party := range []struct {
				name     string
				clientId server.Id
			}{{"source", f.sourceId}, {"destination", f.destinationId}} {
				reports[contract.contractId] = append(reports[contract.contractId], report{
					clientId: party.clientId, reportId: horizonTestContractId(contract.createTime.Add(time.Minute)),
					contractId: contract.contractId, party: party.name, acceptedAt: contract.createTime.Add(time.Minute),
				})
			}
		}
		late := report{clientId: f.sourceId, reportId: horizonTestContractId(now.Add(-30 * day)), contractId: open.contractId, party: "source", acceptedAt: now.Add(-30 * day)}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, report := range append(append(append(append([]report{}, reports[reaped.contractId]...), reports[open.contractId]...), reports[young.contractId]...), late) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report_evidence
					(client_id,report_id,contract_id,party,acked_byte_count,unacked_byte_count,checkpoint,accepted_at)
					VALUES($1,$2,$3,$4,0,0,true,$5)`,
					report.clientId, report.reportId, report.contractId, report.party, report.acceptedAt.UTC()))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		evidence := func(contractId server.Id) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_close_report_evidence WHERE contract_id=$1`, contractId).Scan(&count))
			})
			return
		}
		contractPresent := func(contract *horizonTestContract) bool {
			rows, _ := countHorizonTestRows(t, ctx, contract.contractId)
			return rows[0] == 1
		}
		removeEvidence := func() int64 {
			report, _ := server.MaintainContractPartitions(WithContractHorizonControl(ctx), server.NowUtc(), server.ContractRetentionCursor{}, time.Minute)
			return report.EvidenceRemoved
		}
		reap := func() {
			if _, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings()); err != nil {
				t.Fatal(err)
			}
		}

		// the code's 91 days, for both
		removeHorizonTestControl(t, ctx)
		if retention := server.CloseEvidenceRetention(WithContractHorizonControl(ctx)); retention != server.ContractRetention {
			t.Fatalf("without the control the evidence keeps %s", retention)
		}
		reap()
		if removed := removeEvidence(); removed != 0 || !contractPresent(reaped) || evidence(reaped.contractId) != 2 {
			t.Fatalf("the code's horizon removed %d evidence rows, or the 60-day contract", removed)
		}

		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldHorizonDays: "45"})
		if retention := server.CloseEvidenceRetention(WithContractHorizonControl(ctx)); retention != 45*day {
			t.Fatalf("at horizonDays 45 the evidence keeps %s", retention)
		}
		// a control read that fails keeps the code's 91 days
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if retention := server.CloseEvidenceRetention(WithContractHorizonControl(canceled)); retention != server.ContractRetention {
			t.Fatalf("an unreadable control keeps the evidence %s", retention)
		}
		reap()
		if contractPresent(reaped) || !contractPresent(open) || !contractPresent(young) {
			t.Fatal("the reaper did not delete exactly the 60-day settled contract")
		}
		// the reaped contract's reports are orphans now
		if evidence(reaped.contractId) != 2 {
			t.Fatal("the reaper deleted close evidence")
		}
		if removed := removeEvidence(); removed != 2 {
			t.Fatalf("the evidence pass removed %d rows, want the reaped contract's 2", removed)
		}
		if evidence(reaped.contractId) != 0 {
			t.Fatal("the reaped contract's evidence outlived it")
		}
		// an unresolved contract's evidence and every report inside the
		// horizon stay: admission and the census read them
		if evidence(open.contractId) != 3 || evidence(young.contractId) != 2 {
			t.Fatalf("evidence a reader needs was removed: open %d, young %d", evidence(open.contractId), evidence(young.contractId))
		}
		// a second pass finds nothing
		if removed := removeEvidence(); removed != 0 {
			t.Fatalf("a second pass removed %d rows", removed)
		}
	})
}
