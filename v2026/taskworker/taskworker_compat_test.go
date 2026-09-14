package taskworker

import (
	"context"

	"github.com/urnetwork/server/v2026/task"
)

// Compile-time compatibility guards for consumers that retain either
// constructor as a function value.
var (
	_ func(context.Context) *task.TaskWorker                           = InitTaskWorker
	_ func(context.Context, *task.TaskWorkerSettings) *task.TaskWorker = InitTaskWorkerWithSettings
)
