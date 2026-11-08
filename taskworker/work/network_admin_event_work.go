package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The daily sweep of §7.11's admin event log: events past their 400 days go
// (§2.8 api-8). It does nothing in a binary that does not link MG1.

type RemoveOldNetworkAdminEventsArgs struct {
}

type RemoveOldNetworkAdminEventsResult struct {
}

// Tunables of the sweep.
type RemoveOldNetworkAdminEventsSettings struct {
	// Between two runs. A day keeps a row at most a day past its ttl.
	RunInterval time.Duration
	// One run's bound; the retention batches stop well inside it.
	MaxTime time.Duration
}

func DefaultRemoveOldNetworkAdminEventsSettings() *RemoveOldNetworkAdminEventsSettings {
	return &RemoveOldNetworkAdminEventsSettings{
		RunInterval: 24 * time.Hour,
		MaxTime:     10 * time.Minute,
	}
}

func ScheduleRemoveOldNetworkAdminEvents(clientSession *session.ClientSession, tx server.PgTx) {
	settings := DefaultRemoveOldNetworkAdminEventsSettings()
	task.ScheduleTaskInTx(
		tx,
		RemoveOldNetworkAdminEvents,
		&RemoveOldNetworkAdminEventsArgs{},
		clientSession,
		task.RunOnce("remove_old_network_admin_events"),
		task.RunAt(server.NowUtc().Add(settings.RunInterval)),
		task.MaxTime(settings.MaxTime),
	)
}

func RemoveOldNetworkAdminEvents(
	removeOldNetworkAdminEvents *RemoveOldNetworkAdminEventsArgs,
	clientSession *session.ClientSession,
) (*RemoveOldNetworkAdminEventsResult, error) {
	minTime := server.NowUtc().Add(-model.DefaultNetworkAdminEventSettings().Retention)
	removedCount := model.RemoveOldNetworkAdminEvents(clientSession.Ctx, minTime)
	if 0 < removedCount {
		glog.Infof("[admin event]removed %d events past their retention\n", removedCount)
	}
	return &RemoveOldNetworkAdminEventsResult{}, nil
}

func RemoveOldNetworkAdminEventsPost(
	removeOldNetworkAdminEvents *RemoveOldNetworkAdminEventsArgs,
	removeOldNetworkAdminEventsResult *RemoveOldNetworkAdminEventsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldNetworkAdminEvents(clientSession, tx)
	return nil
}
