// Optional durable writer groups use the existing direct claim session. They
// refuse immediately and create no persistent queue or additional connection.
package task

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Larger resource sets retain ordinary task/accounting ownership. This caps
// new advisory work per candidate without refusing a previously valid payload.
const TaskClaimGroupKeyLimit = 2

// Keys describe every resource this locked durable payload may write. The
// positive limit certifies how many same-group tasks one preparation can combine
// into a single writer. Multi-resource or uncombined work must return one.
// Empty keys retain ordinary claim behavior. Execution still reads its durable
// authority; these keys never certify successful accounting or finalization.
// The method only decodes its input and must not perform I/O or retain state.
// More than TaskClaimGroupKeyLimit keys retain ordinary ungrouped admission.
type TaskClaimGroupTarget interface {
	TaskClaimGroupIds(argsJson string) (ids []server.Id, maxTasks int)
}

// PostgreSQL's two-int key space is disjoint from the bigint task-owner keys.
// A digest collision can conservatively share admission, never permit overlap.
type taskClaimGroupKey [2]int32

type taskClaimGroupState struct {
	count    int
	maxTasks int
}

type taskClaimGroupQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Aliases use their registered target's canonical name and one resource identity.
func taskClaimGroupLockKey(functionName string, id server.Id) taskClaimGroupKey {
	digest := sha256.Sum256([]byte("task-claim-group-v1\x00" + functionName + "\x00" + string(id[:])))
	return taskClaimGroupKey{int32(binary.BigEndian.Uint32(digest[:4])), int32(binary.BigEndian.Uint32(digest[4:8]))}
}

// Ordinary registrations retain the original four-column claim projection.
func (self *TaskWorker) hasTaskClaimGroups() bool {
	for _, target := range self.targets {
		if _, ok := target.(TaskClaimGroupTarget); ok {
			return true
		}
	}
	return false
}

// Only new members in this same claim pass may share a held key. A refill must
// not overlap another accounting batch still live on its own reentrant session.
func (self *TaskWorker) reserveTaskClaimGroups(ctx context.Context, query taskClaimGroupQuery, guard *taskClaimGuard,
	taskId server.Id, functionName string, argsJson string, passKeys map[taskClaimGroupKey]bool,
) (bool, error) {
	target := self.targets[updateFunctionName(functionName)]
	grouper, ok := target.(TaskClaimGroupTarget)
	if !ok {
		return true, nil
	}
	ids, maxTasks := grouper.TaskClaimGroupIds(argsJson)
	if len(ids) == 0 || len(ids) > TaskClaimGroupKeyLimit {
		return true, nil
	}
	if maxTasks < 1 {
		return false, errors.New("invalid task claim group sharing limit")
	}
	keys := make([]taskClaimGroupKey, 0, len(ids))
	for _, id := range ids {
		if id == (server.Id{}) {
			return false, errors.New("invalid task claim group identity")
		}
		keys = append(keys, taskClaimGroupLockKey(target.TargetFunctionName(), id))
	}
	slices.SortFunc(keys, func(a, b taskClaimGroupKey) int {
		for index := range 2 {
			if a[index] < b[index] {
				return -1
			}
			if a[index] > b[index] {
				return 1
			}
		}
		return 0
	})
	keys = slices.Compact(keys)
	if len(keys) != 1 && maxTasks != 1 {
		return false, errors.New("multi-resource task claim group cannot share a writer")
	}
	for _, key := range keys {
		if state := guard.groupKeyStates[key]; state != nil &&
			(!passKeys[key] || state.maxTasks != maxTasks || state.count >= maxTasks) {
			return false, nil
		}
	}
	if guard.groupKeyStates == nil {
		guard.groupKeyStates = map[taskClaimGroupKey]*taskClaimGroupState{}
		guard.taskGroupKeys = map[server.Id][]taskClaimGroupKey{}
	}
	acquiredKeys := make([]taskClaimGroupKey, 0, len(keys))
	for _, key := range keys {
		if guard.groupKeyStates[key] != nil {
			continue
		}
		var acquired bool
		if err := query.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::integer,$2::integer)`, key[0], key[1]).Scan(&acquired); err != nil {
			return false, err
		}
		if !acquired {
			// Only a known refusal permits releasing this candidate's new keys.
			// Any failed/ambiguous call leaves the session guard responsible.
			for _, acquiredKey := range acquiredKeys {
				if err := releaseTaskClaimGroupKey(ctx, query, acquiredKey); err != nil {
					return false, err
				}
				delete(guard.groupKeyStates, acquiredKey)
				delete(passKeys, acquiredKey)
			}
			return false, nil
		}
		guard.groupKeyStates[key] = &taskClaimGroupState{maxTasks: maxTasks}
		passKeys[key] = true
		acquiredKeys = append(acquiredKeys, key)
	}
	for _, key := range keys {
		guard.groupKeyStates[key].count++
	}
	guard.taskGroupKeys[taskId] = keys
	return true, nil
}

// Retire a key only after its exact server-side unlock is acknowledged.
func releaseTaskClaimGroupKey(ctx context.Context, query taskClaimGroupQuery, key taskClaimGroupKey) error {
	var unlocked bool
	if err := query.QueryRow(ctx, `SELECT pg_advisory_unlock($1::integer,$2::integer)`, key[0], key[1]).Scan(&unlocked); err != nil {
		return err
	}
	if !unlocked {
		return errors.New("task claim group lock was not held")
	}
	return nil
}

// No caller retries a partial/ambiguous retirement. The collector retains its
// entire direct session until live functions and committed posts have joined.
func (self *taskClaimGuard) retireTaskWithQuery(ctx context.Context, query taskClaimGroupQuery, taskId server.Id) error {
	var unlocked bool
	if err := query.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(taskId)).Scan(&unlocked); err != nil {
		return err
	}
	if !unlocked {
		return fmt.Errorf("task advisory lock was not held for %s", taskId)
	}
	for _, key := range self.taskGroupKeys[taskId] {
		state := self.groupKeyStates[key]
		if state == nil || state.count < 1 {
			return errors.New("task claim group ownership missing")
		}
		if state.count == 1 {
			if err := releaseTaskClaimGroupKey(ctx, query, key); err != nil {
				return err
			}
			delete(self.groupKeyStates, key)
		} else {
			state.count--
		}
	}
	delete(self.taskGroupKeys, taskId)
	delete(self.taskIds, taskId)
	self.releaseAdmission(taskId)
	return nil
}
