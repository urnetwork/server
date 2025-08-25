package work

import (
	"time"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/task"

	// "github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/session"
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
		task.RunAt(server.NowUtc().Add(15*time.Minute)),
	)
}

func RemoveOldClientReliabilityStats(
	removeOldClientReliabilityStats *RemoveOldClientReliabilityStatsArgs,
	clientSession *session.ClientSession,
) (*RemoveOldClientReliabilityStatsResult, error) {
	minTime := server.NowUtc().Add(-30 * 24 * time.Hour)
	model.RemoveOldClientReliabilityStats(clientSession.Ctx, minTime)
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

func ScheduleUpdateClientReliabilityScores(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		UpdateClientReliabilityScores,
		&UpdateClientReliabilityScoresArgs{},
		clientSession,
		task.RunOnce("update_client_reliability_scores"),
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
	)
}

func UpdateClientReliabilityScores(
	updateClientReliabilityScores *UpdateClientReliabilityScoresArgs,
	clientSession *session.ClientSession,
) (*UpdateClientReliabilityScoresResult, error) {
	// the use case for these stats is match making, which values near term data over long term data
	minTime := server.NowUtc().Add(-15 * time.Minute)
	model.UpdateClientReliabilityScores(clientSession.Ctx, minTime, server.NowUtc())
	return &UpdateClientReliabilityScoresResult{}, nil
}

func UpdateClientReliabilityScoresPost(
	updateClientReliabilityScores *UpdateClientReliabilityScoresArgs,
	updateClientReliabilityScoresResult *UpdateClientReliabilityScoresResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleUpdateClientReliabilityScores(clientSession, tx)
	return nil
}

type UpdateNetworkReliabilityWindowArgs struct {
}

type UpdateNetworkReliabilityWindowResult struct {
}

func ScheduleUpdateNetworkReliabilityWindow(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		UpdateNetworkReliabilityWindow,
		&UpdateNetworkReliabilityWindowArgs{},
		clientSession,
		task.RunOnce("update_network_reliability_window"),
		task.RunAt(server.NowUtc().Add(5*time.Minute)),
	)
}

func UpdateNetworkReliabilityWindow(
	updateNetworkReliabilityWindow *UpdateNetworkReliabilityWindowArgs,
	clientSession *session.ClientSession,
) (*UpdateNetworkReliabilityWindowResult, error) {
	minTime := server.NowUtc().Add(-7 * 24 * time.Hour)
	model.UpdateNetworkReliabilityWindow(clientSession.Ctx, minTime, server.NowUtc())
	return &UpdateNetworkReliabilityWindowResult{}, nil
}

func UpdateNetworkReliabilityWindowPost(
	updateNetworkReliabilityWindow *UpdateNetworkReliabilityWindowArgs,
	updateNetworkReliabilityWindowResult *UpdateNetworkReliabilityWindowResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleUpdateNetworkReliabilityWindow(clientSession, tx)
	return nil
}
