package task

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func TestTaskExecutionIdentityChangesOnRetryAndSurvivesPost(t *testing.T) {
	var identities []ExecutionIdentity
	target := NewTaskTargetWithPost(
		func(_ struct{}, s *session.ClientSession) (struct{}, error) {
			identity, ok := ExecutionIdentityFromContext(s.Ctx)
			if !ok {
				t.Fatal("task execution has no owner")
			}
			identities = append(identities, identity)
			return struct{}{}, nil
		},
		func(_ struct{}, _ struct{}, s *session.ClientSession, _ server.PgTx) error {
			identity, ok := ExecutionIdentityFromContext(s.Ctx)
			if !ok || identity != identities[len(identities)-1] {
				t.Fatal("post lost its invocation owner")
			}
			return nil
		},
	)
	pending := &Task{TaskId: server.NewId(), ArgsJson: `{}`}
	ctx := withExecutionIdentity(context.Background(), server.NewId())
	for range 2 {
		_, post, err := target.RunSpecific(ctx, pending)
		if err != nil || post == nil {
			t.Fatal("task did not finish", err)
		}
		if _, err := post(nil); err != nil {
			t.Fatal(err)
		}
	}
	if identities[0].TaskId != pending.TaskId || identities[1].TaskId != pending.TaskId || identities[0].Epoch == identities[1].Epoch || pending.ArgsJson != `{}` {
		t.Fatal("retry reused an invocation owner or persisted runtime state in arguments")
	}
}
