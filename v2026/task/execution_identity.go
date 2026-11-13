package task

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

type executionIdentityKey struct{}

// ExecutionIdentity binds runtime-owned resources to a durable task and one
// invocation under its task claim guard. It contains no client credentials.
type ExecutionIdentity struct {
	TaskId server.Id
	Epoch  server.Id
}

func ExecutionIdentityFromContext(ctx context.Context) (ExecutionIdentity, bool) {
	value, ok := ctx.Value(executionIdentityKey{}).(ExecutionIdentity)
	return value, ok && value.TaskId != (server.Id{}) && value.Epoch != (server.Id{})
}

func withExecutionIdentity(ctx context.Context, taskId server.Id) context.Context {
	return context.WithValue(ctx, executionIdentityKey{}, ExecutionIdentity{TaskId: taskId, Epoch: server.NewId()})
}
