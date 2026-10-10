// Startup enumerates every nonterminal contract independently of sweep cursors.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

const startupContractClosurePageSize = 1024
const startupContractClosureMaxPageSize = 10000
const startupContractClosurePublicationSize = 256

// One timestamp supplies the default lifetime for this complete startup pass.
type ScheduleOpenContractClosuresArgs struct {
	PageSize  int       `json:"page_size"`
	StartedAt time.Time `json:"started_at"`
}

type ScheduledContractClose struct {
	ContractId server.Id `json:"contract_id"`
	Deadline   time.Time `json:"deadline"`
}

type ScheduleOpenContractClosuresResult struct{}

func ScheduleOpenContractClosuresOnStartup(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures,
		&ScheduleOpenContractClosuresArgs{PageSize: startupContractClosurePageSize, StartedAt: server.NowUtc().Truncate(time.Microsecond)},
		clientSession, task.RunOnce("schedule_open_contract_closures_on_startup"), task.RunAt(server.NowUtc()))
}

// Every page belongs to this invocation. The old queued After field is ignored;
// retry starts at the head and coalesces any children already committed.
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
	busy := false
	for {
		if err := clientSession.Ctx.Err(); err != nil {
			return nil, err
		}
		contracts := make([]ScheduledContractClose, 0, pageSize)
		server.MaintenanceDb(clientSession.Ctx, func(conn server.PgConn) {
			rows, err := conn.Query(clientSession.Ctx, `SELECT contract_id,expiration_time FROM transfer_contract
				WHERE outcome IS NULL AND contract_id>$2 ORDER BY contract_id LIMIT $1`, pageSize, lastContractId)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contract ScheduledContractClose
					var expiration *time.Time
					server.Raise(rows.Scan(&contract.ContractId, &expiration))
					contract.Deadline = args.StartedAt.Add(model.DefaultContractExpiration)
					if expiration != nil {
						contract.Deadline = *expiration
					}
					contracts = append(contracts, contract)
				}
			})
		}, server.OptNoRetry())
		// Release the maintenance connection before ordinary queue writes.
		// A busy group cannot hold back independent later pages. Any refusal
		// retains this scanner for a full retry, including all skipped groups.
		for offset := 0; offset < len(contracts); offset += startupContractClosurePublicationSize {
			chunk := contracts[offset:min(offset+startupContractClosurePublicationSize, len(contracts))]
			keys := make([]server.PgOwnershipKey, 0, len(chunk))
			for _, contract := range chunk {
				keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", contract.ContractId)))
			}
			admitted := server.TryOwnedTx(clientSession.Ctx, keys, func(tx server.PgTx) {
				server.BatchInTx(clientSession.Ctx, tx, func(batch server.PgBatch) {
					for _, contract := range chunk {
						args := &CloseScheduledContractArgs{Private: true, ScheduledContractClose: contract}
						task.QueueTaskInBatch(tx, batch, CloseScheduledContract, args, clientSession,
							task.RunOnce("close_scheduled_contract", contract.ContractId), task.RunAt(contract.Deadline),
							task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
					}
				})
			}, server.TxReadCommitted, server.OptNoRetry())
			busy = busy || !admitted
		}
		if len(contracts) == 0 {
			break
		}
		lastContractId = contracts[len(contracts)-1].ContractId
	}
	if busy {
		return nil, fmt.Errorf("startup contract scan child publication ownership is busy")
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

type CloseScheduledContractResult struct {
	Owner   *model.ContractCloseOwner `json:"owner,omitempty"`
	RetryAt *time.Time                `json:"retry_at,omitempty"`
}

func scheduleContractClose(clientSession *session.ClientSession, tx server.PgTx, args *CloseScheduledContractArgs) {
	task.ScheduleTaskInTx(tx, CloseScheduledContract, args, clientSession,
		task.RunOnce("close_scheduled_contract", args.ContractId), task.RunAt(args.Deadline),
		task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
}

// An early queue wake does not grant deadline authority. Normal task retries
// retain failed closes; success means terminal or a durable financial handoff.
func CloseScheduledContract(args *CloseScheduledContractArgs, clientSession *session.ClientSession) (*CloseScheduledContractResult, error) {
	if args == nil || !args.Private || args.ContractId == (server.Id{}) || args.Deadline.IsZero() {
		return nil, fmt.Errorf("invalid scheduled contract close")
	}
	if server.NowUtc().Before(args.Deadline) {
		return &CloseScheduledContractResult{RetryAt: &args.Deadline}, nil
	}
	owner, err := model.CloseContractAtDeadline(clientSession.Ctx, args.ContractId, args.Deadline)
	return &CloseScheduledContractResult{Owner: owner}, err
}

// Repair only retained routing hints in the same commit as their owner wake.
// Busy or changed custody yields a new child without changing its deadline.
func CloseScheduledContractPost(args *CloseScheduledContractArgs, result *CloseScheduledContractResult,
	clientSession *session.ClientSession, tx server.PgTx) error {
	if result.RetryAt != nil {
		scheduleContractClose(clientSession, tx, args)
	}
	if result.Owner != nil {
		queued, err := model.QueueRegisteredLegacyCloseContractInTx(clientSession, tx, args.ContractId, *result.Owner)
		if err != nil {
			return err
		}
		if !queued {
			task.ScheduleTaskInTx(tx, CloseScheduledContract, args, clientSession,
				task.RunOnce("close_scheduled_contract", args.ContractId), task.RunAt(server.NowUtc().Add(2*time.Second)),
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
