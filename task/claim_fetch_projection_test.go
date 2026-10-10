// A pooled claim session must not reuse another registry's cursor row shape.
package task

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Retain one actual claim session after retiring each exact task. Switching
// registries then forces both projections through the same pgx statement cache
// without depending on which connection a pool happens to return next.
func taskClaimFetchProjectionSequence(t *testing.T, firstGrouped bool) {
	t.Helper()
	taskClaimGroupRun(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := make([]server.Id, 0, 3)
		for index := range 3 {
			ids = append(ids, taskClaimGroupTestSchedule(owner, index, &taskClaimGroupTestArgs{GroupId: server.NewId()}))
		}
		grouped := taskClaimGroupTestWorker(ctx)
		defer grouped.Close()
		plain := NewTaskWorkerWithDefaults(ctx)
		plain.AddTargets(NewTaskTarget(taskClaimGroupTestCall))
		defer plain.Close()
		if !grouped.hasTaskClaimGroups() || plain.hasTaskClaimGroups() {
			t.Fatal("fixture did not construct both real claim-cursor projections")
		}

		var guard *taskClaimGuard
		defer func() { guard.release() }()
		var physicalPid uint32
		for index, useGroups := range []bool{firstGrouped, !firstGrouped, firstGrouped} {
			worker := plain
			if useGroups {
				worker = grouped
			}
			prior := guard
			claimed, retained, _, err := worker.takeTasksWithGuard(ctx, 1, guard, taskClaimOptions{})
			if retained != nil {
				guard = retained
			}
			if err != nil {
				t.Fatalf("claim projection transition %d grouped=%t failed on the shared physical session: %v", index, useGroups, err)
			}
			if guard == nil || prior != nil && guard != prior || len(claimed) != 1 || claimed[ids[index]] == nil {
				t.Fatal("projection transition lost its exact task or retained claim session", index, len(claimed))
			}
			connection := guard.conn.Conn()
			if index == 0 {
				physicalPid = connection.PgConn().PID()
				config := connection.Config()
				if config.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement || config.StatementCacheCapacity <= 0 {
					t.Fatal("fixture did not retain the actual default pgx statement cache")
				}
			} else if connection.PgConn().PID() != physicalPid {
				t.Fatal("projection transition escaped onto a different physical session")
			}
			if claimed[ids[index]].ClaimGeneration != 1 || claimed[ids[index]].RunOnceGeneration != 0 {
				t.Fatal("projection transition did not retain its exact committed claim generation")
			}
			if useGroups != (len(guard.taskGroupKeys[ids[index]]) == 1) {
				t.Fatal("projection transition changed real group admission")
			}
			server.Raise(guard.retireTask(ctx, ids[index]))
			if len(guard.taskIds)+len(guard.taskGroupKeys)+len(guard.groupKeyStates)+len(guard.admissionKVs) != 0 {
				t.Fatal("registry handoff retained an earlier task's execution or group ownership")
			}
		}
		var exact bool
		server.Raise(guard.conn.QueryRow(ctx, `SELECT count(*)=3 AND bool_and(claim_generation=1 AND reschedule_error_count=0)
			FROM pending_task WHERE task_id=ANY($1)`, ids).Scan(&exact))
		if !exact {
			t.Fatal("projection changes retried or skipped an exact durable task claim")
		}
	})
}

func TestTaskClaimFetchProjectionGroupedThenPlain(t *testing.T) {
	taskClaimFetchProjectionSequence(t, true)
}

func TestTaskClaimFetchProjectionPlainThenGrouped(t *testing.T) {
	taskClaimFetchProjectionSequence(t, false)
}
