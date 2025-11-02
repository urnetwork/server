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
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(1*time.Hour),
	)
}

func RemoveOldClientReliabilityStats(
	removeOldClientReliabilityStats *RemoveOldClientReliabilityStatsArgs,
	clientSession *session.ClientSession,
) (*RemoveOldClientReliabilityStatsResult, error) {
	minTime := server.NowUtc().Add(-10 * 24 * time.Hour)
	limit := 50000
	model.RemoveOldClientReliabilityStats(clientSession.Ctx, minTime, limit)
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
		task.MaxTime(60*time.Minute),
		task.Priority(task.TaskPriorityFastest),
	)
}

func UpdateClientReliabilityScores(
	updateClientReliabilityScores *UpdateClientReliabilityScoresArgs,
	clientSession *session.ClientSession,
) (*UpdateClientReliabilityScoresResult, error) {
	// the use case for these stats is match making, which values near term data over long term data
	minTime := server.NowUtc().Add(-15 * time.Minute)
	maxTime := server.NowUtc()

	model.UpdateClientLocationReliabilities(clientSession.Ctx, minTime, maxTime)
	model.UpdateClientReliabilityScores(clientSession.Ctx, minTime, maxTime, false)
	model.UpdateNetworkReliabilityScores(clientSession.Ctx, minTime, maxTime, false)
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
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(30*time.Minute),
		task.Priority(task.TaskPriorityFastest),
	)
}

func UpdateNetworkReliabilityWindow(
	updateNetworkReliabilityWindow *UpdateNetworkReliabilityWindowArgs,
	clientSession *session.ClientSession,
) (*UpdateNetworkReliabilityWindowResult, error) {
	minTime := server.NowUtc().Add(-1 * time.Hour)
	maxTime := server.NowUtc()
	model.UpdateNetworkReliabilityWindow(clientSession.Ctx, minTime, maxTime)
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
	minTime := server.NowUtc().Add(-10 * 24 * time.Hour)
	limit := 50000
	model.RemoveOldNetworkReliabilityWindow(clientSession.Ctx, minTime, limit)
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
	minTime := server.NowUtc().Add(-30 * 24 * time.Hour)
	model.RemoveOldClientLocationReliabilities(clientSession.Ctx, minTime)
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
