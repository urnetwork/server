// Batch preparation shares work only among tasks already owned by one evaluator.
package task

import (
	"maps"

	"github.com/urnetwork/server/v2026"
)

// A target may return an invocation-local adapter for its already-claimed tasks.
// Preparation performs no I/O and must not mutate the registered target or tasks.
// Every task still runs through its ordinary cancellation and finalization owner.
type TaskBatchPreparer interface {
	PrepareTaskBatch([]*Task) Target
}

// Aliases share one preparation, while unrelated targets retain ordinary dispatch.
func (self *TaskWorker) prepareTaskBatchTargets(tasks map[server.Id]*Task) map[string]Target {
	targetNameTasks := map[string][]*Task{}
	targetNamePreparers := map[string]TaskBatchPreparer{}
	for _, queued := range tasks {
		target := self.targets[queued.FunctionName]
		if preparer, ok := target.(TaskBatchPreparer); ok {
			name := target.TargetFunctionName()
			targetNameTasks[name] = append(targetNameTasks[name], queued)
			targetNamePreparers[name] = preparer
		}
	}
	if len(targetNameTasks) == 0 {
		return self.targets
	}
	targetNameTargets := map[string]Target{}
	for name, queued := range targetNameTasks {
		targetNameTargets[name] = targetNamePreparers[name].PrepareTaskBatch(queued)
	}
	targets := maps.Clone(self.targets)
	for name, target := range targets {
		if prepared, ok := targetNameTargets[target.TargetFunctionName()]; ok {
			targets[name] = prepared
		}
	}
	return targets
}
