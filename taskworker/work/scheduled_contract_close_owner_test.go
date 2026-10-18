// A retained legacy backlog closes through bounded owner transactions, a held
// owner's closes wait in their claims and defer only when the grant stays held
// through the wait, and terminal queued closes take no locks.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Every synthetic contract reserves this escrow and reports this final usage.
const scheduledCloseOwnerEscrowBytes = 100
const scheduledCloseOwnerUsedBytes = 10
const scheduledCloseOwnerInitialBytes = 1_000_000

// One provider endpoint receives every synthetic contract.
type scheduledCloseOwnerFixture struct {
	providerNetworkId server.Id
	providerClientId  server.Id
}

// A payer network with exactly one grant and its queued contracts.
type scheduledCloseOwnerPayer struct {
	networkId   server.Id
	clientId    server.Id
	balanceId   server.Id
	contractIds []server.Id
}

func newScheduledCloseOwnerFixture(ctx context.Context) *scheduledCloseOwnerFixture {
	f := &scheduledCloseOwnerFixture{providerNetworkId: server.NewId(), providerClientId: server.NewId()}
	model.Testing_CreateNetwork(ctx, f.providerNetworkId, "synthetic-owner-batch-provider", server.NewId())
	model.Testing_CreateDevice(ctx, f.providerNetworkId, server.NewId(), f.providerClientId, "synthetic-provider", "synthetic")
	return f
}

func (self *scheduledCloseOwnerFixture) newPayer(t testing.TB, ctx context.Context, name string) *scheduledCloseOwnerPayer {
	t.Helper()
	payer := &scheduledCloseOwnerPayer{networkId: server.NewId(), clientId: server.NewId()}
	model.Testing_CreateNetwork(ctx, payer.networkId, name, server.NewId())
	model.Testing_CreateDevice(ctx, payer.networkId, server.NewId(), payer.clientId, "synthetic-payer", "synthetic")
	server.Raise(model.AddBasicTransferBalance(ctx, payer.networkId, scheduledCloseOwnerInitialBytes,
		server.NowUtc().Add(-24*time.Hour), server.NowUtc().Add(24*time.Hour)))
	balances := model.GetActiveTransferBalances(ctx, payer.networkId)
	if len(balances) != 1 {
		t.Fatal("synthetic payer did not retain exactly one grant")
	}
	payer.balanceId = balances[0].BalanceId
	return payer
}

// Retained production backlog rows are legacy positive escrows without a stored
// expiration. Each contract has bilateral final reports for the same usage.
func (self *scheduledCloseOwnerFixture) addLegacyContracts(ctx context.Context, payer *scheduledCloseOwnerPayer, count int) []server.Id {
	ids := make([]server.Id, count)
	for index := range ids {
		ids[index] = server.NewId()
	}
	slices.SortFunc(ids, server.Id.Cmp)
	created := server.NowUtc().Add(-3 * time.Hour)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
			contract_id,source_network_id,source_id,destination_network_id,destination_id,
			transfer_byte_count,payer_network_id,usage_origin_is_source,create_time,expiration_time)
			SELECT contract_id,$2,$3,$4,$5,$6,$2,true,$7,NULL FROM unnest($1::uuid[]) AS candidate(contract_id)`,
			ids, payer.networkId, payer.clientId, self.providerNetworkId, self.providerClientId, scheduledCloseOwnerEscrowBytes, created))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count)
			SELECT contract_id,$2,$3 FROM unnest($1::uuid[]) AS candidate(contract_id)`, ids, payer.balanceId, scheduledCloseOwnerEscrowBytes))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close (contract_id,party,used_transfer_byte_count,close_time,checkpoint)
			SELECT contract_id,party,$2,$3,false FROM unnest($1::uuid[]) AS candidate(contract_id)
			CROSS JOIN (VALUES ('source'),('destination')) AS report(party)`, ids, scheduledCloseOwnerUsedBytes, created.Add(time.Minute)))
	})
	payer.contractIds = append(payer.contractIds, ids...)
	return ids
}

// The real startup pass publishes every open contract with its legacy deadline.
func publishScheduledCloses(t testing.TB, ctx context.Context) time.Time {
	t.Helper()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	startedAt := server.NowUtc().Truncate(time.Microsecond).Add(-2 * time.Hour)
	if _, err := ScheduleOpenContractClosures(&ScheduleOpenContractClosuresArgs{PageSize: startupContractClosurePageSize, StartedAt: startedAt}, owner); err != nil {
		t.Fatal("startup pass did not publish the synthetic backlog", err)
	}
	return startedAt.Add(model.DefaultContractExpiration)
}

// Counts session admissions at the real ownership boundary, per balance.
type scheduledCloseOwnerEvents struct {
	stateLock sync.Mutex
	admitted  map[server.Id]int
	waiting   map[server.Id]int
	refused   map[server.Id]int
	balances  map[server.PgOwnershipKey]server.Id
}

func newScheduledCloseOwnerEvents(payers ...*scheduledCloseOwnerPayer) *scheduledCloseOwnerEvents {
	events := &scheduledCloseOwnerEvents{admitted: map[server.Id]int{}, waiting: map[server.Id]int{}, refused: map[server.Id]int{},
		balances: map[server.PgOwnershipKey]server.Id{}}
	for _, payer := range payers {
		events.balances[server.NewPgOwnershipKey("transfer_balance", payer.balanceId)] = payer.balanceId
	}
	return events
}

func (self *scheduledCloseOwnerEvents) observe(event server.PgOwnershipEvent) {
	if event.TransactionScoped {
		// Optional mirror caches probe inside their own transactions.
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, key := range event.Keys {
		balanceId, ok := self.balances[key]
		if !ok {
			continue
		}
		switch event.Kind {
		case server.PgOwnershipAdmitted:
			self.admitted[balanceId]++
		case server.PgOwnershipWaiting:
			self.waiting[balanceId]++
		case server.PgOwnershipRefused:
			self.refused[balanceId]++
		}
	}
}

func (self *scheduledCloseOwnerEvents) counts(balanceId server.Id) (admitted, waiting, refused int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.admitted[balanceId], self.waiting[balanceId], self.refused[balanceId]
}

// One claim per admission keeps every owner decision in a fixed order.
func drainScheduledCloses(t testing.TB, worker *task.TaskWorker, limit int) (executions int) {
	t.Helper()
	for {
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(retried) != 0 || len(posts) != 0 {
			t.Fatal("scheduled close attempt failed instead of finishing", finished, retried, posts, err)
		}
		if len(finished) == 0 {
			return
		}
		executions += len(finished)
		if limit < executions {
			t.Fatal("scheduled close backlog did not drain within its task count")
		}
	}
}

// Exact accounting for closed contracts: one settled outcome, one payout and
// one sweep per contract, reports unchanged and the grant debited once each.
func requireScheduledCloseOwnerSettled(t testing.TB, ctx context.Context, payer *scheduledCloseOwnerPayer, closed int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var settled, paid, swept, reports, journals int
		var balance model.ByteCount
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled' AND NOT dispute),
			(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND payout_byte_count=$3),
			(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=ANY($1) AND payout_byte_count=$3),
			(SELECT count(*) FROM contract_close WHERE contract_id=ANY($1) AND used_transfer_byte_count=$3 AND NOT checkpoint),
			(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=ANY($1)),
			(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)`,
			payer.contractIds, payer.balanceId, scheduledCloseOwnerUsedBytes).Scan(&settled, &paid, &swept, &reports, &journals, &balance))
		if settled != closed || paid != closed || swept != closed || reports != 2*len(payer.contractIds) || journals != 0 ||
			balance != model.ByteCount(scheduledCloseOwnerInitialBytes-closed*scheduledCloseOwnerUsedBytes) {
			t.Fatalf("owner accounting is not exact: settled=%d paid=%d swept=%d reports=%d journals=%d balance=%d want closed=%d",
				settled, paid, swept, reports, journals, balance, closed)
		}
	})
}

// Reads the stored results of every finished scheduled close.
func readScheduledCloseResults(t testing.TB, ctx context.Context) []CloseScheduledContractResult {
	t.Helper()
	results := []CloseScheduledContractResult{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT result_json FROM finished_task WHERE function_name=$1 ORDER BY run_end_time,task_id`, scheduledCloseFunctionName)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var data string
				server.Raise(rows.Scan(&data))
				var result CloseScheduledContractResult
				server.Raise(json.Unmarshal([]byte(data), &result))
				results = append(results, result)
			}
		})
	})
	return results
}

// One grant owner admitted once per queued contract on the old worker, so a
// backlog serialized behind its own owner. The owner is now admitted twice per
// bounded batch: once for the primary and its sibling read, once for the
// siblings. The batch also removes its siblings' queued closes, so one claim
// retires each batch; those queued closes used to outlive their contracts and
// each was claimed again only to finish through the terminal read.
func TestScheduledContractCloseOwnerBatchBoundsAdmissions(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const count = 2*model.ContractDeadlineOwnerBatchLimit + 9
		f := newScheduledCloseOwnerFixture(t.Context())
		payer := f.newPayer(t, t.Context(), "synthetic-owner-batch-payer")
		f.addLegacyContracts(t.Context(), payer, count)
		events := newScheduledCloseOwnerEvents(payer)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		publishScheduledCloses(t, ctx)
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		executions := drainScheduledCloses(t, worker, count)
		admitted, waiting, refused := events.counts(payer.balanceId)
		batches := (count + model.ContractDeadlineOwnerBatchLimit - 1) / model.ContractDeadlineOwnerBatchLimit
		if executions != batches || admitted != 2*batches || waiting != 0 || refused != 0 {
			t.Fatalf("owner admissions=%d waiting=%d refused=%d executions=%d; want %d admissions and claims for %d queued closes",
				admitted, waiting, refused, executions, 2*batches, count)
		}
		if queued := readQueuedScheduledCloseContracts(t, ctx); len(queued) != 0 {
			t.Fatal("owner batches left their siblings' queued closes", len(queued))
		}
		requireScheduledCloseOwnerSettled(t, ctx, payer, count)
		closing, siblings, terminal, stealing, stolen := 0, 0, 0, 0, 0
		for _, result := range readScheduledCloseResults(t, ctx) {
			switch {
			case result.Reconciliation == nil || result.Reconciliation.Missing:
				t.Fatal("queued close finished without a reconciliation")
			case result.Reconciliation.AlreadyClosed:
				terminal++
			default:
				closing++
				siblings += result.OwnerBatchClosed
			}
			if result.StolenClosed != 0 {
				stealing++
				stolen += result.StolenClosed
			}
		}
		if closing != batches || closing+siblings != count || terminal != 0 || stealing != 0 || stolen != 0 {
			t.Fatalf("results closing=%d siblings=%d stealing=%d stolen=%d terminal=%d; want %d batches covering %d contracts",
				closing, siblings, stealing, stolen, terminal, batches, count)
		}
	})
}

// Ends every refused close's owner wait at once, so a held owner's close defers
// at its first refusal, as every one did before closes waited.
func endScheduledCloseOwnerWait(t testing.TB) {
	ended := make(chan time.Time)
	close(ended)
	ownerWaitTimeout := scheduledCloseOwnerWaitTimeout
	scheduledCloseOwnerWaitTimeout = func() <-chan time.Time { return ended }
	t.Cleanup(func() { scheduledCloseOwnerWaitTimeout = ownerWaitTimeout })
}

// Another owner holds the payer's grant through every close's wait. Every
// queued close for that payer is refused once and defers with its deadline
// unchanged and no recorded failure, while other payers' contracts close in
// the same claim. Each successor wakes at its deferral, behind every due row;
// the test used to move those wakes into the past and drain them, which hid
// how far behind the queue a deferral lands. A grant released within the wait
// closes in the claims instead.
func TestScheduledContractCloseOwnerHeldThroughTheWaitDefers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const heldCount = model.ContractDeadlineOwnerBatchLimit + 8
		const otherCount = 8
		endScheduledCloseOwnerWait(t)
		f := newScheduledCloseOwnerFixture(t.Context())
		held := f.newPayer(t, t.Context(), "synthetic-held-owner-payer")
		f.addLegacyContracts(t.Context(), held, heldCount)
		others := make([]*scheduledCloseOwnerPayer, otherCount)
		for index := range others {
			others[index] = f.newPayer(t, t.Context(), fmt.Sprintf("synthetic-other-owner-payer-%d", index))
			f.addLegacyContracts(t.Context(), others[index], 1)
		}
		events := newScheduledCloseOwnerEvents(append([]*scheduledCloseOwnerPayer{held}, others...)...)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		deadline := publishScheduledCloses(t, ctx)
		trace := &scheduledCloseLockTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()
		release := holdScheduledCloseOwner(t, t.Context(), held)
		defer release()

		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		before := server.NowUtc()
		finished, retried, posts, err := worker.EvalTasks(heldCount + otherCount)
		if err != nil || len(retried) != 0 || len(posts) != 0 || len(finished) != heldCount+otherCount {
			t.Fatalf("held owner attempts failed instead of deferring: finished=%d retried=%d posts=%d err=%v",
				len(finished), len(retried), len(posts), err)
		}
		admitted, waiting, refused := events.counts(held.balanceId)
		if admitted != 0 || waiting != 0 || refused != heldCount || trace.siblingScanCount() != otherCount {
			t.Fatalf("held owner admitted=%d waiting=%d refused=%d sibling scans=%d; want %d refusals, no wait and scans only by %d admitted owners",
				admitted, waiting, refused, trace.siblingScanCount(), heldCount, otherCount)
		}
		for _, other := range others {
			admitted, waiting, refused := events.counts(other.balanceId)
			if admitted != 1 || waiting != 0 || refused != 0 {
				t.Fatal("independent payer did not close through one admission", admitted, waiting, refused)
			}
			requireScheduledCloseOwnerSettled(t, ctx, other, 1)
		}
		requireScheduledCloseOwnerSettled(t, ctx, held, 0)
		deferred := []server.Id{}
		for _, row := range readExpiryRecoveryQueue(t, ctx) {
			if row.function != scheduledCloseFunctionName {
				continue
			}
			var args CloseScheduledContractArgs
			server.Raise(json.Unmarshal([]byte(row.args), &args))
			if !args.Deadline.Equal(deadline) || row.runAt.Before(before.Add(scheduledCloseOwnerBusyDeferral)) || row.claimTime.After(time.Time{}) ||
				!slices.Contains(held.contractIds, args.ContractId) {
				t.Fatal("held owner deferral changed its deadline, lost its wake or claimed another payer", args, row.runAt)
			}
			deferred = append(deferred, row.id)
		}
		if len(deferred) != heldCount {
			t.Fatal("held owner did not keep exactly one deferred close per contract", len(deferred))
		}
		server.Db(ctx, func(conn server.PgConn) {
			var errored int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE task_id=ANY($1) AND reschedule_error_count<>0`, deferred).Scan(&errored))
			if errored != 0 {
				t.Fatal("a busy owner was recorded as a failed close")
			}
		})
	})
}

// Another owner holds the payer's grant while all of its queued closes and
// other payers' closes are claimed together. A refused close waits in its
// claim and holds no close slot between tries, so every other payer's close
// commits while the grant is still held, and no refused close reads the
// owner's siblings. Once the holder releases, the refused closes retire every
// one of the payer's contracts in bounded owner batches with exact accounting,
// each within its own claim: none defers or leaves a queued close. These
// closes used to defer at their first refusal.
func TestScheduledContractCloseOwnerReleasedWithinTheWaitClosesInTheClaims(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const heldCount = model.ContractDeadlineOwnerBatchLimit + 8
		const otherCount = 8
		// Neither wait ends on its own, so only the release ends them.
		ownerWaitTimeout, slotTimeout := scheduledCloseOwnerWaitTimeout, scheduledCloseSlotTimeout
		scheduledCloseOwnerWaitTimeout = func() <-chan time.Time { return nil }
		scheduledCloseSlotTimeout = func() <-chan time.Time { return nil }
		defer func() { scheduledCloseOwnerWaitTimeout, scheduledCloseSlotTimeout = ownerWaitTimeout, slotTimeout }()
		f := newScheduledCloseOwnerFixture(t.Context())
		held := f.newPayer(t, t.Context(), "synthetic-released-owner-payer")
		f.addLegacyContracts(t.Context(), held, heldCount)
		others := make([]*scheduledCloseOwnerPayer, otherCount)
		for index := range others {
			others[index] = f.newPayer(t, t.Context(), fmt.Sprintf("synthetic-released-other-payer-%d", index))
			f.addLegacyContracts(t.Context(), others[index], 1)
		}
		events := newScheduledCloseOwnerEvents(append([]*scheduledCloseOwnerPayer{held}, others...)...)
		// A refusal carries its contract's own publication key.
		heldKey := server.NewPgOwnershipKey("transfer_balance", held.balanceId)
		heldContractKeys := map[server.PgOwnershipKey]server.Id{}
		for _, contractId := range held.contractIds {
			heldContractKeys[task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId))] = contractId
		}
		otherKeys := map[server.PgOwnershipKey]bool{}
		for _, other := range others {
			otherKeys[server.NewPgOwnershipKey("transfer_balance", other.balanceId)] = true
		}
		var stateLock sync.Mutex
		refusedIds := map[server.Id]bool{}
		otherReleases := 0
		waited := make(chan struct{})
		var waitedOnce sync.Once
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), func(event server.PgOwnershipEvent) {
			events.observe(event)
			if event.TransactionScoped {
				return
			}
			ready := false
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				switch {
				case event.Kind == server.PgOwnershipRefused && slices.Contains(event.Keys, heldKey):
					for _, key := range event.Keys {
						if contractId, found := heldContractKeys[key]; found {
							refusedIds[contractId] = true
						}
					}
				case event.Kind == server.PgOwnershipReleased:
					for _, key := range event.Keys {
						if otherKeys[key] {
							otherReleases++
						}
					}
				}
				ready = len(refusedIds) == heldCount && otherReleases == otherCount
			}()
			if ready {
				waitedOnce.Do(func() { close(waited) })
			}
		})
		publishScheduledCloses(t, ctx)
		trace := &scheduledCloseLockTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()
		var releaseOnce sync.Once
		releaseHolder := holdScheduledCloseOwner(t, t.Context(), held)
		release := func() { releaseOnce.Do(releaseHolder) }
		defer release()

		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		done := make(chan struct{})
		var finished, retried, posts []server.Id
		var evalErr error
		go func() {
			defer close(done)
			finished, retried, posts, evalErr = worker.EvalTasks(heldCount + otherCount)
		}()
		defer func() {
			release()
			<-done
		}()
		select {
		case <-waited:
		case <-done:
			t.Fatal("the claimed closes finished while the grant was held", len(finished), len(retried), len(posts), evalErr)
		case <-time.After(time.Minute):
			t.Fatal("the other payers' closes did not commit while the refused closes waited")
		}
		if scans := trace.siblingScanCount(); scans != otherCount {
			t.Fatal("a refused close read its owner's siblings", scans)
		}
		requireScheduledCloseOwnerSettled(t, ctx, held, 0)
		for _, other := range others {
			requireScheduledCloseOwnerSettled(t, ctx, other, 1)
		}

		release()
		<-done
		if evalErr != nil || len(retried) != 0 || len(posts) != 0 || len(finished) != heldCount+otherCount {
			t.Fatalf("released owner's closes failed: finished=%d retried=%d posts=%d err=%v", len(finished), len(retried), len(posts), evalErr)
		}
		requireScheduledCloseOwnerSettled(t, ctx, held, heldCount)
		closed := 0
		for _, result := range readScheduledCloseResults(t, ctx) {
			if result.DeferredUntil != nil || result.RetryAt != nil || result.Reconciliation == nil || result.Reconciliation.Missing ||
				model.ContractDeadlineOwnerBatchLimit-1 < result.OwnerBatchClosed {
				t.Fatalf("a claimed close did not finish within its claim in one bounded batch: %+v", result)
			}
			if !result.Reconciliation.AlreadyClosed {
				closed++
			}
			closed += result.OwnerBatchClosed + result.StolenClosed
		}
		if closed != heldCount+otherCount {
			t.Fatal("the claims did not close each contract exactly once", closed, heldCount+otherCount)
		}
		if queued := readQueuedScheduledCloseContracts(t, ctx); len(queued) != 0 {
			t.Fatal("a close that finished in its claim left a queued close", len(queued))
		}
		if admitted, waiting, refused := events.counts(held.balanceId); admitted < 2 || waiting != 0 || refused < heldCount {
			t.Fatal("the released owner did not close through refused closes that waited", admitted, waiting, refused)
		}
		for _, other := range others {
			if admitted, waiting, refused := events.counts(other.balanceId); admitted != 1 || waiting != 0 || refused != 0 {
				t.Fatal("independent payer did not close through one admission", admitted, waiting, refused)
			}
		}
	})
}

// Records the locked contract read that every reconciliation performs first
// and the owner's sibling scan, which only an admitted owner may run.
type scheduledCloseLockTrace struct {
	stateLock    sync.Mutex
	locked       int
	siblingScans int
}

func (self *scheduledCloseLockTrace) record(sql string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if strings.Contains(sql, "FROM transfer_contract WHERE contract_id=$1 FOR UPDATE") {
		self.locked++
	}
	if strings.Contains(sql, "WHERE balance_id=owner.balance_id AND settled=false") {
		self.siblingScans++
	}
}

func (self *scheduledCloseLockTrace) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.locked
}

func (self *scheduledCloseLockTrace) siblingScanCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.siblingScans
}

// Ordinary statements, including the claim and completion protocol.
func (self *scheduledCloseLockTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.record(data.SQL)
	return ctx
}

// Nothing to record after a statement.
func (self *scheduledCloseLockTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Batches are recorded per queued statement.
func (self *scheduledCloseLockTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

// The locked settlement read is sent as a batch statement.
func (self *scheduledCloseLockTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	self.record(data.SQL)
}

// Nothing to record after a batch.
func (self *scheduledCloseLockTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// Queued closes retained from before closers removed them, and those a close
// skipped while leased or held, outlive their contracts; retention can remove
// a contract too. Each queued payload, exactly as the startup pass stored it,
// finishes as a no-op without an owner admission or a locked read.
func TestScheduledContractCloseTerminalQueuedTasksFinishWithoutLocks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const count = 6
		f := newScheduledCloseOwnerFixture(t.Context())
		payer := f.newPayer(t, t.Context(), "synthetic-terminal-queue-payer")
		f.addLegacyContracts(t.Context(), payer, count)
		events := newScheduledCloseOwnerEvents(payer)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		deadline := publishScheduledCloses(t, ctx)
		for _, id := range payer.contractIds {
			if closed, err := model.ReconcileContractAtDeadline(t.Context(), id, deadline); err != nil || closed.AlreadyClosed {
				t.Fatal("fixture did not close its contract before the queued task", err)
			}
			// A close now removes its queued close; the retained queue still
			// holds those that outlived their contracts.
			requeueRetiredScheduledClose(t, ctx, id, deadline)
		}
		missingId := server.NewId()
		requeueRetiredScheduledClose(t, ctx, missingId, deadline)
		requireScheduledCloseOwnerSettled(t, ctx, payer, count)
		baseline, _, _ := events.counts(payer.balanceId)

		trace := &scheduledCloseLockTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		executions := drainScheduledCloses(t, worker, count+1)
		worker.Close()
		server.Raise(scope.Close())
		admitted, waiting, refused := events.counts(payer.balanceId)
		if executions != count+1 || trace.count() != 0 || admitted != baseline || waiting != 0 || refused != 0 {
			t.Fatalf("terminal queued closes executions=%d locked reads=%d admissions=%d waiting=%d refused=%d; want no locks or owners",
				executions, trace.count(), admitted-baseline, waiting, refused)
		}
		requireScheduledCloseOwnerSettled(t, ctx, payer, count)
		terminal, missing := 0, 0
		for _, result := range readScheduledCloseResults(t, ctx) {
			switch {
			case result.Reconciliation == nil:
				t.Fatal("queued terminal close returned no reconciliation")
			case result.Reconciliation.Missing && result.Reconciliation.ContractId == missingId:
				missing++
			case result.Reconciliation.AlreadyClosed && result.Reconciliation.Outcome == model.ContractOutcomeSettled:
				terminal++
			default:
				t.Fatal("queued terminal close reported a new closure", result.Reconciliation)
			}
		}
		if terminal != count || missing != 1 {
			t.Fatal("queued terminal closes did not finish as explicit no-ops", terminal, missing)
		}
	})
}
