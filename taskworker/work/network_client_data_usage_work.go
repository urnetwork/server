package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// RollupClientDataUsage drains the closed per-block redis data usage counters
// into monthly per-client usage and the data cap markers
// (model/network_client_data_cap_model.go). It runs continuously, rescheduled
// one block interval after each completion, so caps act within a couple of
// blocks of the usage that reaches them.

type RollupClientDataUsageArgs struct {
}

type RollupClientDataUsageResult struct {
}

func ScheduleRollupClientDataUsage(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RollupClientDataUsage,
		&RollupClientDataUsageArgs{},
		clientSession,
		task.RunOnce("rollup_client_data_usage"),
		task.RunAt(server.NowUtc().Add(model.ClientDataUsageRollupInterval)),
		task.MaxTime(15*time.Minute),
		task.Priority(task.TaskPriorityFastest),
	)
}

func RollupClientDataUsage(
	rollupClientDataUsage *RollupClientDataUsageArgs,
	clientSession *session.ClientSession,
) (*RollupClientDataUsageResult, error) {
	model.RollupClientDataUsage(clientSession.Ctx, server.NowUtc())
	return &RollupClientDataUsageResult{}, nil
}

func RollupClientDataUsagePost(
	rollupClientDataUsage *RollupClientDataUsageArgs,
	rollupClientDataUsageResult *RollupClientDataUsageResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRollupClientDataUsage(clientSession, tx)
	return nil
}
