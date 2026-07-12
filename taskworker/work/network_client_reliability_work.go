package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	// "github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

type RemoveOldClientReliabilityStatsArgs struct {
}

type RemoveOldClientReliabilityStatsResult struct {
}

func ScheduleRemoveOldClientReliabilityStats(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldClientReliabilityStats,
		&RemoveOldClientReliabilityStatsArgs{},
		clientSession,
		task.RunOnce("remove_old_client_reliability_stats"),
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(1*time.Hour),
	)
}

func RemoveOldClientReliabilityStats(
	removeOldClientReliabilityStats *RemoveOldClientReliabilityStatsArgs,
	clientSession *session.ClientSession,
) (*RemoveOldClientReliabilityStatsResult, error) {
	// client_reliability accumulates one row per connected client per block:
	// a single limit-sized batch per run cannot drain a backlog faster than it
	// grows. Keep deleting batches until the expired backlog is gone or the
	// time budget for this run is spent (the task reschedules a minute after
	// each run either way).
	limit := 50000
	budget := 5 * time.Minute
	endTime := server.NowUtc().Add(budget)
	var totalRemovedCount int64
	for {
		removedCount := model.RemoveOldClientReliabilityStats(clientSession.Ctx, server.NowUtc(), limit)
		totalRemovedCount += removedCount
		if removedCount < int64(limit) || endTime.Before(server.NowUtc()) {
			break
		}
	}
	if 0 < totalRemovedCount {
		glog.Infof("[ncr]removed %d expired client_reliability rows\n", totalRemovedCount)
	}
	return &RemoveOldClientReliabilityStatsResult{}, nil
}

func RemoveOldClientReliabilityStatsPost(
	removeOldClientReliabilityStats *RemoveOldClientReliabilityStatsArgs,
	removeOldClientReliabilityStatsResult *RemoveOldClientReliabilityStatsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldClientReliabilityStats(clientSession, tx)
	return nil
}

type UpdateClientReliabilityScoresArgs struct {
}

type UpdateClientReliabilityScoresResult struct {
}

// moved into `UpdateReliabilities`
func ScheduleUpdateClientReliabilityScores(clientSession *session.ClientSession, tx server.PgTx) {
	// task.ScheduleTaskInTx(
	// 	tx,
	// 	UpdateClientReliabilityScores,
	// 	&UpdateClientReliabilityScoresArgs{},
	// 	clientSession,
	// 	task.RunOnce("update_client_reliability_scores"),
	// 	task.RunAt(server.NowUtc().Add(1*time.Minute)),
	// 	task.MaxTime(60*time.Minute),
	// 	task.Priority(task.TaskPriorityFastest),
	// )
}

func UpdateClientReliabilityScores(
	updateClientReliabilityScores *UpdateClientReliabilityScoresArgs,
	clientSession *session.ClientSession,
) (*UpdateClientReliabilityScoresResult, error) {
	// the use case for these stats is match making, which values near term data over long term data
	// minTime := server.NowUtc().Add(-15 * time.Minute)
	// maxTime := server.NowUtc()
	// model.UpdateClientReliabilityScores(clientSession.Ctx, minTime, maxTime, false)
	return &UpdateClientReliabilityScoresResult{}, nil
}

func UpdateClientReliabilityScoresPost(
	updateClientReliabilityScores *UpdateClientReliabilityScoresArgs,
	updateClientReliabilityScoresResult *UpdateClientReliabilityScoresResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	// ScheduleUpdateClientReliabilityScores(clientSession, tx)
	return nil
}

type UpdateNetworkReliabilityWindowArgs struct {
}

type UpdateNetworkReliabilityWindowResult struct {
}

// moved into `UpdateReliabilities`
func ScheduleUpdateNetworkReliabilityWindow(clientSession *session.ClientSession, tx server.PgTx) {
	// task.ScheduleTaskInTx(
	// 	tx,
	// 	UpdateNetworkReliabilityWindow,
	// 	&UpdateNetworkReliabilityWindowArgs{},
	// 	clientSession,
	// 	task.RunOnce("update_network_reliability_window"),
	// 	task.RunAt(server.NowUtc().Add(1*time.Minute)),
	// 	task.MaxTime(30*time.Minute),
	// 	task.Priority(task.TaskPriorityFastest),
	// )
}

func UpdateNetworkReliabilityWindow(
	updateNetworkReliabilityWindow *UpdateNetworkReliabilityWindowArgs,
	clientSession *session.ClientSession,
) (*UpdateNetworkReliabilityWindowResult, error) {
	// minTime := server.NowUtc().Add(-1 * time.Hour)
	// maxTime := server.NowUtc()
	// model.UpdateNetworkReliabilityWindow(clientSession.Ctx, minTime, maxTime, false)
	return &UpdateNetworkReliabilityWindowResult{}, nil
}

func UpdateNetworkReliabilityWindowPost(
	updateNetworkReliabilityWindow *UpdateNetworkReliabilityWindowArgs,
	updateNetworkReliabilityWindowResult *UpdateNetworkReliabilityWindowResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	// ScheduleUpdateNetworkReliabilityWindow(clientSession, tx)
	return nil
}

type UpdateReliabilitiesArgs struct {
	MinTime time.Time `json:"min_time"`
}

type UpdateReliabilitiesResult struct {
	MaxTime time.Time `json:"max_time"`
}

func ScheduleUpdateReliabilities(clientSession *session.ClientSession, tx server.PgTx, minTime time.Time) {
	task.ScheduleTaskInTx(
		tx,
		UpdateReliabilities,
		&UpdateReliabilitiesArgs{
			MinTime: minTime,
		},
		clientSession,
		task.RunOnce("update_reliabilities"),
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(120*time.Minute),
		task.Priority(task.TaskPriorityFastest),
	)
}

func UpdateReliabilities(
	updateReliabilities *UpdateReliabilitiesArgs,
	clientSession *session.ClientSession,
) (*UpdateReliabilitiesResult, error) {
	maxTime := server.NowUtc()
	minTime := server.MinTime(
		maxTime.Add(-1*time.Hour),
		updateReliabilities.MinTime.Add(-5*time.Minute),
	)

	// the location reliabilities read the live connection tables, not the
	// drained blocks, so they update regardless of the rollup state
	model.UpdateClientLocationReliabilities(clientSession.Ctx, minTime, maxTime)

	// the window and score computations read the drained blocks: wait for the
	// redis->pg rollup to be live rather than recompute over windows that
	// drift away from the present while the drain is down
	if !model.ClientReliabilityRollupSynced(clientSession.Ctx, maxTime) {
		glog.Infof("[ncr]reliability rollup is stale; waiting to update the reliability stats\n")
		return &UpdateReliabilitiesResult{
			MaxTime: updateReliabilities.MinTime,
		}, nil
	}

	model.UpdateNetworkReliabilityWindow(clientSession.Ctx, minTime, maxTime, false)

	model.UpdateClientReliabilityScores(clientSession.Ctx, maxTime, false)

	return &UpdateReliabilitiesResult{
		MaxTime: maxTime,
	}, nil
}

func UpdateReliabilitiesPost(
	updateReliabilities *UpdateReliabilitiesArgs,
	updateReliabilitiesResult *UpdateReliabilitiesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleUpdateReliabilities(clientSession, tx, updateReliabilitiesResult.MaxTime)
	return nil
}

// RollupClientReliabilityStats drains the per-block redis reliability
// counters (written by the connection announce hot path) into
// `client_reliability` and advances the drain high-water mark. It runs
// continuously (rescheduled a minute after each completion) so pg lags the
// hot path by only a couple of blocks.

type RollupClientReliabilityStatsArgs struct {
}

type RollupClientReliabilityStatsResult struct {
}

func ScheduleRollupClientReliabilityStats(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RollupClientReliabilityStats,
		&RollupClientReliabilityStatsArgs{},
		clientSession,
		task.RunOnce("rollup_client_reliability_stats"),
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(15*time.Minute),
		task.Priority(task.TaskPriorityFastest),
	)
}

func RollupClientReliabilityStats(
	rollupClientReliabilityStats *RollupClientReliabilityStatsArgs,
	clientSession *session.ClientSession,
) (*RollupClientReliabilityStatsResult, error) {
	model.RollupClientReliabilityStats(
		clientSession.Ctx,
		server.NowUtc(),
	)
	return &RollupClientReliabilityStatsResult{}, nil
}

func RollupClientReliabilityStatsPost(
	rollupClientReliabilityStats *RollupClientReliabilityStatsArgs,
	rollupClientReliabilityStatsResult *RollupClientReliabilityStatsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRollupClientReliabilityStats(clientSession, tx)
	return nil
}

type RemoveOldNetworkReliabilityWindowArgs struct {
}

type RemoveOldNetworkReliabilityWindowResult struct {
}

func ScheduleRemoveOldNetworkReliabilityWindow(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldNetworkReliabilityWindow,
		&RemoveOldNetworkReliabilityWindowArgs{},
		clientSession,
		task.RunOnce("remove_old_network_reliability_window"),
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(1*time.Hour),
	)
}

func RemoveOldNetworkReliabilityWindow(
	removeOldNetworkReliabilityWindow *RemoveOldNetworkReliabilityWindowArgs,
	clientSession *session.ClientSession,
) (*RemoveOldNetworkReliabilityWindowResult, error) {
	limit := 500000
	model.RemoveOldNetworkReliabilityWindow(clientSession.Ctx, server.NowUtc(), limit)
	return &RemoveOldNetworkReliabilityWindowResult{}, nil
}

func RemoveOldNetworkReliabilityWindowPost(
	removeOldNetworkReliabilityWindow *RemoveOldNetworkReliabilityWindowArgs,
	removeOldNetworkReliabilityWindowResult *RemoveOldNetworkReliabilityWindowResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldNetworkReliabilityWindow(clientSession, tx)
	return nil
}

type RemoveOldClientLocationReliabilitiesArgs struct {
}

type RemoveOldClientLocationReliabilitiesResult struct {
}

func ScheduleRemoveOldClientLocationReliabilities(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldClientLocationReliabilities,
		&RemoveOldClientLocationReliabilitiesArgs{},
		clientSession,
		task.RunOnce("remove_old_client_location_reliabilities"),
		task.RunAt(server.NowUtc().Add(15*time.Minute)),
	)
}

func RemoveOldClientLocationReliabilities(
	removeOldClientLocationReliabilities *RemoveOldClientLocationReliabilitiesArgs,
	clientSession *session.ClientSession,
) (*RemoveOldClientLocationReliabilitiesResult, error) {
	model.RemoveOldClientLocationReliabilities(clientSession.Ctx, server.NowUtc())
	return &RemoveOldClientLocationReliabilitiesResult{}, nil
}

func RemoveOldClientLocationReliabilitiesPost(
	removeOldClientLocationReliabilities *RemoveOldClientLocationReliabilitiesArgs,
	removeOldClientLocationReliabilitiesResult *RemoveOldClientLocationReliabilitiesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldClientLocationReliabilities(clientSession, tx)
	return nil
}
