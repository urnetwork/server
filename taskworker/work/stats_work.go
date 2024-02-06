package work

import (
    "time"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/task"
    "bringyour.com/bringyour/model"
    // "bringyour.com/bringyour/controller"
    "bringyour.com/bringyour/session"
)


type ExportStatsArgs struct {
}

type ExportStatsResult struct {
}

func ScheduleExportStats(clientSession *session.ClientSession, tx bringyour.PgTx) {
	task.ScheduleTaskInTx(
        tx,
        ExportStats,
        &ExportStatsArgs{},
        clientSession,
        task.RunOnce("export_stats"),
        task.RunAt(time.Now().Add(30 * time.Second)),
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
	tx bringyour.PgTx,
) error {
	ScheduleExportStats(clientSession, tx)
	return nil
}