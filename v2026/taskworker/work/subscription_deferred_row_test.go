// One contract's operational failure must not pin or park the real expiry task.
package work

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Redis-reserved contracts in the legacy shape that stalled the singleton: no
// stored expiration, created long ago, and only a final zero-byte source report.
// The victim sits among closable siblings; ineligible active rows fill the rest
// of the first 256-row raw page, and one healthy expired tail follows it.
type closeDeferredCohort struct {
	siblingIds   []server.Id
	victimId     server.Id
	protectedIds []server.Id
	tailId       server.Id
}

// Every identity and account is generated inside DefaultTestEnv. Tail-less
// cohorts have no protected rows, so their page holds only the escrowed rows.
func newCloseDeferredCohort(t testing.TB, ctx context.Context, siblingCount int, fillPage bool) *closeDeferredCohort {
	t.Helper()
	payerNetworkId, providerNetworkId := server.NewId(), server.NewId()
	sourceId, destinationId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, payerNetworkId, "synthetic-deferred-payer-"+payerNetworkId.String(), server.NewId())
	model.Testing_CreateNetwork(ctx, providerNetworkId, "synthetic-deferred-provider-"+providerNetworkId.String(), server.NewId())
	server.Tx(ctx, func(tx server.PgTx) {
		for clientId, networkId := range map[server.Id]server.Id{sourceId: payerNetworkId, destinationId: providerNetworkId} {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, active, create_time, auth_time)
				VALUES ($1,$2,true,$3,$3)`, clientId, networkId, server.NowUtc()))
		}
	})
	model.AddBasicTransferBalance(ctx, payerNetworkId, 1024*1024, server.NowUtc(), server.NowUtc().Add(24*time.Hour))
	create := func() server.Id {
		id, _, err := model.CreateContract(ctx, payerNetworkId, sourceId, providerNetworkId, destinationId, 1024)
		if err != nil {
			t.Fatal("synthetic escrowed contract creation failed", err)
		}
		server.Raise(model.CloseContract(ctx, id, sourceId, 0, false))
		return id
	}
	cohort := &closeDeferredCohort{}
	// The victim is second, so earlier and later siblings share its page.
	ordered := make([]server.Id, 0, siblingCount+1)
	for index := range siblingCount + 1 {
		id := create()
		ordered = append(ordered, id)
		if index == 1 || siblingCount == 0 {
			cohort.victimId = id
		} else {
			cohort.siblingIds = append(cohort.siblingIds, id)
		}
	}
	created := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	server.Tx(ctx, func(tx server.PgTx) {
		for index, id := range ordered {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`,
				id, created.Add(time.Duration(index)*time.Millisecond)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, id, created.Add(time.Second)))
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	if !fillPage {
		return cohort
	}
	cohort.tailId = create()
	cohort.protectedIds = make([]server.Id, forceCloseTestRawPageSize-len(ordered))
	for index := range cohort.protectedIds {
		cohort.protectedIds[index] = server.NewId()
	}
	slices.SortFunc(cohort.protectedIds, server.Id.Cmp)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`,
			cohort.tailId, created.Add(2*time.Minute)))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, cohort.tailId, created.Add(2*time.Minute)))
		// A future deadline and fresh checkpoints keep these rows ineligible.
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
			transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
			SELECT id,$2,$3,$2,$4,$2,100,true,$5,$6 FROM unnest($1::uuid[]) row(id)`,
			cohort.protectedIds, payerNetworkId, sourceId, destinationId, created.Add(time.Minute), server.NowUtc().Add(time.Hour)))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
			SELECT id,party,0,$2,true FROM unnest($1::uuid[]) row(id)
			CROSS JOIN (VALUES ('source'),('destination')) parties(party)`, cohort.protectedIds, server.NowUtc()))
	}, server.TxReadCommitted, server.OptNoRetry())
	return cohort
}

// The production raw subpage the expiry scan reads per lane visit.
const forceCloseTestRawPageSize = 256

// Reads the terminal outcome, debit journal count and retained proof of one row.
func readCloseDeferredContract(t testing.TB, ctx context.Context, id server.Id) (terminal bool, journals int, proof bool, intents int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL,
			(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1),
			usage_unverified,
			(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=$1)
			FROM transfer_contract WHERE contract_id=$1`, id).Scan(&terminal, &journals, &proof, &intents))
	})
	return
}

// A failed row must stay open in its original custody: no outcome, debit
// journal, settlement intent or settled reservation was fabricated for it.
func requireCloseDeferredVictimOpen(t testing.TB, ctx context.Context, id server.Id) {
	t.Helper()
	terminal, journals, _, intents := readCloseDeferredContract(t, ctx, id)
	var unsettled int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND NOT settled AND redis_reserved`, id).Scan(&unsettled))
	})
	if terminal || journals != 0 || intents != 0 || unsettled != 1 {
		t.Fatal("the failed row gained a close, debit, intent or released reservation", terminal, journals, intents, unsettled)
	}
}

// Schedules the singleton through its real entry point, then gives it the
// cumulative failure count of a closer already deep in ordinary backoff.
func scheduleCloseDeferredTask(t testing.TB, ctx context.Context, errorCount int) server.Id {
	t.Helper()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	server.Tx(ctx, func(tx server.PgTx) { ScheduleCloseExpiredContracts(owner, tx, 0, false) })
	id := readCloseDeferredTaskId(t, ctx)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error_count=$2 WHERE task_id=$1`, id, errorCount))
	})
	makeCloseRetryTaskDue(ctx, id)
	return id
}

// The singleton has exactly one pending row under its RunOnce key.
func readCloseDeferredTaskId(t testing.TB, ctx context.Context) (id server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE function_name=$1`,
			NewCloseExpiredContractsTaskTarget().TargetFunctionName()).Scan(&id))
	})
	return
}

// Claims, runs and finalizes through the ordinary worker and registered target.
func newCloseDeferredWorker(ctx context.Context) *task.TaskWorker {
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	worker.AddTargets(NewCloseExpiredContractsTaskTarget())
	return worker
}

// The first attempt must commit siblings, persist its advanced cursor and keep
// the explicit failure, while the retry comes at the ordinary scan cadence.
func requireCloseDeferredFirstAttempt(t testing.TB, ctx context.Context, cohort *closeDeferredCohort, worker *task.TaskWorker, id server.Id, cause string) {
	t.Helper()
	_, _, _, originalMetadata, _ := readCloseRetryTask(t, ctx, id)
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != id {
		t.Fatal("deferred row hid its explicit failure or lost the same retry owner", finished, retried, posts, err)
	}
	for _, siblingId := range cohort.siblingIds {
		if terminal, journals, _, _ := readCloseDeferredContract(t, ctx, siblingId); !terminal || journals != 1 {
			t.Fatal("a sibling in the failed row's page did not commit its close", siblingId, terminal, journals)
		}
	}
	requireCloseDeferredVictimOpen(t, ctx, cohort.victimId)
	// Report every violated property of the attempt together.
	args, diagnostic, errorCount, metadata, delay := readCloseRetryTask(t, ctx, id)
	if args.Sweep == nil || args.Sweep.Historical == nil || args.Sweep.Historical.Open == nil ||
		args.Sweep.Historical.Open.ContractId != cohort.protectedIds[len(cohort.protectedIds)-1] {
		t.Error("one failed row pinned the raw cursor; finished progress was discarded", args.Sweep)
	}
	if errorCount != 17 || !strings.Contains(diagnostic, cohort.victimId.String()) || !strings.Contains(diagnostic, cause) ||
		metadata != originalMetadata {
		t.Error("the deferred row was not recorded in the explicit task failure", errorCount)
	}
	if delay < 2*time.Second || 4*time.Second <= delay {
		t.Error("one failed row put the singleton into long backoff instead of continuing", delay)
	}
	if t.Failed() {
		t.FailNow()
	}
	if _, terminal := model.GetContractClose(ctx, cohort.tailId); terminal {
		t.Fatal("the first raw page crossed its 256-row boundary")
	}
}

// The continuation reaches the tail, then the next pass retires the deferred
// row from its retained proof once its operation succeeds.
func requireCloseDeferredRecovery(t testing.TB, ctx context.Context, cohort *closeDeferredCohort, worker *task.TaskWorker, id server.Id, recover func()) {
	t.Helper()
	makeCloseRetryTaskDue(ctx, id)
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
		t.Fatal("the continued scan failed to reach its next raw page", finished, retried, posts, err)
	}
	if terminal, journals, _, _ := readCloseDeferredContract(t, ctx, cohort.tailId); !terminal || journals != 1 {
		t.Fatal("the deferred row starved the healthy tail")
	}
	requireCloseDeferredVictimOpen(t, ctx, cohort.victimId)
	recover()
	nextId := readCloseDeferredTaskId(t, ctx)
	if nextId == id {
		t.Fatal("the completed pass failed to publish a new scan")
	}
	makeCloseRetryTaskDue(ctx, nextId)
	finished, retried, posts, err = worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != nextId || len(retried)+len(posts) != 0 {
		t.Fatal("the next pass did not revisit the deferred row", finished, retried, posts, err)
	}
	if terminal, journals, _, _ := readCloseDeferredContract(t, ctx, cohort.victimId); !terminal || journals != 1 {
		t.Fatal("the deferred row was not retired once its operation succeeded")
	}
}

// A row-local deadline (here an already-expired child context for one
// continuation, the class of the bounded connection check that timed out in
// production) leaves that contract open. Siblings commit, the cursor advances,
// the failure stays explicit, and the retry comes at the scan cadence.
func TestCloseExpiredRowDeadlineDefersRowAndContinuesPromptly(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 120*time.Second)
		defer cancel()
		cohort := newCloseDeferredCohort(t, ctx, 3, true)
		var expire atomic.Bool
		expire.Store(true)
		ctx = model.Testing_WithForceCloseContinuationContext(ctx, func(parent context.Context, contractId server.Id) context.Context {
			if contractId != cohort.victimId || !expire.Load() {
				return parent
			}
			// The deadline has passed before the first statement, so the
			// row-local operation fails deterministically while the task runs.
			child, stop := context.WithDeadline(parent, time.Unix(1, 0))
			stop()
			return child
		})
		id := scheduleCloseDeferredTask(t, ctx, 16)
		worker := newCloseDeferredWorker(ctx)
		defer worker.Close()
		requireCloseDeferredFirstAttempt(t, ctx, cohort, worker, id, context.DeadlineExceeded.Error())
		requireCloseDeferredRecovery(t, ctx, cohort, worker, id, func() { expire.Store(false) })
	})
}

// Another owner holds the victim's immutable provider-totals key, so its
// try-lock refuses. The busy row stays open and the singleton keeps moving.
func TestCloseExpiredBusyRowDefersRowAndContinuesPromptly(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 120*time.Second)
		defer cancel()
		cohort := newCloseDeferredCohort(t, ctx, 3, true)
		held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- func() (returnErr error) {
				server.HandleError(func() {
					server.OwnedTx(ctx, []server.PgOwnershipKey{
						task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", cohort.victimId)),
					}, func(tx server.PgTx) {
						close(held)
						<-release
					}, server.TxReadCommitted, server.OptNoRetry())
				}, func(err error) { returnErr = err })
				return
			}()
		}()
		select {
		case <-held:
		case err := <-done:
			t.Fatal("the synthetic owner did not admit its key", err)
		}
		released := false
		releaseOwner := func() {
			if !released {
				released = true
				close(release)
				if err := <-done; err != nil {
					t.Fatal("the synthetic owner failed", err)
				}
			}
		}
		defer releaseOwner()
		id := scheduleCloseDeferredTask(t, ctx, 16)
		worker := newCloseDeferredWorker(ctx)
		defer worker.Close()
		requireCloseDeferredFirstAttempt(t, ctx, cohort, worker, id, "transfer balance ownership is busy")
		requireCloseDeferredRecovery(t, ctx, cohort, worker, id, releaseOwner)
	})
}

// When every selected row fails, the page proves no progress and may share one
// unavailable dependency. It stays an explicit failure without a checkpoint,
// and the registered target still retries within its cap, not after an hour.
func TestCloseExpiredFailureWithoutProgressRetriesWithinCap(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 120*time.Second)
		defer cancel()
		cohort := newCloseDeferredCohort(t, ctx, 0, false)
		ctx = model.Testing_WithForceCloseContinuationContext(ctx, func(parent context.Context, contractId server.Id) context.Context {
			if contractId != cohort.victimId {
				return parent
			}
			child, stop := context.WithDeadline(parent, time.Unix(1, 0))
			stop()
			return child
		})
		id := scheduleCloseDeferredTask(t, ctx, 16)
		originalArgs, _, _, originalMetadata, _ := readCloseRetryTask(t, ctx, id)
		if originalArgs.Sweep != nil || originalArgs.Cursor != nil {
			t.Fatal("a fresh scan unexpectedly started with a cursor")
		}
		worker := newCloseDeferredWorker(ctx)
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != id {
			t.Fatal("a page without progress hid its failure", finished, retried, posts, err)
		}
		requireCloseDeferredVictimOpen(t, ctx, cohort.victimId)
		args, diagnostic, errorCount, metadata, delay := readCloseRetryTask(t, ctx, id)
		if args.Sweep != nil || args.Cursor != nil || errorCount != 17 ||
			!strings.Contains(diagnostic, cohort.victimId.String()) || metadata != originalMetadata {
			t.Fatal("a page without progress changed its arguments or lost its explicit failure", args.Sweep, errorCount, diagnostic)
		}
		if delay <= 0 || closeExpiredContractsErrorRetryCap < delay {
			t.Fatal("an uncheckpointed failure escalated past the closer's retry cap", delay)
		}
	})
}
