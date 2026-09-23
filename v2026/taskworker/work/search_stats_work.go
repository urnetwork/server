package work

// search_stats_work.go — the recurring rollup for provider "search interest".
// FindProviders2 accumulates per-provider match counts in redis on the hot
// path; RollupSearchProviderStats drains completed hour buckets into the
// `search_provider_stats` table that the /stats/providers* APIs read. Mirrors
// RollupVerifyProviderStats (redis counters -> periodic task -> pg rollup).

import (
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type RollupSearchProviderStatsArgs struct {
}

type RollupSearchProviderStatsResult struct {
}

func ScheduleRollupSearchProviderStats(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RollupSearchProviderStats,
		&RollupSearchProviderStatsArgs{},
		clientSession,
		task.RunOnce("rollup_search_provider_stats"),
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(15*time.Minute),
	)
}

func RollupSearchProviderStats(
	rollupSearchProviderStats *RollupSearchProviderStatsArgs,
	clientSession *session.ClientSession,
) (*RollupSearchProviderStatsResult, error) {
	model.RollupSearchProviderStats(
		clientSession.Ctx,
		server.NowUtc(),
	)
	return &RollupSearchProviderStatsResult{}, nil
}

func RollupSearchProviderStatsPost(
	rollupSearchProviderStats *RollupSearchProviderStatsArgs,
	rollupSearchProviderStatsResult *RollupSearchProviderStatsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRollupSearchProviderStats(clientSession, tx)
	return nil
}

type RemoveOldSearchProviderStatsArgs struct {
}

type RemoveOldSearchProviderStatsResult struct {
}

func ScheduleRemoveOldSearchProviderStats(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldSearchProviderStats,
		&RemoveOldSearchProviderStatsArgs{},
		clientSession,
		task.RunOnce("remove_old_search_provider_stats"),
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(1*time.Hour),
	)
}

func RemoveOldSearchProviderStats(
	removeOldSearchProviderStats *RemoveOldSearchProviderStatsArgs,
	clientSession *session.ClientSession,
) (*RemoveOldSearchProviderStatsResult, error) {
	limit := 50000
	model.RemoveOldSearchProviderStats(clientSession.Ctx, server.NowUtc(), limit)
	return &RemoveOldSearchProviderStatsResult{}, nil
}

func RemoveOldSearchProviderStatsPost(
	removeOldSearchProviderStats *RemoveOldSearchProviderStatsArgs,
	removeOldSearchProviderStatsResult *RemoveOldSearchProviderStatsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldSearchProviderStats(clientSession, tx)
	return nil
}
