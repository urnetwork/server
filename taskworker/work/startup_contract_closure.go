// Startup enumerates every nonterminal contract independently of sweep cursors
// and queues a scheduled close for each legacy contract, one without a stored
// deadline: its queued close is that contract's only deadline authority. A
// contract issued with a stored deadline is closed at it by the deadline pass,
// owner batches and steals, so it gets no queued close; a queued close would
// only wait behind the legacy backlog and finish as a terminal no-op.
//
// Every contract minted since the stored deadline rollout carries one, so a
// pass ends after startupContractClosureLegacyFreePageLimit consecutive full
// pages without a legacy contract instead of reading every open contract
// minted since. A legacy straggler beyond them is still due through the expiry
// sweep, which gives a missing deadline its maximum lifetime.
//
// A publication inserts a missing queued close and leaves an existing one
// untouched. A queued close's arguments already carry the earliest deadline
// any pass assigns it (a later pass starts later), so merging only wrote a new
// tuple per contract on every pass, moved a deferred close's wake back into
// the backlog, and for a claimed row recorded a wake whose successor was
// another queued close for the contract its claim was closing.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const startupContractClosurePageSize = 1024
const startupContractClosureMaxPageSize = 10000
const startupContractClosurePublicationSize = 256

// Consecutive full pages without a legacy contract that end a pass. On Main,
// legacy stragglers minted after the rollout were at most a few per 5000 open
// contracts and stopped within three days, so these pages lie past them.
const startupContractClosureLegacyFreePageLimit = 64

// One timestamp supplies the default lifetime for this complete startup pass.
type ScheduleOpenContractClosuresArgs struct {
	Private   bool                            `json:"_private_task_arguments,omitempty"`
	PageSize  int                             `json:"page_size"`
	StartedAt time.Time                       `json:"started_at"`
	Progress  *startupContractClosureProgress `json:"_startup_scan_progress,omitempty"`
}

type ScheduledContractClose struct {
	ContractId server.Id `json:"contract_id"`
	Deadline   time.Time `json:"deadline"`
}

type ScheduleOpenContractClosuresResult struct{}

// A pass enumerates every nonterminal contract and resumes from committed
// checkpoints, so one attempt can run for hours. The default task deadline cut
// every attempt short and the pass never finished.
const startupContractClosureMaxTime = 24 * time.Hour

// An earlier release scheduled the same pass under this key. Both passes
// walked the same contracts, the earlier key hours behind, so each of its
// upserts only moved an already queued close back to that pass's earlier
// deadline and recorded another wake. Its pending pass finishes on its next
// claim without reading; the startup pass covers every contract it would.
const retiredStartupContractClosureKey = "schedule_open_contract_closures"

func ScheduleOpenContractClosuresOnStartup(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures,
		&ScheduleOpenContractClosuresArgs{PageSize: startupContractClosurePageSize, StartedAt: server.NowUtc().Truncate(time.Microsecond)},
		clientSession, task.RunOnce("schedule_open_contract_closures_on_startup"), task.RunAt(server.NowUtc()),
		task.MaxTime(startupContractClosureMaxTime))
}

// One logical pass retains its original start time. The old queued After field
// remains ignored; only a claim-fenced committed checkpoint can resume a pass.
func ScheduleOpenContractClosures(args *ScheduleOpenContractClosuresArgs, clientSession *session.ClientSession) (*ScheduleOpenContractClosuresResult, error) {
	if args == nil || args.StartedAt.IsZero() {
		return nil, fmt.Errorf("startup contract scan has no start time")
	}
	pageSize := args.PageSize
	if pageSize == 0 {
		pageSize = startupContractClosurePageSize
	}
	if pageSize < 1 || pageSize > startupContractClosureMaxPageSize {
		return nil, fmt.Errorf("invalid startup contract scan page size")
	}
	var lastContractId server.Id
	var retryFrom *server.Id
	checkpoint, _ := clientSession.Ctx.Value(startupContractClosureCheckpointKey{}).(*startupContractClosureCheckpoint)
	if checkpoint != nil {
		lastContractId = checkpoint.progress.After
		retryFrom = checkpoint.progress.RetryFrom
	}
	legacyFreePages := 0
	for {
		if err := clientSession.Ctx.Err(); err != nil {
			return nil, err
		}
		// Only legacy contracts are published; the page still advances past
		// every open contract it read.
		contracts := make([]ScheduledContractClose, 0, pageSize)
		pageCount := 0
		var pageLastContractId server.Id
		server.MaintenanceDb(clientSession.Ctx, func(conn server.PgConn) {
			rows, err := conn.Query(clientSession.Ctx, `SELECT contract_id,expiration_time FROM transfer_contract
				WHERE outcome IS NULL AND contract_id>$2 ORDER BY contract_id LIMIT $1`, pageSize, lastContractId)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contractId server.Id
					var expiration *time.Time
					server.Raise(rows.Scan(&contractId, &expiration))
					pageCount++
					pageLastContractId = contractId
					if expiration != nil {
						continue
					}
					contracts = append(contracts, ScheduledContractClose{
						ContractId: contractId,
						Deadline:   args.StartedAt.Add(model.DefaultContractExpiration),
					})
				}
			})
		}, server.OptNoRetry())
		// Release the maintenance connection before ordinary queue writes.
		// A busy group cannot hold back independent later pages. Its earliest
		// predecessor remains debt in each later committed checkpoint.
		for offset := 0; offset < len(contracts); offset += startupContractClosurePublicationSize {
			chunk := contracts[offset:min(offset+startupContractClosurePublicationSize, len(contracts))]
			keys := make([]server.PgOwnershipKey, 0, len(chunk))
			for _, contract := range chunk {
				keys = append(keys, task.RunOnceOwnershipKey(model.ScheduledContractCloseRunOnce(contract.ContractId)))
			}
			if checkpoint != nil {
				keys = append(keys, checkpoint.key)
			}
			var next *startupContractClosureProgress
			if checkpoint != nil {
				copyProgress := *checkpoint.progress
				copyProgress.After = chunk[len(chunk)-1].ContractId
				copyProgress.RetryFrom = retryFrom
				next = &copyProgress
			}
			admitted := server.TryOwnedTx(clientSession.Ctx, keys, func(tx server.PgTx) {
				server.BatchInTx(clientSession.Ctx, tx, func(batch server.PgBatch) {
					for _, contract := range chunk {
						args := &CloseScheduledContractArgs{Private: true, ScheduledContractClose: contract}
						task.QueueTaskInBatchIfAbsent(tx, batch, CloseScheduledContract, args, clientSession,
							model.ScheduledContractCloseRunOnce(contract.ContractId), task.RunAt(contract.Deadline),
							task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
					}
				})
				if checkpoint != nil {
					checkpoint.write(clientSession.Ctx, tx, args, next)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			if admitted {
				if checkpoint != nil {
					checkpoint.progress = next
					checkpoint.published = true
				}
			} else if retryFrom == nil {
				predecessor := lastContractId
				retryFrom = &predecessor
			}
			lastContractId = chunk[len(chunk)-1].ContractId
		}
		if pageCount == 0 {
			break
		}
		lastContractId = pageLastContractId
		if pageCount == pageSize && len(contracts) == 0 {
			legacyFreePages++
		} else {
			legacyFreePages = 0
		}
		if startupContractClosureLegacyFreePageLimit <= legacyFreePages {
			break
		}
	}
	if retryFrom != nil {
		if checkpoint != nil {
			next := *checkpoint.progress
			next.After, next.RetryFrom = *retryFrom, nil
			server.OwnedTx(clientSession.Ctx, []server.PgOwnershipKey{checkpoint.key}, func(tx server.PgTx) {
				checkpoint.write(clientSession.Ctx, tx, args, &next)
			}, server.TxReadCommitted, server.OptNoRetry())
			checkpoint.progress = &next
		}
		return nil, task.WithRetryDelay(fmt.Errorf("startup contract scan child publication ownership is busy"), task.RescheduleTimeout)
	}
	return &ScheduleOpenContractClosuresResult{}, nil
}

func ScheduleOpenContractClosuresPost(_ *ScheduleOpenContractClosuresArgs, _ *ScheduleOpenContractClosuresResult,
	_ *session.ClientSession, _ server.PgTx) error {
	return nil
}

type startupContractClosureTarget struct{ task.Target }

// Child chunks already committed under their bounded owners. Completion keeps
// only the scanner's pending and finished owners, including ordinary retries.
func (self *startupContractClosureTarget) TaskCompletionOwnershipKeys(_ *task.Task, _ string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}

func NewStartupContractClosureTaskTarget() task.Target {
	return &startupContractClosureTarget{Target: task.NewTaskTargetWithPost(ScheduleOpenContractClosures, ScheduleOpenContractClosuresPost)}
}

type CloseScheduledContractArgs struct {
	Private bool `json:"_private_task_arguments"`
	ScheduledContractClose
}

// A deferred close keeps RetryAt so an older Post still reschedules it at its
// deadline; a current Post honors the later deferral instead.
type CloseScheduledContractResult struct {
	Owner          *model.ContractCloseOwner             `json:"owner,omitempty"`
	RetryAt        *time.Time                            `json:"retry_at,omitempty"`
	Reconciliation *model.ContractDeadlineReconciliation `json:"reconciliation,omitempty"`
	DeferredUntil  *time.Time                            `json:"deferred_until,omitempty"`
	// Due siblings newly closed by this task's second owner turn.
	OwnerBatchClosed int `json:"owner_batch_closed,omitempty"`
	// Another owner's due contracts newly closed by this task's claim after
	// its own close was refused or already terminal.
	StolenClosed int `json:"stolen_closed,omitempty"`
}

func scheduleContractClose(clientSession *session.ClientSession, tx server.PgTx, args *CloseScheduledContractArgs) {
	task.ScheduleTaskInTx(tx, CloseScheduledContract, args, clientSession,
		model.ScheduledContractCloseRunOnce(args.ContractId), task.RunAt(args.Deadline),
		task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
}

// An early queue wake does not grant deadline authority. Normal task retries
// retain failed closes; a due success means the terminal commit completed.
// A committed terminal or missing contract finishes from one unlocked read.
// An open contract's close first takes one of the process's close slots, and
// defers when none frees in time. A funded close admits its balance owner
// without waiting, closes, and reads the owner's due siblings; one more owner
// turn retires up to a batch of them. A held owner defers this close rather
// than holding the worker slot. A held or terminal close first spends its
// claim on another owner's due closes.
func CloseScheduledContract(args *CloseScheduledContractArgs, clientSession *session.ClientSession) (*CloseScheduledContractResult, error) {
	if args == nil || !args.Private || args.ContractId == (server.Id{}) || args.Deadline.IsZero() {
		return nil, fmt.Errorf("invalid scheduled contract close")
	}
	now := server.NowUtc()
	if now.Before(args.Deadline) {
		return &CloseScheduledContractResult{RetryAt: &args.Deadline}, nil
	}
	ctx := clientSession.Ctx
	custody, err := model.ReadContractDeadlineCustody(ctx, []server.Id{args.ContractId})
	if err != nil {
		return nil, err
	}
	state := custody[args.ContractId]
	switch {
	case state == nil || !state.Found:
		return &CloseScheduledContractResult{Reconciliation: &model.ContractDeadlineReconciliation{ContractId: args.ContractId, Missing: true},
			StolenClosed: stealScheduledCloseOwner(ctx, now, args.ContractId, nil)}, nil
	case state.Outcome != nil:
		return &CloseScheduledContractResult{Reconciliation: &model.ContractDeadlineReconciliation{
			ContractId: args.ContractId, Outcome: *state.Outcome, AlreadyClosed: true},
			StolenClosed: stealScheduledCloseOwner(ctx, now, args.ContractId, nil)}, nil
	}
	release, admitted := scheduledCloseSlots.acquire(ctx, scheduledCloseParallelLimit, scheduledCloseSlotTimeout())
	if !admitted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		deferredUntil := server.NowUtc().Add(scheduledCloseOwnerBusyDeferral)
		return &CloseScheduledContractResult{RetryAt: &args.Deadline, DeferredUntil: &deferredUntil}, nil
	}
	defer release()
	if len(state.BalanceIds) == 0 {
		// No grant owns this contract, so there is no shared owner to batch.
		closed, err := model.ReconcileContractAtDeadline(ctx, args.ContractId, args.Deadline)
		return &CloseScheduledContractResult{Reconciliation: closed}, err
	}
	closed, err := model.ReconcileContractOwnerAtDeadline(ctx, state.BalanceIds,
		model.ContractDeadline{ContractId: args.ContractId, Deadline: args.Deadline}, dueContractDeadlines(now))
	if err != nil {
		return nil, err
	}
	if closed.Busy {
		// A steal holds its own slot.
		release()
		stolenClosed := stealScheduledCloseOwner(ctx, now, args.ContractId, state.BalanceIds)
		deferredUntil := server.NowUtc().Add(scheduledCloseOwnerBusyDeferral)
		return &CloseScheduledContractResult{RetryAt: &args.Deadline, DeferredUntil: &deferredUntil, StolenClosed: stolenClosed}, nil
	}
	return &CloseScheduledContractResult{Reconciliation: closed.Reconciliation, OwnerBatchClosed: closed.SiblingsClosed}, nil
}

// Early wakes retain the deadline. Owner handoffs are only for results written
// by older workers; new due results already committed terminal reconciliation.
// Register those retained hints with their owner wake; busy or changed custody
// yields a new child without changing its deadline. A busy owner deferral wakes
// later; its unchanged arguments keep the original deadline authority. A
// contract another closer retired meanwhile, usually the held owner's own
// batch, gets no successor: it could only finish as a terminal no-op.
func CloseScheduledContractPost(args *CloseScheduledContractArgs, result *CloseScheduledContractResult,
	clientSession *session.ClientSession, tx server.PgTx) error {
	if result.DeferredUntil != nil || result.RetryAt != nil {
		var open bool
		err := tx.QueryRow(clientSession.Ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1 AND outcome IS NULL)`,
			args.ContractId).Scan(&open)
		if err != nil {
			return err
		}
		switch {
		case !open:
		case result.DeferredUntil != nil:
			task.ScheduleTaskInTx(tx, CloseScheduledContract, args, clientSession,
				model.ScheduledContractCloseRunOnce(args.ContractId), task.RunAt(*result.DeferredUntil),
				task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
		default:
			scheduleContractClose(clientSession, tx, args)
		}
	}
	if result.Owner != nil {
		queued, err := model.QueueRegisteredLegacyCloseContractInTx(clientSession, tx, args.ContractId, *result.Owner)
		if err != nil {
			return err
		}
		if !queued {
			task.ScheduleTaskInTx(tx, CloseScheduledContract, args, clientSession,
				model.ScheduledContractCloseRunOnce(args.ContractId), task.RunAt(server.NowUtc().Add(2*time.Second)),
				task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
			return nil
		}
		shard := int(args.ContractId[15]) % model.LegacySettlementShardCount
		scheduleFlushLegacySettlements(clientSession, tx, shard, nil, nil, true, nil, nil)
	}
	return nil
}

type scheduledContractClosureTarget struct{ task.Target }

// RunOnce retains args while advancing its requested wake. Honor that earlier
// deadline in a private call copy; later retry backoff never extends the cap.
func (self *scheduledContractClosureTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	var args CloseScheduledContractArgs
	if err := json.Unmarshal([]byte(queued.ArgsJson), &args); err != nil {
		return nil, nil, err
	}
	if !queued.RunAt.IsZero() && queued.RunAt.Before(args.Deadline) {
		args.Deadline = queued.RunAt
		encoded, err := json.Marshal(&args)
		if err != nil {
			return nil, nil, err
		}
		copyTask := *queued
		copyTask.ArgsJson = string(encoded)
		return self.Target.Run(ctx, &copyTask)
	}
	return self.Target.Run(ctx, queued)
}

func (self *scheduledContractClosureTarget) TaskCompletionOwnershipKeys(t *task.Task, raw string) ([]server.PgOwnershipKey, error) {
	var result CloseScheduledContractResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, err
	}
	if result.Owner == nil {
		return nil, nil
	}
	var args CloseScheduledContractArgs
	if err := json.Unmarshal([]byte(t.ArgsJson), &args); err != nil {
		return nil, err
	}
	var payers, sources []server.Id
	switch result.Owner.Kind {
	case model.ContractCloseOwnerPayerNetwork:
		payers = []server.Id{result.Owner.Id}
	case model.ContractCloseOwnerSourceClient:
		sources = []server.Id{result.Owner.Id}
	default:
		return nil, fmt.Errorf("invalid scheduled close owner")
	}
	keys, err := model.LegacyCloseSettlementQueueOwnershipKeys(payers, sources)
	if err != nil {
		return nil, err
	}
	shard := int(args.ContractId[15]) % model.LegacySettlementShardCount
	return append(keys, task.RunOnceOwnershipKey(task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard)))), nil
}

func NewScheduledContractClosureTaskTarget() task.Target {
	return &scheduledContractClosureTarget{Target: task.NewTaskTargetWithPost(CloseScheduledContract, CloseScheduledContractPost)}
}
