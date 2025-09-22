package work

import (
	"time"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/task"

	// "github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/session"
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
