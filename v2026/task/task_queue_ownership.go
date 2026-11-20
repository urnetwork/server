// Queue writers share the exact durable deduplication identity. Task ids name
// only rows without a run-once key; invocation arguments never reconstruct it.
package task

import (
	"errors"

	"github.com/urnetwork/server/v2026"
)

const taskRunOnceOwnershipDomain = "pending_task/run_once"
const taskIdentityOwnershipDomain = "pending_task/task_id"

// Use the stored key byte-for-byte, including its complete JSON representation.
// A non-deduplicated row is owned by its actual nonzero task identity instead.
func PendingTaskOwnershipKey(taskId server.Id, storedRunOnceKey *string) server.PgOwnershipKey {
	if storedRunOnceKey != nil {
		return server.NewPgOwnershipKeyFromString(taskRunOnceOwnershipDomain, *storedRunOnceKey)
	}
	return server.NewPgOwnershipKey(taskIdentityOwnershipDomain, taskId)
}

// Producers can declare the same identity before preparing their new task id.
func RunOnceOwnershipKey(runOnce *RunOnceOption) server.PgOwnershipKey {
	if runOnce == nil {
		panic(errors.New("task queue ownership requires a run-once key"))
	}
	key := runOnce.String()
	return PendingTaskOwnershipKey(server.Id{}, &key)
}

// Loaded task records represent a null durable key as the empty string.
func taskQueueOwnershipKey(taskId server.Id, runOnceKey string) server.PgOwnershipKey {
	if runOnceKey == "" {
		return PendingTaskOwnershipKey(taskId, nil)
	}
	return PendingTaskOwnershipKey(taskId, &runOnceKey)
}
