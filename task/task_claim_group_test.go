// Durable claim groups isolate one writer while unrelated work stays available.
package task

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type taskClaimGroupTestArgs struct {
	GroupId server.Id
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
	return []server.Id{args.GroupId}, 64
}

// Two exact durable owners already held by one maintenance session must keep a
// same-group peer unclaimed, without hiding the next unrelated eligible task.
func TestTaskClaimGroupSerializesSameKeyAndLeavesUnrelatedWork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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
