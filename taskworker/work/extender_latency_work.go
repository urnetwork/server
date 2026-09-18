package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Retention of the provider latency attestations
// (connect/DESIGNNOTES4.md §3). A row is a measurement of a path on a day;
// after the retention it describes nothing current and goes.

type RemoveOldExtenderLatenciesArgs struct {
}

type RemoveOldExtenderLatenciesResult struct {
	Removed int `json:"removed"`
}

func ScheduleRemoveOldExtenderLatencies(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldExtenderLatencies,
		&RemoveOldExtenderLatenciesArgs{},
		clientSession,
		task.RunOnce("remove_old_extender_latencies"),
		task.RunAt(server.NowUtc().Add(1*time.Hour)),
	)
}

func RemoveOldExtenderLatencies(
	_ *RemoveOldExtenderLatenciesArgs,
	clientSession *session.ClientSession,
) (*RemoveOldExtenderLatenciesResult, error) {
	minCreateTime := server.NowUtc().Add(-controller.ExtenderLatencyRetention)
	removed := model.RemoveOldNetworkExtenderLatencies(clientSession.Ctx, minCreateTime)
	return &RemoveOldExtenderLatenciesResult{
		Removed: removed,
	}, nil
}

func RemoveOldExtenderLatenciesPost(
	_ *RemoveOldExtenderLatenciesArgs,
	_ *RemoveOldExtenderLatenciesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldExtenderLatencies(clientSession, tx)
	return nil
}
