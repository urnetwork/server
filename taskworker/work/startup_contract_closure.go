// Startup enumerates every nonterminal contract independently of sweep cursors.
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

const startupContractClosurePageSize = 10000
const startupContractClosurePublicationSize = 256

// One timestamp caps every page in this startup pass; the cursor is only a PK.
type ScheduleOpenContractClosuresArgs struct {
	StartedAt time.Time  `json:"started_at"`
	After     *server.Id `json:"after,omitempty"`
}

type ScheduledContractClose struct {
	ContractId server.Id `json:"contract_id"`
	Deadline   time.Time `json:"deadline"`
}

type ScheduleOpenContractClosuresResult struct {
	Contracts []ScheduledContractClose `json:"contracts"`
	After     *server.Id               `json:"after,omitempty"`
}

func ScheduleOpenContractClosuresOnStartup(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleOpenContractClosuresPage(clientSession, tx, &ScheduleOpenContractClosuresArgs{StartedAt: server.NowUtc().Truncate(time.Microsecond)})
}

func scheduleOpenContractClosuresPage(clientSession *session.ClientSession, tx server.PgTx, args *ScheduleOpenContractClosuresArgs) {
	task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, args, clientSession,
		task.RunOnce("schedule_open_contract_closures"), task.RunAt(server.NowUtc()), task.MaxTime(30*time.Second))
}

// No lifecycle, report, funding or intent shape is excluded. Generated open
// excludes disputes, so outcome NULL is the complete nonterminal predicate.
func ScheduleOpenContractClosures(args *ScheduleOpenContractClosuresArgs, clientSession *session.ClientSession) (*ScheduleOpenContractClosuresResult, error) {
	if args == nil || args.StartedAt.IsZero() {
		return nil, fmt.Errorf("startup contract scan has no start time")
	}
	result := &ScheduleOpenContractClosuresResult{}
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
		query := `SELECT contract_id,expiration_time FROM transfer_contract
			WHERE outcome IS NULL ORDER BY contract_id LIMIT 10000`
		var params []any
		if args.After != nil {
			query = `SELECT contract_id,expiration_time FROM transfer_contract
				WHERE outcome IS NULL AND contract_id>$1 ORDER BY contract_id LIMIT 10000`
			params = append(params, *args.After)
		}
		rows, err := conn.Query(clientSession.Ctx, query, params...)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var contract ScheduledContractClose
				var expiration *time.Time
				server.Raise(rows.Scan(&contract.ContractId, &expiration))
				contract.Deadline = args.StartedAt.Add(model.DefaultContractExpiration)
				if expiration != nil && expiration.Before(contract.Deadline) {
					contract.Deadline = *expiration
				}
				result.Contracts = append(result.Contracts, contract)
			}
		})
	})
	// Release the discovery connection before queue ownership. Each chunk
	// commits through the ordinary coalescing writer; a failed page retries
	// these stable keys before it can advance its durable scan cursor.
	for offset := 0; offset < len(result.Contracts); offset += startupContractClosurePublicationSize {
		contracts := result.Contracts[offset:min(offset+startupContractClosurePublicationSize, len(result.Contracts))]
		keys := make([]server.PgOwnershipKey, 0, len(contracts))
		for _, contract := range contracts {
			keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", contract.ContractId)))
		}
		server.OwnedTx(clientSession.Ctx, keys, func(tx server.PgTx) {
			server.BatchInTx(clientSession.Ctx, tx, func(batch server.PgBatch) {
				for _, contract := range contracts {
					args := &CloseScheduledContractArgs{Private: true, ScheduledContractClose: contract}
					task.QueueTaskInBatch(tx, batch, CloseScheduledContract, args, clientSession,
						task.RunOnce("close_scheduled_contract", contract.ContractId), task.RunAt(contract.Deadline),
						task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
				}
			})
		}, server.TxReadCommitted, server.OptNoRetry())
	}
	if len(result.Contracts) == startupContractClosurePageSize {
		last := result.Contracts[len(result.Contracts)-1].ContractId
		result.After = &last
	}
	return result, nil
}

// All child chunks committed during Run. Only successful completion advances
// the existing startup RunOnce key; failures retain the same page for retry.
func ScheduleOpenContractClosuresPost(args *ScheduleOpenContractClosuresArgs, result *ScheduleOpenContractClosuresResult,
	clientSession *session.ClientSession, tx server.PgTx) error {
	if result.After != nil {
		scheduleOpenContractClosuresPage(clientSession, tx, &ScheduleOpenContractClosuresArgs{StartedAt: args.StartedAt, After: result.After})
	}
	return nil
}

type startupContractClosureTarget struct{ task.Target }

// Keep the scanner's pending and finished owners during success and retry.
// Child publication already committed under its separate bounded queue owners.
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

// Registration also wakes because retained intents may lack current owner
// hints. It alone repairs that metadata; this task never rewrites an intent.
func CloseScheduledContractPost(args *CloseScheduledContractArgs, result *CloseScheduledContractResult,
	clientSession *session.ClientSession, tx server.PgTx) error {
	if result.RetryAt != nil {
		scheduleContractClose(clientSession, tx, args)
	}
	if result.Owner != nil {
		model.QueueLegacyCloseSettlementsInTx(clientSession, tx, *result.Owner)
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
