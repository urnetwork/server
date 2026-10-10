// Explicit prepared cohorts retain many durable identities in one Run slot.
package task

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/urnetwork/server/v2026"
)

// TaskRunCohortTarget certifies that one prepared adapter combines these members
// into one execution owner. Members have no posts or continuations and retain
// their individual durable payloads, results and completion generations. The
// method only decodes arguments; zero identity and limit mean ordinary work.
// Finite EvalTasks never uses this Run-only extension of a physical slot.
type TaskRunCohortTarget interface {
	TaskRunCohort(argsJson string) (id server.Id, limit int)
}

type taskRunCohortKey struct {
	functionName string
	id           server.Id
}

// A token belongs to one acknowledged claim. A later GetTasks read cannot
// regroup a changed payload or merge members from another claim transaction.
type taskRunCohort struct {
	key   taskRunCohortKey
	limit int
	count int
}

// Admission remains ordinary for isolated priority/max-time work. Cohort
// registration must also provide the existing grouping, preparation and exact
// completion contracts; a partial opt-in fails before any candidate lock.
func (self *TaskWorker) claimRunCohort(functionName, argsJson string, priority, maxTimeSeconds int) (taskRunCohortKey, int, error) {
	if priority != DefaultPriority || DefaultMaxTime < time.Duration(maxTimeSeconds)*time.Second {
		return taskRunCohortKey{}, 0, nil
	}
	target := self.targets[updateFunctionName(functionName)]
	cohort, ok := target.(TaskRunCohortTarget)
	if !ok {
		return taskRunCohortKey{}, 0, nil
	}
	_, grouped := target.(TaskClaimGroupTarget)
	_, prepared := target.(TaskBatchPreparer)
	_, owned := target.(TaskCompletionOwnershipTarget)
	completed, completion := target.(TaskCompletionBatchTarget)
	if !grouped || !prepared || !owned || !completion || !completed.TaskCompletionBatchEnabled() {
		return taskRunCohortKey{}, 0, errors.New("Run cohort requires grouped preparation and batched completion")
	}
	id, limit := cohort.TaskRunCohort(argsJson)
	if id == (server.Id{}) && limit == 0 {
		return taskRunCohortKey{}, 0, nil
	}
	if id == (server.Id{}) || limit < 2 || limit > taskCompletionBatchLimit {
		return taskRunCohortKey{}, 0, errors.New("invalid Run cohort identity or member bound")
	}
	return taskRunCohortKey{functionName: target.TargetFunctionName(), id: id}, limit, nil
}

// Active slots count owners, while the member maps retain every lease and
// exact task lock. Only the Run collector changes remaining or retires a slot.
type taskRunSlot struct {
	id        server.Id
	tasks     []*Task
	remaining int
}

func taskRunSlots(tasks map[server.Id]*Task) []*taskRunSlot {
	cohorts := map[*taskRunCohort]*taskRunSlot{}
	slots := []*taskRunSlot{}
	for _, queued := range tasks {
		var slot *taskRunSlot
		if queued.runCohort != nil {
			slot = cohorts[queued.runCohort]
		}
		if slot == nil {
			slot = &taskRunSlot{}
			slots = append(slots, slot)
			if queued.runCohort != nil {
				cohorts[queued.runCohort] = slot
			}
		}
		slot.tasks = append(slot.tasks, queued)
	}
	for _, slot := range slots {
		slices.SortFunc(slot.tasks, func(a, b *Task) int { return a.TaskId.Cmp(b.TaskId) })
		slot.id, slot.remaining = slot.tasks[0].TaskId, len(slot.tasks)
	}
	return slots
}

// The cohort has one minimum-member deadline and one goroutine. The first
// ordinary invocation owns the shared prepared transaction; later invocations
// observe that same acknowledgement through the adapter. No member gets a fresh
// extension of the cohort budget or publishes a result before all members join.
func (self *TaskWorker) executeTaskRunCohort(ctx context.Context, slot *taskRunSlot, targets map[string]Target, admissions taskExecutionAdmissions) (results []*taskExecutionResult) {
	if len(slot.tasks) < 2 || len(slot.tasks) > taskCompletionBatchLimit {
		panic("invalid Run cohort execution size")
	}
	limit := max(DefaultMaxTime, time.Duration(slot.tasks[0].RunMaxTimeSeconds)*time.Second)
	for _, queued := range slot.tasks[1:] {
		limit = min(limit, max(DefaultMaxTime, time.Duration(queued.RunMaxTimeSeconds)*time.Second))
	}
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	stopAfterRoot := context.AfterFunc(self.ctx, cancel)
	defer stopAfterRoot()
	stopAfterDrain := context.AfterFunc(self.drainCtx, cancel)
	defer stopAfterDrain()
	results = make([]*taskExecutionResult, 0, len(slot.tasks))
	for _, queued := range slot.tasks {
		result := self.executeTask(bounded, queued, targets[queued.FunctionName])
		if reservation := admissions[queued.TaskId]; reservation != nil {
			reservation.release()
			delete(admissions, queued.TaskId)
		}
		results = append(results, result)
	}
	return results
}

// Successful members use the existing exact-generation batch move. Errors and
// canceled members keep their ordinary handback policy inside one complete-key
// owner and one finalization deadline. No known or ambiguous failure replays the
// cohort as independent finalizations with fresh per-member budgets.
func (self *TaskWorker) finalizeTaskRunCohort(results []*taskExecutionResult) (returnErr error) {
	return self.finalizeTaskRunCohortWithGuard(results, nil)
}

func (self *TaskWorker) finalizeTaskRunCohortWithGuard(results []*taskExecutionResult, guard *taskClaimGuard) (returnErr error) {
	delegated := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if !delegated {
				self.observeTaskFinalizationFailure(results, recovered)
			}
			panic(recovered)
		}
		if !delegated {
			self.observeTaskFinalizationFailure(results, returnErr)
		}
	}()
	if len(results) < 2 || len(results) > taskCompletionBatchLimit {
		return errors.New("invalid Run cohort completion size")
	}
	allSucceeded := true
	keys := []server.PgOwnershipKey{}
	seen := map[server.Id]bool{}
	for _, result := range results {
		if result == nil || result.task == nil || seen[result.task.TaskId] {
			return errors.New("invalid Run cohort completion member")
		}
		seen[result.task.TaskId] = true
		target, ok := self.targets[result.task.FunctionName].(TaskCompletionBatchTarget)
		if !ok || !target.TaskCompletionBatchEnabled() {
			return errors.New("Run cohort lost its completion certificate")
		}
		memberKeys, owned, err := taskCompletionOwnershipKeys(self.targets[result.task.FunctionName], result.task, result.resultJson, result.err == nil)
		if err != nil {
			return err
		}
		if !owned {
			return errors.New("Run cohort completion requires every exact queue owner")
		}
		keys = append(keys, memberKeys...)
		allSucceeded = allSucceeded && self.canBatchTaskCompletion(result)
	}
	if allSucceeded {
		delegated = true
		_, err := self.finalizeTaskBatchWithGuard(results, guard)
		return err
	}
	timeout := self.settings.FinalizeTimeout
	if timeout <= 0 {
		timeout = DefaultTaskFinalizeTimeout
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(self.ctx), timeout)
	defer cancel()
	server.HandleError(func() {
		guard.finalizeOwnedTx(bounded, keys, func(tx server.PgTx) {
			for _, result := range results {
				posts, postRescheduled := self.finalizeTaskInTx(bounded, tx, result)
				if len(posts) != 0 || postRescheduled {
					panic(errors.New("Run cohort produced an uncertified post"))
				}
			}
		})
		if self.completionBatchCommitReturned != nil {
			self.completionBatchCommitReturned()
		}
	}, func(err error) { returnErr = err })
	return
}
