// A cohort lane claim fills its Run slots from interleaved groups' rows and
// leaves every row it passes untouched. Exact claim boundaries, no timing.
package task

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// One Run refill slot, as a retired slot claims on the production loop.
func taskRunCohortLaneClaim(t testing.TB, ctx context.Context, worker *TaskWorker) map[server.Id]*Task {
	t.Helper()
	claimed, guard, isolated, err := worker.takeTasksWithGuard(ctx, 1, nil,
		taskClaimOptions{ordinaryOnly: true, detachCommittedRead: true, poll: &taskClaimPoll{}, runCohorts: true})
	if guard != nil {
		guard.release()
	}
	if err != nil || isolated {
		t.Fatal("refill claim failed", isolated, err)
	}
	return claimed
}

// An older ordinary backlog fills every ordinary window, and sixty-four empty
// lanes rotate. Within four claims the share lane's turn fills one Run slot
// with all 64 rows of a group whose rows alternate with four other groups'.
// Without the share no turn reaches the lane; without the wider window the
// slot holds one row.
func TestTaskRunCohortShareLaneFillsPastOtherGroupsBehindAnOlderBacklog(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		target := &taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)}
		settings := DefaultTaskWorkerSettings()
		settings.FairClaimFunctions = true
		settings.TargetClaimShares = map[string]int{target.TargetFunctionName(): 2}
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target, NewTaskTarget(claimProfileAllowed), NewTaskTarget(claimProfileExcluded, claimShareTestAliases()...))
		base := server.NowUtc().Truncate(time.Second).Add(-2 * time.Hour)
		backlogIds := map[server.Id]bool{}
		for range 140 {
			backlogIds[ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(base))] = true
		}
		hot := server.NewId()
		others := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId()}
		hotIds, otherIds := map[server.Id]bool{}, []server.Id{}
		for index := range 128 {
			at := RunAt(base.Add(time.Hour + time.Duration(index)*time.Second))
			if index%2 == 0 {
				hotIds[ScheduleTask(taskClaimGroupTestCall, &taskClaimGroupTestArgs{GroupId: hot}, owner, at)] = true
			} else {
				group := others[(index/2)%len(others)]
				otherIds = append(otherIds, ScheduleTask(taskClaimGroupTestCall, &taskClaimGroupTestArgs{GroupId: group}, owner, at))
			}
		}
		before := GetTasks(ctx, otherIds...)
		// The rotation starts just past the share lane and does not reach it.
		lane := slices.Index(worker.claimFunctionNames, target.TargetFunctionName())
		taskFunctionTestTurn(t, worker, worker.claimFunctionNames[(lane+1)%len(worker.claimFunctionNames)])
		backlogClaims, hotClaims := 0, 0
		for claim := range 4 {
			claimed := taskRunCohortLaneClaim(t, ctx, worker)
			backlog, hotCount := 0, 0
			for id := range claimed {
				switch {
				case backlogIds[id]:
					backlog++
				case hotIds[id]:
					hotCount++
				default:
					t.Fatal("a claim took a row of a group without a slot", claim)
				}
			}
			if backlog != 0 {
				backlogClaims++
			}
			if hotCount != 0 {
				hotClaims++
				if hotCount != len(hotIds) || len(taskRunSlots(claimed)) != 1 {
					t.Fatalf("the share turn filled %d of %d interleaved rows in %d slots", hotCount, len(hotIds), len(taskRunSlots(claimed)))
				}
			}
		}
		if hotClaims != 1 || backlogClaims != 3 {
			t.Fatalf("four claims behind the backlog gave the share lane %d turns and the backlog %d", hotClaims, backlogClaims)
		}
		if !reflect.DeepEqual(before, GetTasks(ctx, otherIds...)) {
			t.Fatal("a passed row of another group changed")
		}
	})
}

// Another session holds the leading group's key. A lane claim tries that group
// once, passes its later rows without a statement, and fills its slot with the
// next group's rows that the held group's rows interleave.
func TestTaskRunCohortLanePassesAHeldGroupAfterOneRefusal(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		target := &taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)}
		settings := DefaultTaskWorkerSettings()
		settings.FairClaimFunctions = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		held, open := server.NewId(), server.NewId()
		base := server.NowUtc().Truncate(time.Second).Add(-time.Hour)
		heldIds, openIds := []server.Id{}, map[server.Id]bool{}
		// Eight held rows lead; then the two groups alternate.
		for index := range 72 {
			at := RunAt(base.Add(time.Duration(index) * time.Second))
			if 8 <= index && index%2 == 1 {
				openIds[ScheduleTask(taskClaimGroupTestCall, &taskClaimGroupTestArgs{GroupId: open}, owner, at)] = true
			} else {
				heldIds = append(heldIds, ScheduleTask(taskClaimGroupTestCall, &taskClaimGroupTestArgs{GroupId: held}, owner, at))
			}
		}
		before := GetTasks(ctx, heldIds...)
		probe, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer probe.Release()
		key := taskClaimGroupLockKey(target.TargetFunctionName(), held)
		server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_lock($1::integer,$2::integer)`, key[0], key[1]))
		defer func() {
			server.RaisePgResult(probe.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1::integer,$2::integer)`, key[0], key[1]))
		}()
		heldAttempts := 0
		worker.claimQueueAdmission = func(id server.Id, _ bool) {
			if slices.Contains(heldIds, id) {
				heldAttempts++
			}
		}
		taskFunctionTestTurn(t, worker, target.TargetFunctionName())
		claimed := taskRunCohortLaneClaim(t, ctx, worker)
		for id := range claimed {
			if !openIds[id] {
				t.Fatal("the claim took a row of the held group")
			}
		}
		if len(claimed) != len(openIds) || len(taskRunSlots(claimed)) != 1 {
			t.Errorf("the slot filled %d of %d rows the held group interleaves", len(claimed), len(openIds))
		}
		if heldAttempts != 1 {
			t.Errorf("the held group's rows took %d task locks instead of one", heldAttempts)
		}
		if !reflect.DeepEqual(before, GetTasks(ctx, heldIds...)) {
			t.Fatal("a passed row of the held group changed")
		}
	})
}
