// Durable claim groups isolate one writer while unrelated work stays available.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func taskClaimGroupRun(t *testing.T, run func(testing.TB)) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, run)
}

type taskClaimGroupTestArgs struct {
	GroupId  server.Id
	GroupIds []server.Id
	MaxTasks int
}

func taskClaimGroupTestCall(_ *taskClaimGroupTestArgs, _ *session.ClientSession) (*struct{}, error) {
	return &struct{}{}, nil
}

// The method is deliberately declared without referring to the new interface,
// so the causal control also compiles on the ungrouped claim implementation.
type taskClaimGroupTestTarget struct{ Target }

func (self *taskClaimGroupTestTarget) TaskClaimGroupIds(argsJson string) ([]server.Id, int) {
	var args taskClaimGroupTestArgs
	server.Raise(json.Unmarshal([]byte(argsJson), &args))
	if len(args.GroupIds) != 0 {
		return args.GroupIds, args.MaxTasks
	}
	return []server.Id{args.GroupId}, 64
}

func taskClaimGroupTestSchedule(clientSession *session.ClientSession, index int, args *taskClaimGroupTestArgs) server.Id {
	return ScheduleTask(taskClaimGroupTestCall, args, clientSession,
		RunAt(server.NowUtc().Add(-time.Hour+time.Duration(index)*time.Minute)))
}

func taskClaimGroupTestWorker(ctx context.Context) *TaskWorker {
	worker := NewTaskWorkerWithDefaults(ctx)
	worker.AddTargets(&taskClaimGroupTestTarget{NewTaskTarget(taskClaimGroupTestCall)})
	return worker
}

// The real server executes the selected command successfully before its local
// reply is discarded. No production hook or mock advisory table is involved.
type taskClaimGroupLostReplyQuery struct {
	query     taskClaimGroupQuery
	statement string
	after     int
	calls     int
	err       error
}

func (self *taskClaimGroupLostReplyQuery) QueryRow(ctx context.Context, statement string, args ...any) pgx.Row {
	row := self.query.QueryRow(ctx, statement, args...)
	if statement == self.statement {
		self.calls++
		if self.calls == self.after {
			return &taskClaimGroupLostReplyRow{Row: row, err: self.err}
		}
	}
	return row
}

type taskClaimGroupLostReplyRow struct {
	pgx.Row
	err error
}

func (self *taskClaimGroupLostReplyRow) Scan(dest ...any) error {
	if err := self.Row.Scan(dest...); err != nil {
		return err
	}
	if len(dest) != 1 || !*(dest[0].(*bool)) {
		return errors.New("controlled advisory operation did not succeed")
	}
	return self.err
}

// Probe group keys directly: a retained per-task bigint lock cannot satisfy
// this oracle. Release every successfully probed key before any assertion.
func taskClaimGroupRequireKeys(t testing.TB, ctx context.Context, probe server.PgConn, keys []taskClaimGroupKey, held bool) {
	t.Helper()
	for _, key := range keys {
		var acquired bool
		server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::integer,$2::integer)`, key[0], key[1]).Scan(&acquired))
		if acquired {
			server.Raise(releaseTaskClaimGroupKey(ctx, probe, key))
		}
		if acquired == held {
			t.Fatal("independent provider-key ownership differs")
		}
	}
}

func TestTaskClaimGroupLostAcquireReplyRetainsAllSessionKeys(t *testing.T) {
	taskClaimGroupRun(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		worker := taskClaimGroupTestWorker(ctx)
		defer worker.Close()
		liveId, firstId, lastId := server.NewId(), server.NewId(), server.NewId()
		ownedId := taskClaimGroupTestSchedule(clientSession, 0, &taskClaimGroupTestArgs{GroupId: liveId})
		candidateId := taskClaimGroupTestSchedule(clientSession, 1, &taskClaimGroupTestArgs{GroupIds: []server.Id{firstId, lastId}, MaxTasks: 1})
		owned, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || owned[ownedId] == nil {
			t.Fatal("live owning claim missing", err)
		}
		candidate := GetTasks(ctx, candidateId)[candidateId]
		lost := errors.New("controlled provider-key acquisition reply loss")
		query := &taskClaimGroupLostReplyQuery{query: guard.conn, statement: `SELECT pg_try_advisory_lock($1::integer,$2::integer)`, after: 2, err: lost}
		admitted, err := worker.reserveTaskClaimGroups(ctx, query, guard, candidateId, candidate.FunctionName, candidate.ArgsJson, map[taskClaimGroupKey]bool{})
		if admitted || !errors.Is(err, lost) || query.calls != 2 {
			t.Fatal("lost acquisition reply was retried or acknowledged", query.calls, err)
		}
		probe, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer probe.Release()
		name := NewTaskTarget(taskClaimGroupTestCall).TargetFunctionName()
		keys := []taskClaimGroupKey{taskClaimGroupLockKey(name, liveId), taskClaimGroupLockKey(name, firstId), taskClaimGroupLockKey(name, lastId)}
		taskClaimGroupRequireKeys(t, ctx, probe, keys, true)
		guard.release()
		taskClaimGroupRequireKeys(t, ctx, probe, keys, false)
	})
}

func TestTaskClaimGroupLostRetirementReplyRetainsSiblingKeys(t *testing.T) {
	taskClaimGroupRun(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		worker := taskClaimGroupTestWorker(ctx)
		defer worker.Close()
		firstId, lastId, liveId := server.NewId(), server.NewId(), server.NewId()
		retiredId := taskClaimGroupTestSchedule(clientSession, 0, &taskClaimGroupTestArgs{GroupIds: []server.Id{firstId, lastId}, MaxTasks: 1})
		ownedId := taskClaimGroupTestSchedule(clientSession, 1, &taskClaimGroupTestArgs{GroupId: liveId})
		owned, guard, err := worker.takeTasks(2)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(owned) != 2 || owned[retiredId] == nil || owned[ownedId] == nil {
			t.Fatal("exact retirement owners missing", err)
		}
		retiredKeys := append([]taskClaimGroupKey(nil), guard.taskGroupKeys[retiredId]...)
		if len(retiredKeys) != 2 {
			t.Fatal("multi-provider owner lacks both keys")
		}
		lost := errors.New("controlled provider-key retirement reply loss")
		query := &taskClaimGroupLostReplyQuery{query: guard.conn, statement: `SELECT pg_advisory_unlock($1::integer,$2::integer)`, after: 1, err: lost}
		err = guard.retireTaskWithQuery(ctx, query, retiredId)
		if !errors.Is(err, lost) || query.calls != 1 {
			t.Fatal("lost retirement reply was retried or acknowledged", query.calls, err)
		}
		probe, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer probe.Release()
		liveKey := taskClaimGroupLockKey(NewTaskTarget(taskClaimGroupTestCall).TargetFunctionName(), liveId)
		taskClaimGroupRequireKeys(t, ctx, probe, retiredKeys[:1], false)
		taskClaimGroupRequireKeys(t, ctx, probe, []taskClaimGroupKey{retiredKeys[1], liveKey}, true)
		guard.release()
		taskClaimGroupRequireKeys(t, ctx, probe, []taskClaimGroupKey{retiredKeys[0], retiredKeys[1], liveKey}, false)
	})
}

// A reused maintenance session must not reenter a provider group still held by
// one old member. Its unrelated free slot may refill normally.
func TestTaskClaimGroupRefillWaitsForLastOldMember(t *testing.T) {
	taskClaimGroupRun(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		first, peer := taskClaimGroupTestWorker(ctx), taskClaimGroupTestWorker(ctx)
		defer first.Close()
		defer peer.Close()
		groupId := server.NewId()
		ids := []server.Id{}
		for index := range 4 {
			key := groupId
			if index == 3 {
				key = server.NewId()
			}
			ids = append(ids, taskClaimGroupTestSchedule(clientSession, index, &taskClaimGroupTestArgs{GroupId: key}))
		}
		owned, guard, err := first.takeTasks(2)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(owned) != 2 {
			t.Fatal("first group claim failed", err)
		}
		server.Raise(guard.retireTask(ctx, ids[0]))
		refilled, reused, _, err := first.takeTasksWithGuard(ctx, 1, guard, taskClaimOptions{})
		if err != nil || reused != guard || len(refilled) != 1 || refilled[ids[3]] == nil {
			t.Fatal("refill reentered a live group or hid independent capacity", len(refilled), err)
		}
		blocked, blockedGuard, err := peer.takeTasks(1)
		if blockedGuard != nil {
			defer blockedGuard.release()
		}
		if err != nil || len(blocked) != 0 {
			t.Fatal("partial retirement released its live group", len(blocked), err)
		}
		server.Raise(guard.retireTask(ctx, ids[1]))
		next, nextGuard, err := peer.takeTasks(1)
		if nextGuard != nil {
			defer nextGuard.release()
		}
		if err != nil || len(next) != 1 || next[ids[2]] == nil {
			t.Fatal("last retirement retained the unused group", len(next), err)
		}
	})
}

// A refused multi-provider owner releases only keys it newly acquired. It does
// not retain an unrelated provider or disturb an existing owner's session.
func TestTaskClaimGroupPartialMultiKeyRefusalReleasesNewKeys(t *testing.T) {
	taskClaimGroupRun(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		first, peer := taskClaimGroupTestWorker(ctx), taskClaimGroupTestWorker(ctx)
		defer first.Close()
		defer peer.Close()
		name := NewTaskTarget(taskClaimGroupTestCall).TargetFunctionName()
		heldId, freeId := server.NewId(), server.NewId()
		// Select an exact key order; no goroutine race determines which key the
		// refused candidate acquires first.
		held, free := taskClaimGroupLockKey(name, heldId), taskClaimGroupLockKey(name, freeId)
		if held == free {
			t.Fatal("test identities collided in their advisory digest")
		}
		if free[0] > held[0] || free[0] == held[0] && free[1] > held[1] {
			heldId, freeId = freeId, heldId
		}
		firstId := taskClaimGroupTestSchedule(clientSession, 0, &taskClaimGroupTestArgs{GroupId: heldId})
		multiId := taskClaimGroupTestSchedule(clientSession, 1, &taskClaimGroupTestArgs{GroupIds: []server.Id{freeId, heldId}, MaxTasks: 1})
		freeTaskId := taskClaimGroupTestSchedule(clientSession, 2, &taskClaimGroupTestArgs{GroupId: freeId})
		owned, guard, err := first.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || owned[firstId] == nil {
			t.Fatal("held group was not acquired", err)
		}
		other, otherGuard, err := peer.takeTasks(1)
		if otherGuard != nil {
			defer otherGuard.release()
		}
		if err != nil || len(other) != 1 || other[freeTaskId] == nil || other[multiId] != nil {
			t.Fatal("partial group refusal retained or executed an unrelated owner", len(other), err)
		}
	})
}

type taskClaimGroupPostTarget struct {
	*taskClaimGroupTestTarget
	heldId  server.Id
	entered chan struct{}
	release <-chan struct{}
}

func (self *taskClaimGroupPostTarget) Run(ctx context.Context, queued *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if queued.TaskId != self.heldId {
		return self.Target.Run(ctx, queued)
	}
	return &struct{}{}, func(server.PgTx) ([]server.PostFunction, error) {
		return []server.PostFunction{func() any { close(self.entered); <-self.release; return nil }}, nil
	}, nil
}

// Group ownership belongs to the complete slot: a returned function and a
// committed finished row do not release a still-running external post.
func TestTaskClaimGroupActualRunRetainsCommittedPost(t *testing.T) {
	taskClaimGroupRun(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		groupId := server.NewId()
		firstId := taskClaimGroupTestSchedule(clientSession, 0, &taskClaimGroupTestArgs{GroupId: groupId})
		blockedId := taskClaimGroupTestSchedule(clientSession, 1, &taskClaimGroupTestArgs{GroupId: groupId})
		freeId := taskClaimGroupTestSchedule(clientSession, 2, &taskClaimGroupTestArgs{GroupId: server.NewId()})
		settings := DefaultTaskWorkerSettings()
		settings.BatchSize = 1
		first, peer := NewTaskWorker(ctx, settings), taskClaimGroupTestWorker(ctx)
		defer peer.Close()
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		first.AddTargets(&taskClaimGroupPostTarget{taskClaimGroupTestTarget: &taskClaimGroupTestTarget{NewTaskTarget(taskClaimGroupTestCall)}, heldId: firstId, entered: entered, release: release})
		done := make(chan struct{})
		var drainDone chan struct{}
		var runErr error
		go func() { defer close(done); server.HandleError(first.Run, func(err error) { runErr = err }) }()
		defer func() {
			unblock()
			first.Close()
			select {
			case <-done:
			case <-ctx.Done():
				t.Error("group Run cleanup did not join")
			}
			if drainDone != nil {
				select {
				case <-drainDone:
				case <-ctx.Done():
					t.Error("group drain owner did not join")
				}
			}
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("actual Run never entered its committed post")
		}
		finished, retried, posts, err := peer.EvalTasks(2)
		if err != nil || len(finished) != 1 || finished[0] != freeId || len(retried)+len(posts) != 0 {
			t.Fatal("committed post released its group or hid unrelated work", len(finished), len(retried), err)
		}
		if GetTasks(ctx, blockedId)[blockedId] == nil {
			t.Fatal("held post lost its unrelated durable successor")
		}
		drainDone = make(chan struct{})
		go func() { defer close(drainDone); first.Drain() }()
		select {
		case <-first.runCtx.Done():
		case <-ctx.Done():
			t.Fatal("actual Run did not stop admission")
		}
		unblock()
		select {
		case <-drainDone:
		case <-ctx.Done():
			t.Fatal("post retirement did not finish drain")
		}
		if !first.WaitFinalHandback() {
			t.Fatal("actual Run exhausted final handback")
		}
		first.Close()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("actual Run did not join")
		}
		if runErr != nil || first.InflightCount() != 0 {
			t.Fatal("actual Run retained an owner", runErr)
		}
		finished, retried, posts, err = peer.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != blockedId || len(retried)+len(posts) != 0 {
			t.Fatal("post retirement did not release its exact group", len(finished), err)
		}
		if len(GetFinishedTasks(ctx, firstId, blockedId, freeId)) != 3 || len(GetTasks(ctx, firstId, blockedId, freeId)) != 0 {
			t.Fatal("actual Run group changed exact durable completion identities")
		}
	})
}

// A failed initial claim releases its session. A failed refill retains every
// group with its original live siblings until the one guard finally retires.
func TestTaskClaimGroupInitialFailureReleasesOwnership(t *testing.T) {
	taskClaimGroupClaimFailure(t, false)
}

func TestTaskClaimGroupRefillFailureRetainsOwnership(t *testing.T) {
	taskClaimGroupClaimFailure(t, true)
}

func taskClaimGroupClaimFailure(t *testing.T, refill bool) {
	taskClaimGroupRun(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		first, peer := taskClaimGroupTestWorker(ctx), taskClaimGroupTestWorker(ctx)
		defer first.Close()
		defer peer.Close()
		firstKey, nextKey := server.NewId(), server.NewId()
		firstId := taskClaimGroupTestSchedule(clientSession, 0, &taskClaimGroupTestArgs{GroupId: firstKey})
		nextId := taskClaimGroupTestSchedule(clientSession, 1, &taskClaimGroupTestArgs{GroupId: nextKey})
		var guard *taskClaimGuard
		if refill {
			owned, held, err := first.takeTasks(1)
			guard = held
			if guard != nil {
				defer guard.release()
			}
			if err != nil || owned[firstId] == nil {
				t.Fatal("initial owner missing", err)
			}
		}
		refused := errors.New("controlled group claim refusal")
		first.claimBeforeCommit = func(*taskClaimGuard) error { return refused }
		_, _, _, err := first.takeTasksWithGuard(ctx, 1, guard, taskClaimOptions{})
		if !errors.Is(err, refused) {
			t.Fatal("claim refusal was lost", err)
		}
		if refill {
			// These identities have never held a per-task advisory lock.
			// Only retained provider keys can keep both new owners out.
			for index, key := range []server.Id{firstKey, nextKey} {
				taskClaimGroupTestSchedule(clientSession, index+2, &taskClaimGroupTestArgs{GroupId: key})
			}
			blocked, blockedGuard, err := peer.takeTasks(4)
			if blockedGuard != nil {
				defer blockedGuard.release()
			}
			if err != nil || len(blocked) != 0 {
				t.Fatal("refill failure released retained group ownership", len(blocked), err)
			}
			guard.release()
		}
		recovered, recoveredGuard, err := peer.takeTasks(2)
		if recoveredGuard != nil {
			defer recoveredGuard.release()
		}
		if err != nil || recovered[nextId] == nil {
			t.Fatal("released claim group did not recover", len(recovered), err)
		}
		if !refill && recovered[firstId] == nil {
			t.Fatal("initial rollback left a false timestamp claim")
		}
	})
}

// Two exact durable owners already held by one maintenance session must keep a
// same-group peer unclaimed, without hiding the next unrelated eligible task.
func TestTaskClaimGroupSerializesSameKeyAndLeavesUnrelatedWork(t *testing.T) {
	taskClaimGroupRun(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		first, peer := NewTaskWorkerWithDefaults(ctx), NewTaskWorkerWithDefaults(ctx)
		defer first.Close()
		defer peer.Close()
		first.AddTargets(&taskClaimGroupTestTarget{NewTaskTarget(taskClaimGroupTestCall)})
		peer.AddTargets(&taskClaimGroupTestTarget{NewTaskTarget(taskClaimGroupTestCall)})
		groupId := server.NewId()
		ids := make([]server.Id, 0, 4)
		for index := range 4 {
			key := groupId
			if index == 3 {
				key = server.NewId()
			}
			ids = append(ids, ScheduleTask(taskClaimGroupTestCall, &taskClaimGroupTestArgs{GroupId: key}, clientSession,
				RunAt(server.NowUtc().Add(-time.Hour+time.Duration(index)*time.Minute))))
		}
		owned, guard, err := first.takeTasks(2)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(owned) != 2 || owned[ids[0]] == nil || owned[ids[1]] == nil {
			t.Fatal("first exact same-group owners were not claimed", len(owned), err)
		}
		other, otherGuard, err := peer.takeTasks(2)
		if otherGuard != nil {
			defer otherGuard.release()
		}
		if err != nil || other[ids[3]] == nil {
			t.Fatal("held group hid the independent eligible owner", len(other), err)
		}
		if other[ids[2]] != nil {
			t.Fatal("peer claimed the same durable writer group while its first owners remained live")
		}
		if len(other) != 1 {
			t.Fatal("peer claim escaped its exact unrelated identity")
		}
	})
}
