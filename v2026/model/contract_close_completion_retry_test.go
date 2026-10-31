// Durable admission ordering keeps report settlement resumable without guessing
// cross-party order from clocks, identities, or an id-less retransmission.
package model

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// A public report stops after admission committed and before settlement begins.
func interruptContractCompletionTestReport(t testing.TB, ctx context.Context, report ContractCloseReport) {
	t.Helper()
	interrupted := errors.New("synthetic stop after report admission")
	reportCtx := context.WithValue(ctx, contractSettlementTxTestKey{}, func(server.Id) { panic(interrupted) })
	var stopped any
	func() {
		defer func() { stopped = recover() }()
		if _, err := CloseContractWithReport(reportCtx, report); err != nil {
			t.Fatal(err)
		}
	}()
	if stopped != interrupted {
		t.Fatalf("report did not reach the admission/settlement boundary: %v", stopped)
	}
}

// Reads durable outcome and debit custody independently of the API result.
func readContractCompletionTestState(t testing.TB, ctx context.Context, contractId server.Id) (outcome *ContractOutcome, journals int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT outcome,
			(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1)
			FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&outcome, &journals))
	})
	return
}

// The production Redis admission and public report owners expose both the lost
// continuation and a settlement rollback, with no scheduler-dependent timing.
func exerciseContractCompletionRetry(t *testing.T, completion contractCheckpointCompletion, busySettlement bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		report := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId}
		prior := report
		prior.ReportId = server.NewId()
		switch completion {
		case contractSenderFinalAfterCheckpoint:
			prior.ClientId, prior.AckedByteCount, prior.Checkpoint = f.destinationId, 9, true
			report.AckedByteCount = 11
		case contractCheckpointAfterSenderFinal:
			prior.AckedByteCount = 11
			report.ClientId, report.AckedByteCount, report.Checkpoint = f.destinationId, 11, true
		}
		if completion != contractSenderZeroFinalUnreported {
			if _, err := CloseContractWithReport(ctx, prior); err != nil {
				t.Fatal(err)
			}
		}
		if busySettlement {
			release := holdFinancialTestOwner(t, ctx, []server.PgOwnershipKey{
				task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contract.ContractId)),
			})
			applied, err := CloseContractWithReport(ctx, report)
			release()
			if !applied || !errors.Is(err, errTransferBalanceOwnershipBusy) {
				t.Fatalf("settlement did not roll back after admission: applied=%t err=%v", applied, err)
			}
		} else {
			interruptContractCompletionTestReport(t, ctx, report)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
			t.Fatalf("interrupted settlement became terminal: outcome=%v journals=%d", outcome, journals)
		}
		if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 40 {
			t.Fatalf("fixture lost its pending reservation: %d", reserved)
		}
		if applied, err := CloseContractWithReport(ctx, report); applied || err != nil {
			t.Fatalf("exact retry changed report custody: applied=%t err=%v", applied, err)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome == nil || *outcome != ContractOutcomeSettled || journals != 1 {
			t.Fatalf("exact retry did not finish settlement: outcome=%v journals=%d", outcome, journals)
		}
		flushRedisAdmissionTestDebits(t, ctx, f.balanceId, 1, 1)
		if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 0 {
			t.Fatalf("resumed settlement left reservation=%d", reserved)
		}
	})
}

// A zero acknowledged-byte sender final retains its accepted completion policy.
func TestContractCompletionRetrySenderZeroFinal(t *testing.T) {
	exerciseContractCompletionRetry(t, contractSenderZeroFinalUnreported, false)
}

// The sender-after-receiver decision permits a receiver below the exact sender.
func TestContractCompletionRetrySenderAfterCheckpoint(t *testing.T) {
	exerciseContractCompletionRetry(t, contractSenderFinalAfterCheckpoint, false)
}

// Decision B also survives interruption once its actual ordering was admitted.
func TestContractCompletionRetryReceiverAfterSender(t *testing.T) {
	exerciseContractCompletionRetry(t, contractCheckpointAfterSenderFinal, false)
}

// A failed settlement rolls back its synthetic receiver final, but not the
// already-committed proof needed by the exact sender retry.
func TestContractCompletionRetryZeroFinalAfterSettlementRollback(t *testing.T) {
	exerciseContractCompletionRetry(t, contractSenderZeroFinalUnreported, true)
}

// A receiver checkpoint remains resumable after a busy settlement owner.
func TestContractCompletionRetryCheckpointAfterSettlementRollback(t *testing.T) {
	exerciseContractCompletionRetry(t, contractSenderFinalAfterCheckpoint, true)
}

// Decision B's receiver-below-sender refusal must not become decision A just
// because an exact retry sees both parties in the aggregate.
func TestContractCompletionRetryPreservesReceiverAfterSenderGuard(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		source := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId, AckedByteCount: 11}
		destination := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 9, Checkpoint: true}
		for _, report := range []ContractCloseReport{source, destination, destination, source} {
			if _, err := CloseContractWithReport(ctx, report); err != nil {
				t.Fatal(err)
			}
			if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
				t.Fatalf("retry broadened decision B: outcome=%v journals=%d", outcome, journals)
			}
		}
		destination.ReportId, destination.AckedByteCount = server.NewId(), 2
		if _, err := CloseContractWithReport(ctx, destination); err != nil {
			t.Fatal(err)
		}
		if outcome, _ := readContractCompletionTestState(t, ctx, contract.ContractId); outcome == nil || *outcome != ContractOutcomeSettled {
			t.Fatal("a covering receiver report did not settle")
		}
	})
}

// Rolling older writers omit the new column. Neither their counts nor an
// id-less replay prove which report followed the other; a new increment does.
func TestContractCompletionRetryLegacyWriterNeedsOrderingProof(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,checkpoint,legacy_checkpoint_byte_count,identified_checkpoint_byte_count)
				VALUES($1,$2,11,true,11,0),($1,$3,11,false,NULL,0)`, contract.ContractId, ContractPartyDestination, ContractPartySource))
		})
		if added, err := CloseContractUsage(ctx, contract.ContractId, f.destinationId, 11, true); added != 0 || err != nil {
			t.Fatalf("legacy replay changed accounting: added=%d err=%v", added, err)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
			t.Fatalf("legacy rows acquired inferred ordering: outcome=%v journals=%d", outcome, journals)
		}
		if added, err := CloseContractUsage(ctx, contract.ContractId, f.destinationId, 12, true); added != 1 || err != nil {
			t.Fatalf("new legacy increment was refused: added=%d err=%v", added, err)
		}
		if outcome, _ := readContractCompletionTestState(t, ctx, contract.ContractId); outcome == nil || *outcome != ContractOutcomeSettled {
			t.Fatal("newly proven receiver ordering did not settle")
		}
	})
}

// The signed fast retry returns before BEGIN; aggregate proof still resumes the
// settlement whose first caller disappeared after its admission commit.
func TestContractCompletionRetrySignedZeroFinal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, _, key := originalCloseAvailabilityFixture(t)
		domain, _ := input.Domain.Digest()
		report := f.report()
		report.AckedByteCount, report.Checkpoint = 0, false
		report = signCloseReportOriginal(t, report, domain, key)
		interruptContractCompletionTestReport(t, f.ctx, report)
		if applied, err := CloseContractWithReport(f.ctx, report); applied || err != nil {
			t.Fatalf("signed retry changed custody: applied=%t err=%v", applied, err)
		}
		if outcome, _ := readContractCompletionTestState(t, f.ctx, f.contractId); outcome == nil || *outcome != ContractOutcomeSettled {
			t.Fatal("signed retained-report retry did not settle")
		}
	})
}

// Retained admission proof never takes a contract back from the expiry owner.
func TestContractCompletionRetryRespectsStartedExpiry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		report := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId}
		interruptContractCompletionTestReport(t, ctx, report)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_unverified=true WHERE contract_id=$1`, contract.ContractId))
		})
		if applied, err := CloseContractWithReport(ctx, report); applied || err != nil {
			t.Fatalf("retained retry failed: applied=%t err=%v", applied, err)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
			t.Fatalf("report retry took expiry ownership: outcome=%v journals=%d", outcome, journals)
		}
	})
}

// The legacy and rolling receipt APIs share admission ordering with the
// original-report API and must resume its separately committed settlement too.
func exerciseLegacyContractCompletionRetry(t *testing.T, identified bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		if err := CloseContract(ctx, contract.ContractId, f.destinationId, 9, true); err != nil {
			t.Fatal(err)
		}
		reportId := server.NewId()
		report := func(ctx context.Context) error {
			if identified {
				_, err := CloseContractReport(ctx, contract.ContractId, f.sourceId, 11, false, reportId)
				return err
			}
			return CloseContract(ctx, contract.ContractId, f.sourceId, 11, false)
		}
		interrupted := errors.New("synthetic stop after legacy admission")
		reportCtx := context.WithValue(ctx, contractSettlementTxTestKey{}, func(server.Id) { panic(interrupted) })
		var stopped any
		func() {
			defer func() { stopped = recover() }()
			if err := report(reportCtx); err != nil {
				t.Fatal(err)
			}
		}()
		if stopped != interrupted {
			t.Fatalf("legacy report did not stop after admission: %v", stopped)
		}
		if err := report(ctx); err != nil {
			t.Fatal(err)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome == nil || *outcome != ContractOutcomeSettled || journals != 1 {
			t.Fatalf("legacy retry did not settle: outcome=%v journals=%d", outcome, journals)
		}
		flushRedisAdmissionTestDebits(t, ctx, f.balanceId, 1, 1)
	})
}

// An id-less final applies once, and its admitted source ordering is durable.
func TestContractCompletionRetryLegacyFinal(t *testing.T) {
	exerciseLegacyContractCompletionRetry(t, false)
}

// Rolling receipt retries preserve their original report identity and decision.
func TestContractCompletionRetryRollingIdentifiedFinal(t *testing.T) {
	exerciseLegacyContractCompletionRetry(t, true)
}

// A checkpoint admitted between the retry's hint and contract lock must be
// checked at its new count before any synthetic final can be committed.
func TestContractCompletionRetryRechecksRacingReceiverCount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		destination := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 9, Checkpoint: true}
		if _, err := CloseContractWithReport(ctx, destination); err != nil {
			t.Fatal(err)
		}
		source := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId, AckedByteCount: 11}
		interruptContractCompletionTestReport(t, ctx, source)
		raced := false
		retryCtx := context.WithValue(ctx, contractSettlementTxTestKey{}, func(server.Id) {
			raced = true
			destination.ReportId, destination.AckedByteCount = server.NewId(), AcceptableTransfersByteDifference+3
			if _, err := CloseContractWithReport(ctx, destination); err != nil {
				t.Fatal(err)
			}
		})
		if applied, err := CloseContractWithReport(retryCtx, source); applied || err != nil {
			t.Fatalf("retry changed custody: applied=%t err=%v", applied, err)
		}
		if !raced {
			t.Fatal("retry never reached the settlement lock boundary")
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
			t.Fatalf("stale hint settled incompatible counts: outcome=%v journals=%d", outcome, journals)
		}
	})
}

// A receiver admitted after the retry's zero-final hint supplies decision B;
// that same retry completes it using the current locked state.
func TestContractCompletionRetryRechecksRacingReceiverAdmission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		source := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId}
		interruptContractCompletionTestReport(t, ctx, source)
		raced := false
		retryCtx := context.WithValue(ctx, contractSettlementTxTestKey{}, func(server.Id) {
			raced = true
			destination := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 6, Checkpoint: true}
			interruptContractCompletionTestReport(t, ctx, destination)
		})
		if applied, err := CloseContractWithReport(retryCtx, source); applied || err != nil {
			t.Fatalf("retry changed custody: applied=%t err=%v", applied, err)
		}
		if !raced {
			t.Fatal("retry never reached the settlement lock boundary")
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome == nil || *outcome != ContractOutcomeSettled || journals != 1 {
			t.Fatalf("retry ignored receiver admitted before its lock: outcome=%v journals=%d", outcome, journals)
		}
		flushRedisAdmissionTestDebits(t, ctx, f.balanceId, 1, 1)
	})
}

// A receiver final committed while the sender's admitted call was suspended
// restores ordinary two-final settlement despite its earlier zero-only mode.
func TestContractCompletionRetryTwoFinalsOverrideZeroDecision(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
			interruptContractCompletionTestReport(t, ctx, ContractCloseReport{
				ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: clientId,
			})
		}
		closed, completed, err := settleReportedContract(ctx, contract.ContractId, contractSenderZeroFinalUnreported)
		if !closed || completed || err != nil {
			t.Fatalf("admitted sender decision suppressed two finals: closed=%t completed=%t err=%v", closed, completed, err)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome == nil || *outcome != ContractOutcomeSettled || journals != 1 {
			t.Fatalf("two finals did not settle once: outcome=%v journals=%d", outcome, journals)
		}
		flushRedisAdmissionTestDebits(t, ctx, f.balanceId, 1, 1)
	})
}

// A receiver report after the final supersedes the earlier sender decision;
// retries must keep decision B's refusal of a receiver below the sender.
func TestContractCompletionRetryUsesLatestReceiverDecision(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		destination := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 9, Checkpoint: true}
		if _, err := CloseContractWithReport(ctx, destination); err != nil {
			t.Fatal(err)
		}
		source := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId, AckedByteCount: 11}
		interruptContractCompletionTestReport(t, ctx, source)
		destination.ReportId, destination.AckedByteCount = server.NewId(), 1
		for _, report := range []ContractCloseReport{destination, destination, source} {
			if _, err := CloseContractWithReport(ctx, report); err != nil {
				t.Fatal(err)
			}
			if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
				t.Fatalf("stale sender decision broadened receiver decision B: outcome=%v journals=%d", outcome, journals)
			}
		}
		destination.ReportId = server.NewId()
		if _, err := CloseContractWithReport(ctx, destination); err != nil {
			t.Fatal(err)
		}
		if outcome, _ := readContractCompletionTestState(t, ctx, contract.ContractId); outcome == nil || *outcome != ContractOutcomeSettled {
			t.Fatal("covering receiver report did not settle")
		}
		flushRedisAdmissionTestDebits(t, ctx, f.balanceId, 1, 1)
	})
}

// The source call's in-memory decision is a hint too: a receiver report that
// commits before its settlement lock must govern the current aggregate.
func TestContractCompletionRetryFreshSenderDecisionRechecksReceiver(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		destination := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 9, Checkpoint: true}
		if _, err := CloseContractWithReport(ctx, destination); err != nil {
			t.Fatal(err)
		}
		raced := false
		reportCtx := context.WithValue(ctx, contractSettlementTxTestKey{}, func(server.Id) {
			raced = true
			destination.ReportId, destination.AckedByteCount = server.NewId(), 1
			if _, err := CloseContractWithReport(ctx, destination); err != nil {
				t.Fatal(err)
			}
		})
		source := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId, AckedByteCount: 11}
		if applied, err := CloseContractWithReport(reportCtx, source); !applied || err != nil {
			t.Fatalf("source admission failed: applied=%t err=%v", applied, err)
		}
		if !raced {
			t.Fatal("source call never reached its settlement lock boundary")
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
			t.Fatalf("in-memory sender decision overrode newer receiver: outcome=%v journals=%d", outcome, journals)
		}
	})
}

// An older writer can change the receiver aggregate without the new mode.
// Retained sender eligibility applies only to the checkpoint it admitted.
func TestContractCompletionRetryOldWriterChangesReceiverState(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		destination := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 9, Checkpoint: true}
		if _, err := CloseContractWithReport(ctx, destination); err != nil {
			t.Fatal(err)
		}
		source := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId, AckedByteCount: 11}
		interruptContractCompletionTestReport(t, ctx, source)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, contract.ContractId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close
				SET used_transfer_byte_count=10,identified_checkpoint_byte_count=10
				WHERE contract_id=$1 AND party=$2 AND checkpoint`, contract.ContractId, ContractPartyDestination))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report
				(contract_id,party,report_id,used_transfer_byte_count,checkpoint)
				VALUES($1,$2,$3,1,true)`, contract.ContractId, ContractPartyDestination, server.NewId()))
		})
		if applied, err := CloseContractWithReport(ctx, source); applied || err != nil {
			t.Fatalf("sender retry changed custody: applied=%t err=%v", applied, err)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
			t.Fatalf("old-writer update reused a different checkpoint's decision: outcome=%v journals=%d", outcome, journals)
		}
	})
}

// An identified zero increment is still a newer receiver decision. Its count
// matches the sender witness, so decision B itself must supersede decision A.
func TestContractCompletionRetryZeroReceiverDecisionSupersedesSender(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		destination := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 9, Checkpoint: true}
		if _, err := CloseContractWithReport(ctx, destination); err != nil {
			t.Fatal(err)
		}
		source := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId, AckedByteCount: 11}
		interruptContractCompletionTestReport(t, ctx, source)
		destination.ReportId, destination.AckedByteCount = server.NewId(), 0
		for _, report := range []ContractCloseReport{destination, destination, source} {
			if _, err := CloseContractWithReport(ctx, report); err != nil {
				t.Fatal(err)
			}
			if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
				t.Fatalf("sender witness overrode newer zero receiver decision: outcome=%v journals=%d", outcome, journals)
			}
		}
	})
}

// Missing witness data cannot establish which receiver checkpoint was present
// when an older source decision was admitted.
func TestContractCompletionRetrySenderDecisionNeedsCheckpointWitness(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		destination := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 9, Checkpoint: true}
		if _, err := CloseContractWithReport(ctx, destination); err != nil {
			t.Fatal(err)
		}
		source := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId, AckedByteCount: 11}
		interruptContractCompletionTestReport(t, ctx, source)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close
				SET settlement_completion_checkpoint_byte_count=NULL
				WHERE contract_id=$1 AND party=$2`, contract.ContractId, ContractPartySource))
		})
		if applied, err := CloseContractWithReport(ctx, source); applied || err != nil {
			t.Fatalf("sender retry changed custody: applied=%t err=%v", applied, err)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
			t.Fatalf("sender decision inferred missing checkpoint witness: outcome=%v journals=%d", outcome, journals)
		}
	})
}

// Client-origin legacy zero finals retain their eligibility before settlement,
// so a retry can release the real Redis reservation after an interruption.
func TestContractCompletionRetryLegacyClientZeroFinal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		interrupted := errors.New("synthetic stop after legacy client admission")
		reportCtx := context.WithValue(ctx, contractSettlementTxTestKey{}, func(server.Id) { panic(interrupted) })
		var stopped any
		func() {
			defer func() { stopped = recover() }()
			if added, err := CloseClientContractUsage(reportCtx, contract.ContractId, f.sourceId, 0, false); added != 0 || err != nil {
				t.Fatalf("legacy client admission failed: added=%d err=%v", added, err)
			}
		}()
		if stopped != interrupted {
			t.Fatalf("legacy client report did not reach settlement: %v", stopped)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
			t.Fatalf("interrupted legacy settlement became terminal: outcome=%v journals=%d", outcome, journals)
		}
		if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 40 {
			t.Fatalf("legacy report lost its pending reservation: %d", reserved)
		}
		if added, err := CloseClientContractUsage(ctx, contract.ContractId, f.sourceId, 0, false); added != 0 || err != nil {
			t.Fatalf("legacy client retry changed its count: added=%d err=%v", added, err)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome == nil || *outcome != ContractOutcomeSettled || journals != 1 {
			t.Fatalf("legacy client retry did not settle: outcome=%v journals=%d", outcome, journals)
		}
		flushRedisAdmissionTestDebits(t, ctx, f.balanceId, 1, 1)
		if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 0 {
			t.Fatalf("legacy client settlement left reservation=%d", reserved)
		}
	})
}

// An internal zero final retains no client-origin proof. Replaying that final
// through either API cannot create eligibility after its admission.
func TestContractCompletionRetryInternalZeroFinalNeedsReceiver(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 40)
		settlementAttempts := 0
		reportCtx := context.WithValue(ctx, contractSettlementTxTestKey{}, func(server.Id) { settlementAttempts++ })
		for range 2 {
			if err := CloseContract(reportCtx, contract.ContractId, f.sourceId, 0, false); err != nil {
				t.Fatal(err)
			}
		}
		if added, err := CloseClientContractUsage(reportCtx, contract.ContractId, f.sourceId, 0, false); added != 0 || err != nil {
			t.Fatalf("client replay changed an internal final: added=%d err=%v", added, err)
		}
		if settlementAttempts != 0 {
			t.Fatalf("internal final authorized %d settlement attempts", settlementAttempts)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome != nil || journals != 0 {
			t.Fatalf("internal zero final settled without its receiver: outcome=%v journals=%d", outcome, journals)
		}
		if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 40 {
			t.Fatalf("internal final lost its pending reservation: %d", reserved)
		}
		if err := CloseContract(ctx, contract.ContractId, f.destinationId, 0, false); err != nil {
			t.Fatal(err)
		}
		if outcome, journals := readContractCompletionTestState(t, ctx, contract.ContractId); outcome == nil || *outcome != ContractOutcomeSettled || journals != 1 {
			t.Fatalf("two internal finals did not settle: outcome=%v journals=%d", outcome, journals)
		}
		flushRedisAdmissionTestDebits(t, ctx, f.balanceId, 1, 1)
	})
}
