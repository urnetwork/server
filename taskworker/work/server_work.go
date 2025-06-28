package work

import (
	"time"

	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	// "github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

type DbMaintenanceArgs struct {
}

type DbMaintenanceResult struct {
}

func ScheduleDbMaintenance(clientSession *session.ClientSession, tx server.PgTx) {
	runAt := func() time.Time {
		now := time.Now().UTC()
		year, month, day := now.Date()
		return time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
	}()

	task.ScheduleTaskInTx(
		tx,
		DbMaintenance,
		&DbMaintenanceArgs{},
		clientSession,
		task.RunOnce("db_maintenance"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

func DbMaintenance(dbMaintenance *DbMaintenanceArgs, clientSession *session.ClientSession) (*DbMaintenanceResult, error) {
	server.DbMaintenance(clientSession.Ctx)
	return &DbMaintenanceResult{}, nil
}

func DbMaintenancePost(
	dbMaintenance *DbMaintenanceArgs,
	dbMaintenanceResult *DbMaintenanceResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleDbMaintenance(clientSession, tx)
	return nil
}
