// A retained legacy backlog closes through bounded owner transactions, a held
// owner defers instead of waiting, and terminal queued closes take no locks.
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
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
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
// siblings. The other queued closes finish through the terminal read.
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
		if executions != count || admitted != 2*batches || waiting != 0 || refused != 0 {
			t.Fatalf("owner admissions=%d waiting=%d refused=%d executions=%d; want %d admissions for %d queued closes",
				admitted, waiting, refused, executions, 2*batches, count)
		}
		requireScheduledCloseOwnerSettled(t, ctx, payer, count)
		closing, siblings, terminal := 0, 0, 0
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
		}
		if closing != batches || closing+siblings != count || terminal != count-batches {
			t.Fatalf("results closing=%d siblings=%d terminal=%d; want %d batches covering %d contracts", closing, siblings, terminal, batches, count)
		}
	})
}

// Another owner holds the payer's grant. Every queued close for that payer
// refuses once and defers with its deadline unchanged, never waiting out the
// admission budget, while other payers' contracts close in the same claim.
// After release the deferred closes drain in bounded owner batches.
func TestScheduledContractCloseHeldOwnerDefersWithoutWaiting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const heldCount = model.ContractDeadlineOwnerBatchLimit + 8
		const otherCount = 8
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

		admittedHolder, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(holderDone)
			server.OwnedTx(t.Context(), []server.PgOwnershipKey{server.NewPgOwnershipKey("transfer_balance", held.balanceId)}, func(tx server.PgTx) {
				close(admittedHolder)
				<-release
			}, server.TxReadCommitted, server.OptNoRetry())
		}()
		<-admittedHolder
		released := false
		defer func() {
			if !released {
				close(release)
			}
			<-holderDone
		}()

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
			if !args.Deadline.Equal(deadline) || !row.runAt.After(before) || row.claimTime.After(time.Time{}) ||
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

		close(release)
		released = true
		<-holderDone
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE task_id=ANY($1)`, deferred, before.Add(-time.Minute)))
		})
		executions := drainScheduledCloses(t, worker, heldCount)
		admitted, waiting, _ = events.counts(held.balanceId)
		batches := (heldCount + model.ContractDeadlineOwnerBatchLimit - 1) / model.ContractDeadlineOwnerBatchLimit
		if executions != heldCount || admitted != 2*batches || waiting != 0 {
			t.Fatalf("released owner admissions=%d waiting=%d executions=%d; want %d admissions", admitted, waiting, executions, 2*batches)
		}
		requireScheduledCloseOwnerSettled(t, ctx, held, heldCount)
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

// Queued closes outlive their contracts: other closers settle them and
// retention can remove them. Each queued payload, exactly as the startup pass
// stored it, finishes as a no-op without an owner admission or a locked read.
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
		}
		missingId := server.NewId()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		key := task.RunOnce("close_scheduled_contract", missingId)
		server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(key)}, func(tx server.PgTx) {
			scheduleContractClose(owner, tx, &CloseScheduledContractArgs{Private: true,
				ScheduledContractClose: ScheduledContractClose{ContractId: missingId, Deadline: deadline}})
		}, server.TxReadCommitted, server.OptNoRetry())
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
