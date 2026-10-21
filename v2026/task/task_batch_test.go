// Preparation must remain inside the evaluator's existing ownership boundary.
package task

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Retains the preparation membership without changing ordinary target execution.
type taskBatchPreparationTestTarget struct {
	Target
	preparedTaskIds [][]server.Id
}

// Each prepared adapter owns a separate immutable claim set.
func (self *taskBatchPreparationTestTarget) PrepareTaskBatch(tasks []*Task) Target {
	taskIds := make([]server.Id, 0, len(tasks))
	members := map[server.Id]bool{}
	for _, queued := range tasks {
		taskIds = append(taskIds, queued.TaskId)
		members[queued.TaskId] = true
	}
	self.preparedTaskIds = append(self.preparedTaskIds, taskIds)
	return &taskPreparedBatchTestTarget{Target: self.Target, taskIdMembers: members}
}

// Exposes accidental cross-pass membership through the real target entry point.
type taskPreparedBatchTestTarget struct {
	Target
	taskIdMembers map[server.Id]bool
}

// The adapter does not replace per-task contexts, execution epochs or post hooks.
func (self *taskPreparedBatchTestTarget) Run(ctx context.Context, queued *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if !self.taskIdMembers[queued.TaskId] {
		return nil, nil, errors.New("task was not claimed by this preparation")
	}
	return self.Target.Run(ctx, queued)
}

// Aliases group once; unrelated and unknown targets retain ordinary dispatch.
// Canceling one invocation must not replace or cancel another task's context.
func TestTaskBatchPreparationPreservesClaimMembershipAndInvocationLifecycle(t *testing.T) {
	identities := []ExecutionIdentity{}
	base := NewTaskTarget(func(_ struct{}, clientSession *session.ClientSession) (struct{}, error) {
		identity, ok := ExecutionIdentityFromContext(clientSession.Ctx)
		if !ok {
			t.Fatal("prepared execution lost its task identity")
		}
		identities = append(identities, identity)
		return struct{}{}, clientSession.Ctx.Err()
	}, "synthetic.batch.alias")
	preparer := &taskBatchPreparationTestTarget{Target: base}
	unrelated := NewTaskTarget(func(_ struct{}, _ *session.ClientSession) (struct{}, error) { return struct{}{}, nil })
	worker := &TaskWorker{targets: map[string]Target{
		base.TargetFunctionName():      preparer,
		"synthetic.batch.alias":        preparer,
		unrelated.TargetFunctionName(): unrelated,
	}}
	first := &Task{TaskId: server.NewId(), FunctionName: base.TargetFunctionName(), ArgsJson: `{}`}
	alias := &Task{TaskId: server.NewId(), FunctionName: "synthetic.batch.alias", ArgsJson: `{}`}
	other := &Task{TaskId: server.NewId(), FunctionName: unrelated.TargetFunctionName(), ArgsJson: `{}`}
	unknown := &Task{TaskId: server.NewId(), FunctionName: "synthetic.unknown.target", ArgsJson: `{}`}
	firstTargets := worker.prepareTaskBatchTargets(map[server.Id]*Task{
		first.TaskId: first, alias.TaskId: alias, other.TaskId: other, unknown.TaskId: unknown,
	})
	if len(preparer.preparedTaskIds) != 1 || len(preparer.preparedTaskIds[0]) != 2 ||
		firstTargets[first.FunctionName] != firstTargets[alias.FunctionName] ||
		firstTargets[other.FunctionName] != unrelated || firstTargets[unknown.FunctionName] != nil {
		t.Fatal("preparation crossed target membership or split aliases")
	}
	secondTargets := worker.prepareTaskBatchTargets(map[server.Id]*Task{alias.TaskId: alias})
	if len(preparer.preparedTaskIds) != 2 || len(preparer.preparedTaskIds[1]) != 1 ||
		firstTargets[first.FunctionName] == secondTargets[alias.FunctionName] ||
		worker.targets[first.FunctionName] != preparer || worker.targets[alias.FunctionName] != preparer {
		t.Fatal("preparation reused another pass or mutated the registered target")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, post, err := firstTargets[first.FunctionName].Run(ctx, first); !errors.Is(err, context.Canceled) || post != nil {
		t.Fatal("prepared target hid an invocation cancellation", err)
	}
	for _, targets := range []map[string]Target{firstTargets, secondTargets} {
		_, post, err := targets[alias.FunctionName].Run(t.Context(), alias)
		if err != nil || post == nil {
			t.Fatal("a canceled neighbor lost another task's normal result", err)
		}
		if _, err := post(nil); err != nil {
			t.Fatal("prepared task post failed", err)
		}
	}
	if len(identities) != 3 || identities[0].TaskId != first.TaskId || identities[1].TaskId != alias.TaskId ||
		identities[2].TaskId != alias.TaskId || identities[1].Epoch == identities[2].Epoch ||
		first.ArgsJson != `{}` || alias.FunctionName != "synthetic.batch.alias" {
		t.Fatal("prepared execution reused identity or mutated durable arguments")
	}
}
