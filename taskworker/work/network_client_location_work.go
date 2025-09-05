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
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
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
