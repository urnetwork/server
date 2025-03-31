package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	// "github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

type ExportStatsArgs struct {
}

type ExportStatsResult struct {
}

func ScheduleExportStats(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		ExportStats,
		&ExportStatsArgs{},
		clientSession,
		task.RunOnce("export_stats"),
		task.RunAt(time.Now().Add(30*time.Second)),
	)
}

func ExportStats(exportStats *ExportStatsArgs, clientSession *session.ClientSession) (*ExportStatsResult, error) {
	stats := model.ComputeStats90(clientSession.Ctx)
	model.ExportStats(clientSession.Ctx, stats)
	return &ExportStatsResult{}, nil
}

func ExportStatsPost(
	exportStats *ExportStatsArgs,
	exportStatsResult *ExportStatsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleExportStats(clientSession, tx)
	return nil
}
