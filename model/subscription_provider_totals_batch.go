// Claimed allocations for one provider share one additive accounting transaction.
package model

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

const legacyProviderTotalsBatchLimit = 64

// Keep the durable target name and payload understood by existing workers.
// Only tasks already claimed by this evaluator are eligible for coalescing.
func NewLegacyProviderTotalsTaskTarget() task.Target {
	return &legacyProviderTotalsTaskTarget{Target: task.NewTaskTarget(ApplyLegacyProviderTotals)}
}

// Registration has no mutable coordination state; preparation creates it per pass.
type legacyProviderTotalsTaskTarget struct {
	task.Target
}

// This target's exact credits and applied markers commit during Run. It has no
// post or continuation; completed owners may share only their durable handback.
func (self *legacyProviderTotalsTaskTarget) TaskCompletionBatchEnabled() bool { return true }

// Accounting commits during Run and has no transactional post. The finalizer
// always adds the exact durable queue identity to this empty extra-key set.
func (self *legacyProviderTotalsTaskTarget) TaskCompletionOwnershipKeys(_ *task.Task, _ string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}

// Bounded claim grouping helps one provider form a prepared batch. The complete
// account-key owner remains mandatory during execution, including allocations
// above this optional hint's bound and writers outside the task subsystem.
func (self *legacyProviderTotalsTaskTarget) TaskClaimGroupIds(argsJson string) ([]server.Id, int) {
	payload, err := decodeLegacyProviderTotals(argsJson)
	if err != nil || payload.Applied || len(payload.Totals) > task.TaskClaimGroupKeyLimit {
		return nil, 0
	}
	ids := make([]server.Id, 0, len(payload.Totals))
	for _, total := range payload.Totals {
		ids = append(ids, total.NetworkId)
	}
	if len(ids) == 1 {
		return ids, legacyProviderTotalsBatchLimit
	}
	return ids, 1
}

// Single-provider allocations can share a row update without coupling unrelated
// providers. Malformed, applied and multi-provider tasks retain ordinary execution.
func (self *legacyProviderTotalsTaskTarget) PrepareTaskBatch(tasks []*task.Task) task.Target {
	networkIdTasks := map[server.Id][]*task.Task{}
	for _, queued := range tasks {
		payload, err := decodeLegacyProviderTotals(queued.ArgsJson)
		if err == nil && !payload.Applied && len(payload.Totals) == 1 {
			networkId := payload.Totals[0].NetworkId
			networkIdTasks[networkId] = append(networkIdTasks[networkId], queued)
		}
	}
	taskIdBatches := map[server.Id]*legacyProviderTotalsBatch{}
	for networkId, queued := range networkIdTasks {
		slices.SortFunc(queued, func(a, b *task.Task) int { return a.TaskId.Cmp(b.TaskId) })
		for len(queued) > 1 {
			count := min(len(queued), legacyProviderTotalsBatchLimit)
			batch := &legacyProviderTotalsBatch{
				networkId: networkId,
				done:      make(chan struct{}),
				err:       errors.New("legacy provider total batch did not complete"),
			}
			for _, item := range queued[:count] {
				batch.taskIds = append(batch.taskIds, item.TaskId)
				taskIdBatches[item.TaskId] = batch
			}
			queued = queued[count:]
		}
	}
	return &legacyProviderTotalsBatchTarget{Target: self.Target, taskIdBatches: taskIdBatches}
}

// The adapter only supplies shared work; each ordinary target owns its own result,
// max-time cancellation, private argument handling and subsequent finalization.
type legacyProviderTotalsBatchTarget struct {
	task.Target
	taskIdBatches map[server.Id]*legacyProviderTotalsBatch
}

// No new task or waiting queue is created, and a singleton starts immediately.
func (self *legacyProviderTotalsBatchTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if batch := self.taskIdBatches[queued.TaskId]; batch != nil {
		ctx = context.WithValue(ctx, legacyProviderTotalsBatchKey{}, batch)
	}
	return self.Target.Run(ctx, queued)
}

// The context value is private to this target's invocation-local adapter.
type legacyProviderTotalsBatchKey struct{}

// The first invocation performs the transaction outside the once lock; all others
// observe its committed result or their own cancellation. Closing done publishes err.
type legacyProviderTotalsBatch struct {
	networkId server.Id
	taskIds   []server.Id
	runOnce   sync.Once
	done      chan struct{}
	err       error
}

// Cancellation or an ambiguous commit leaves each durable applied marker as the
// retry authority. A canceled follower cannot cancel the elected execution owner.
func (self *legacyProviderTotalsBatch) apply(ctx context.Context) error {
	identity, ok := task.ExecutionIdentityFromContext(ctx)
	if !ok || !slices.Contains(self.taskIds, identity.TaskId) {
		return errors.New("legacy provider total batch ownership missing")
	}
	owner := false
	self.runOnce.Do(func() { owner = true })
	if owner {
		func() {
			defer close(self.done)
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			ownership, err := readLegacyProviderBatchOwnership(bounded, self.taskIds, self.networkId)
			if err != nil {
				self.err = err
				return
			}
			self.err = runLegacyProviderTotalsOwnedTx(bounded, ownership, func(tx server.PgTx) error {
				return applyLegacyProviderTotalsBatchWithOwnersInTx(bounded, tx, self.taskIds, self.networkId, ownership)
			})
		}()
	}
	select {
	case <-self.done:
		return self.err
	case <-ctx.Done():
		return withLegacyProviderTotalsPhase(legacyProviderTotalsBatchWait, ctx.Err())
	}
}

// Lock exact claimed owners in id order and re-read their durable payloads before
// summing. The account row and every contributing marker change in this transaction.
func applyLegacyProviderTotalsBatchInTx(ctx context.Context, tx server.PgTx, taskIds []server.Id, networkId server.Id) error {
	return applyLegacyProviderTotalsBatchWithOwnersInTx(ctx, tx, taskIds, networkId, nil)
}

// The complete owner includes every member's stored queue identity, independently
// of task id ordering. Revalidate it before any account or applied-marker write.
func applyLegacyProviderTotalsBatchWithOwnersInTx(ctx context.Context, tx server.PgTx, taskIds []server.Id, networkId server.Id, ownership *legacyProviderOwnership) error {
	if len(taskIds) < 2 || len(taskIds) > legacyProviderTotalsBatchLimit || networkId == (server.Id{}) {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, errors.New("invalid legacy provider total batch"))
	}
	if ownership != nil && !server.TxOwnsKeys(tx, ownership.keys()) {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, errors.New("legacy provider total batch queue owners were not admitted"))
	}
	rows, err := tx.Query(ctx, `SELECT task_id,args_json,run_once_key FROM pending_task
        WHERE task_id=ANY($1) AND function_name=$2 ORDER BY task_id FOR UPDATE`,
		taskIds, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName())
	if err != nil {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err)
	}
	defer rows.Close()
	total := legacyProviderTotal{NetworkId: networkId}
	unappliedTaskIds := make([]server.Id, 0, len(taskIds))
	count := 0
	for rows.Next() {
		var taskId server.Id
		var data string
		var runOnceKey *string
		if err := rows.Scan(&taskId, &data, &runOnceKey); err != nil {
			return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err)
		}
		if err := ownership.validateQueue(tx, taskId, runOnceKey); err != nil {
			return err
		}
		count++
		payload, err := decodeLegacyProviderTotals(data)
		if err != nil {
			return withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, err)
		}
		if len(payload.Totals) != 1 || payload.Totals[0].NetworkId != networkId {
			return withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, errors.New("legacy provider total batch allocation changed"))
		}
		if payload.Applied {
			continue
		}
		allocation := payload.Totals[0]
		if math.MaxInt64-total.Bytes < allocation.Bytes || math.MaxInt64-total.Revenue < allocation.Revenue {
			return withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, errors.New("legacy provider total batch overflow"))
		}
		total.Bytes += allocation.Bytes
		total.Revenue += allocation.Revenue
		unappliedTaskIds = append(unappliedTaskIds, taskId)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err)
	}
	if count != len(taskIds) {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, errors.New("legacy provider total batch ownership missing"))
	}
	if len(unappliedTaskIds) == 0 {
		return nil
	}
	if err := writeLegacyProviderTotalInTx(ctx, tx, total); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE pending_task SET args_json=jsonb_set(args_json::jsonb,'{applied}','true'::jsonb)::text WHERE task_id=ANY($1)`, unappliedTaskIds)
	if err != nil {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsAppliedMarker, err)
	}
	if tag.RowsAffected() != int64(len(unappliedTaskIds)) {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsAppliedMarker, errors.New("legacy provider total batch marker ownership missing"))
	}
	return nil
}
