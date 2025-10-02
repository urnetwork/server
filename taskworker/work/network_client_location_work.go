package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	// "github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

type UpdateClientScoresArgs struct {
}

type UpdateClientScoresResult struct {
}

func ScheduleUpdateClientScores(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		UpdateClientScores,
		&UpdateClientScoresArgs{},
		clientSession,
		task.RunOnce("update_client_scores"),
		task.RunAt(server.NowUtc().Add(5*time.Second)),
		task.Priority(task.TaskPriorityFastest),
	)
}

func UpdateClientScores(
	updateClientScores *UpdateClientScoresArgs,
	clientSession *session.ClientSession,
) (*UpdateClientScoresResult, error) {
	ttl := 30 * time.Minute
	err := model.UpdateClientScores(clientSession.Ctx, ttl)
	if err != nil {
		return nil, err
	}
	return &UpdateClientScoresResult{}, nil
}

func UpdateClientScoresPost(
	updateClientScores *UpdateClientScoresArgs,
	updateClientScoresResult *UpdateClientScoresResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleUpdateClientScores(clientSession, tx)
	return nil
}

type UpdateClientLocationsArgs struct {
}

type UpdateClientLocationsResult struct {
}

func ScheduleUpdateClientLocations(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		UpdateClientLocations,
		&UpdateClientLocationsArgs{},
		clientSession,
		task.RunOnce("update_client_locations"),
		task.RunAt(server.NowUtc().Add(5*time.Second)),
		task.Priority(task.TaskPriorityFastest),
	)
}

func UpdateClientLocations(
	updateClientLocations *UpdateClientLocationsArgs,
	clientSession *session.ClientSession,
) (*UpdateClientLocationsResult, error) {
	ttl := 30 * time.Minute
	err := model.UpdateClientLocations(clientSession.Ctx, ttl)
	if err != nil {
		return nil, err
	}
	return &UpdateClientLocationsResult{}, nil
}

func UpdateClientLocationsPost(
	updateClientLocations *UpdateClientLocationsArgs,
	updateClientLocationsResult *UpdateClientLocationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleUpdateClientLocations(clientSession, tx)
	return nil
}
