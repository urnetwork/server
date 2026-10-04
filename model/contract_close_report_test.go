// Real database transactions expose retry, collision, rollback and terminal custody.
package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Contracts and directory identities are synthetic; work is admitted only by production paths.
type closeReportFixture struct {
	ctx             context.Context
	contractId      server.Id
	otherContractId server.Id
	sourceId        server.Id
	destinationId   server.Id
}

// A second contract gives changed-identity reuse an otherwise valid target.
func newCloseReportFixture(t testing.TB) *closeReportFixture {
	t.Helper()
	ctx := t.Context()
	networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{sourceId: networkId, destinationId: networkId})
	contractId, err := CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 1000)
	if err != nil {
		t.Fatal(err)
	}
	otherContractId, err := CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return &closeReportFixture{ctx: ctx, contractId: contractId, otherContractId: otherContractId, sourceId: sourceId, destinationId: destinationId}
}

// Every report differs in identity unless a test deliberately retains it for replay.
func (self *closeReportFixture) report() ContractCloseReport {
	return ContractCloseReport{ReportId: server.NewId(), ContractId: self.contractId, ClientId: self.sourceId, AckedByteCount: 20, UnackedByteCount: 7, Checkpoint: true}
}

// The durable census is independent of the API's returned admission flag.
func assertCloseReportCounts(t testing.TB, ctx context.Context, contractId server.Id, reports int, used ByteCount) {
	t.Helper()
	var actualReports int
	var actualUsed ByteCount
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_close_report_evidence WHERE contract_id=$1`, contractId).Scan(&actualReports))
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(sum(used_transfer_byte_count),0) FROM contract_close WHERE contract_id=$1`, contractId).Scan(&actualUsed))
	})
	if actualReports != reports || actualUsed != used {
		t.Fatalf("original report/byte census changed: reports=%d bytes=%d want=%d/%d", actualReports, actualUsed, reports, used)
	}
}

// Exact retries never add bytes; a separate equal-byte report remains independent work.
func TestContractCloseReportDuplicateAndEqualWorkRemainDistinct(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		for index, want := range []bool{true, false, false} {
			applied, err := CloseContractWithReport(f.ctx, report)
			if err != nil || applied != want {
				t.Fatalf("exact close retry %d did not converge once: %v %v", index, applied, err)
			}
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
		report.ReportId = server.NewId()
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("independent equal-byte report was collapsed", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 2, 40)
	})
}

// Every retained content dimension is checked before any new effect or terminal shortcut.
func TestContractCloseReportChangedContentRefuses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		if _, err := CloseContractWithReport(f.ctx, report); err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(*ContractCloseReport){
			func(r *ContractCloseReport) { r.ContractId = f.otherContractId },
			func(r *ContractCloseReport) { r.AckedByteCount++ },
			func(r *ContractCloseReport) { r.UnackedByteCount++ },
			func(r *ContractCloseReport) { r.Checkpoint = false },
		} {
			changed := report
			change(&changed)
			applied, err := CloseContractWithReport(f.ctx, changed)
			if applied || !errors.Is(err, ErrContractCloseReportConflict) {
				t.Fatal("changed original report was accepted", changed, applied, err)
			}
			assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
			assertCloseReportCounts(t, f.ctx, f.otherContractId, 0, 0)
		}
	})
}

// A report identity is scoped by authenticated client; it never supplies party authority.
func TestContractCloseReportOwnerAndFullUnackedDomain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		report.UnackedByteCount = ^uint64(0)
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("full original unacked value was not retained", applied, err)
		}
		foreign := report
		foreign.ClientId = server.NewId()
		if applied, err := CloseContractWithReport(f.ctx, foreign); applied || err == nil || !strings.Contains(err.Error(), "Client is not a party") {
			t.Fatal("foreign report owner borrowed admitted authority", applied, err)
		}
		other := report
		other.ClientId = f.destinationId
		if applied, err := CloseContractWithReport(f.ctx, other); err != nil || !applied {
			t.Fatal("another authenticated owner lost its independent report namespace", applied, err)
		}
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || applied {
			t.Fatal("full uint64 original did not round trip", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 2, 40)
	})
}

// Observe the actual database lock, not a sleep or scheduling guess, before releasing the first transaction.
func waitCloseReportDatabaseConflict(t testing.TB, ctx context.Context, tx server.PgTx, waitingPid, ownerPid int) {
	t.Helper()
	for {
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1))`, waitingPid, ownerPid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("report contender did not reach the retained database lock", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

// Both attempts overlap while the first original is uncommitted. The second sees
// either the contract-row lock or the owner/report unique constraint, as applicable.
func exerciseCloseReportConcurrent(t testing.TB, changedContract, cancelContender bool) {
	t.Helper()
	f := newCloseReportFixture(t)
	first := f.report()
	second := first
	if changedContract {
		second.ContractId = f.otherContractId
	}
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Minute)
	defer cancel()
	type result struct {
		applied bool
		err     error
	}
	ready := make(chan int, 1)
	finished := make(chan result, 1)
	contenderCtx, cancelWaiting := context.WithCancel(ctx)
	defer cancelWaiting()
	joined := make(chan struct{})
	started := false
	defer func() {
		cancelWaiting()
		if started {
			select {
			case <-joined:
			case <-time.After(time.Minute):
				t.Error("close-report contender did not join canceled cleanup")
			}
		}
	}()
	var canceledResult *result
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, tx)
		var ownerPid int
		server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPid))
		applied, err := closeContractReportInTx(ctx, tx, first)
		if err != nil || !applied {
			t.Fatal("first original did not enter its owning transaction", applied, err)
		}
		started = true
		go func() {
			defer close(joined)
			var observed result
			server.HandleError(func() {
				server.Db(contenderCtx, func(other server.PgConn) {
					otherTx, err := other.BeginTx(contenderCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
					server.Raise(err)
					defer rollbackCloseReportTestTransaction(ctx, otherTx)
					var pid int
					server.Raise(otherTx.QueryRow(contenderCtx, `SELECT pg_backend_pid()`).Scan(&pid))
					ready <- pid
					observed.applied, observed.err = closeContractReportInTx(contenderCtx, otherTx, second)
					if observed.err == nil {
						observed.err = otherTx.Commit(contenderCtx)
					}
				})
			}, func(err error) { observed.err = err })
			finished <- observed
		}()
		var contenderPid int
		select {
		case contenderPid = <-ready:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		waitCloseReportDatabaseConflict(t, ctx, tx, contenderPid, ownerPid)
		if cancelContender {
			cancelWaiting()
			select {
			case value := <-finished:
				canceledResult = &value
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		server.Raise(tx.Commit(ctx))
	})
	var observed result
	if canceledResult != nil {
		observed = *canceledResult
	} else {
		select {
		case observed = <-finished:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if cancelContender {
		if observed.applied || (!errors.Is(observed.err, context.Canceled) && !server.IsDoneError(observed.err)) {
			t.Fatal("blocked report cancellation changed or lost original authority", observed.applied, observed.err)
		}
	} else if observed.applied || (changedContract && !errors.Is(observed.err, ErrContractCloseReportConflict)) || (!changedContract && observed.err != nil) {
		t.Fatal("concurrent original report did not converge atomically", observed.applied, observed.err)
	}
	assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	assertCloseReportCounts(t, f.ctx, f.otherContractId, 0, 0)
	if applied, err := CloseContractWithReport(f.ctx, first); applied || err != nil {
		t.Fatal("committed original did not survive a fresh public request", applied, err)
	}
}

// Lock-contended identical checkpoint delivery commits exactly one increment.
func TestContractCloseReportConcurrentDuplicateCommitsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseCloseReportConcurrent(t, false, false) })
}

// Same owner/report id against another real contract is fenced by durable uniqueness.
func TestContractCloseReportConcurrentIdentityConflictHasNoEffect(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseCloseReportConcurrent(t, true, false) })
}

// A body failure after reserving the identity must roll back both the identity and increment.
func TestContractCloseReportPublicRollbackThenRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE FUNCTION synthetic_report_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic report accumulation rollback'; END $$;
   CREATE TRIGGER synthetic_report_abort BEFORE INSERT OR UPDATE ON contract_close FOR EACH ROW EXECUTE FUNCTION synthetic_report_abort()`))
		})
		var refusal error
		server.HandleError(func() { _, refusal = CloseContractWithReport(f.ctx, report) }, func(err error) { refusal = err })
		if refusal == nil || !strings.Contains(refusal.Error(), "synthetic report accumulation rollback") {
			t.Fatal("public report did not reach rollback after reservation", refusal)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `DROP TRIGGER synthetic_report_abort ON contract_close; DROP FUNCTION synthetic_report_abort()`))
		})
		if applied, err := CloseContractWithReport(f.ctx, report); !applied || err != nil {
			t.Fatal("rolled-back report could not retry", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// Losing the post-commit reply cannot lose settlement or add work at restart.
func TestContractCloseReportCommittedRetryFinishesSettlementAndSurvivesCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		first := f.report()
		first.Checkpoint = false
		second := first
		second.ReportId = server.NewId()
		second.ClientId = f.destinationId
		for _, report := range []ContractCloseReport{first, second} {
			server.Tx(f.ctx, func(tx server.PgTx) {
				applied, err := closeContractReportInTx(f.ctx, tx, report)
				server.Raise(err)
				if !applied {
					t.Fatal("original terminal report did not commit before lost reply")
				}
			}, server.TxReadCommitted)
		}
		if applied, err := CloseContractWithReport(f.ctx, second); applied || err != nil {
			t.Fatal("retained terminal retry did not finish original settlement", applied, err)
		}
		var original []byte
		server.Db(f.ctx, func(conn server.PgConn) {
			var outcome *ContractOutcome
			server.Raise(conn.QueryRow(f.ctx, `SELECT outcome,provider_usage FROM transfer_contract WHERE contract_id=$1`, f.contractId).Scan(&outcome, &original))
			if outcome == nil || *outcome != ContractOutcomeSettled || len(original) == 0 {
				t.Fatal("retained retry did not preserve completed-work proof", outcome)
			}
		})
		// The installed retention trigger moves original provider usage to its archive.
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, f.contractId))
			server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM network_client WHERE client_id=ANY($1::uuid[])`, []server.Id{f.sourceId, f.destinationId}))
		})
		reopened, cancel := context.WithCancel(t.Context())
		defer cancel()
		for _, report := range []ContractCloseReport{first, second} {
			if applied, err := CloseContractWithReport(reopened, report); applied || err != nil {
				t.Fatal("terminal retry lost retained identity after cleanup/reopen", applied, err)
			}
		}
		changed := second
		changed.AckedByteCount++
		if applied, err := CloseContractWithReport(reopened, changed); applied || !errors.Is(err, ErrContractCloseReportConflict) {
			t.Fatal("cleanup waived original content agreement", applied, err)
		}
		var retained int
		server.Db(reopened, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(reopened, `SELECT count(*) FROM contract_close_report_evidence WHERE contract_id=$1`, f.contractId).Scan(&retained))
		})
		if retained != 2 {
			t.Fatal("billing cleanup erased original report identity", retained)
		}
	})
}

// New reports cannot extend a terminal party; rejecting one must retire no fresh report id.
func TestContractCloseReportNewTerminalReportRollsBackIdentity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		report.Checkpoint = false
		if _, err := CloseContractWithReport(f.ctx, report); err != nil {
			t.Fatal(err)
		}
		report.ReportId = server.NewId()
		if applied, err := CloseContractWithReport(f.ctx, report); applied || !errors.Is(err, ErrContractCloseReportClosed) {
			t.Fatal("new terminal-party report was admitted", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// Cancellation at ingress changes no facts, and a healthy owner can reuse that exact identity.
func TestContractCloseReportCanceledOwnerRecoversWithoutPartialIdentity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		if applied, err := CloseContractWithReport(ctx, report); applied || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled report lost owner or admitted work", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
		if applied, err := CloseContractWithReport(f.ctx, report); !applied || err != nil {
			t.Fatal("healthy original report did not recover", applied, err)
		}
	})
}

// SQL custody survives a new writer attempting to rewrite, delete or truncate originals.
func TestContractCloseReportImmutableDatabaseCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		if _, err := CloseContractWithReport(f.ctx, report); err != nil {
			t.Fatal(err)
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			for _, sql := range []string{`UPDATE contract_close_report_evidence SET acked_byte_count=acked_byte_count+1`, `DELETE FROM contract_close_report_evidence`, `TRUNCATE contract_close_report_evidence`} {
				if _, err := conn.Exec(f.ctx, sql); err == nil || !strings.Contains(err.Error(), "original contract close reports are immutable") {
					t.Fatal("original report custody was mutable", sql, err)
				}
			}
		}, server.OptReadWrite())
		if applied, err := CloseContractWithReport(f.ctx, report); applied || err != nil {
			t.Fatal("immutable original did not survive failed mutation", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// Report custody applies equally to paid, same-network and reverse companion contracts.
// Billing direction stays separate from the immutable completed-work denominator.
func TestContractCloseReportPaidFreeAndCompanionUseExactCompletedBytes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := server.NowUtc()
		payerNetwork, providerNetwork, freeNetwork := server.NewId(), server.NewId(), server.NewId()
		payer, provider, freeOrigin, freeProvider := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{payer: payerNetwork, provider: providerNetwork, freeOrigin: freeNetwork, freeProvider: freeNetwork})
		addContractPayoutTestBalance(ctx, payerNetwork, 242)
		paid, err := CreateTransferEscrow(ctx, payerNetwork, payer, providerNetwork, provider, 121)
		if err != nil {
			t.Fatal(err)
		}
		streamId := AddToStream(ctx, paid.ContractId, payer, provider, nil)
		companion, err := CreateCompanionTransferEscrow(ctx, providerNetwork, provider, payerNetwork, payer, 121, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		joined, ok := AddCompanionContractToStream(ctx, companion.ContractId, paid.ContractId, provider, payer)
		if !ok || joined != streamId {
			t.Fatal("original companion stream was not retained")
		}
		free, err := CreateContractNoEscrow(ctx, freeNetwork, freeOrigin, freeNetwork, freeProvider, 121)
		if err != nil {
			t.Fatal(err)
		}
		for _, contract := range []struct{ id, source, destination server.Id }{
			{id: paid.ContractId, source: payer, destination: provider},
			{id: companion.ContractId, source: provider, destination: payer},
			{id: free, source: freeOrigin, destination: freeProvider},
		} {
			for _, clientId := range []server.Id{contract.source, contract.destination} {
				for _, step := range []struct {
					count      ByteCount
					checkpoint bool
				}{{count: 20, checkpoint: true}, {count: 101, checkpoint: false}} {
					report := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.id, ClientId: clientId, AckedByteCount: step.count, UnackedByteCount: 13, Checkpoint: step.checkpoint}
					if applied, err := CloseContractWithReport(ctx, report); err != nil || !applied {
						t.Fatal("original paid/free/companion report failed", applied, err)
					}
					if applied, err := CloseContractWithReport(ctx, report); err != nil || applied {
						t.Fatal("paid/free/companion retry changed original work", applied, err)
					}
				}
			}
			assertCloseReportCounts(t, ctx, contract.id, 4, 242)
		}
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		got := map[server.Id]int64{}
		for _, usage := range usages {
			got[usage.ClientId] = usage.PayoutByteCount
		}
		if len(got) != 2 || got[provider] != 242 || got[freeProvider] != 121 {
			t.Fatal("report custody changed paid/free/companion completed work", got)
		}
	})
}

// A canceled contender is observed at the actual database lock before the first owner commits.
func TestContractCloseReportCanceledBlockedOwnerLeavesOriginalUntouched(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseCloseReportConcurrent(t, true, true) })
}
