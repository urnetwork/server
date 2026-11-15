package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The hourly sweep of §7.6's SCIM queue (TEAM-M6-07): a change past its 7 days
// goes (§2.8 api-3's "SCIM changes at most 7 d"). Reads leave such a change out
// already (model.NetworkScimSettings.ChangeRetention); this deletes the row. It
// is not scheduled in a binary that does not link MG6.

type RemoveExpiredNetworkScimChangesArgs struct {
}

type RemoveExpiredNetworkScimChangesResult struct {
}

// Tunables of the sweep.
type RemoveExpiredNetworkScimChangesSettings struct {
	// Between two runs. An hour keeps a row at most an hour past its 7 days.
	RunInterval time.Duration
	// One run's bound.
	MaxTime time.Duration
}

// The defaults, a fresh value each call.
func DefaultRemoveExpiredNetworkScimChangesSettings() *RemoveExpiredNetworkScimChangesSettings {
	return &RemoveExpiredNetworkScimChangesSettings{
		RunInterval: time.Hour,
		MaxTime:     10 * time.Minute,
	}
}

// Schedules the next sweep, a RunInterval on.
func ScheduleRemoveExpiredNetworkScimChanges(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg6Linked() {
		return
	}
	settings := DefaultRemoveExpiredNetworkScimChangesSettings()
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredNetworkScimChanges,
		&RemoveExpiredNetworkScimChangesArgs{},
		clientSession,
		task.RunOnce("remove_expired_network_scim_changes"),
		task.RunAt(server.NowUtc().Add(settings.RunInterval)),
		task.MaxTime(settings.MaxTime),
	)
}

// One sweep at real time.
func RemoveExpiredNetworkScimChanges(
	removeExpiredNetworkScimChanges *RemoveExpiredNetworkScimChangesArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredNetworkScimChangesResult, error) {
	minCreateTime := server.NowUtc().Add(-model.DefaultNetworkScimSettings().ChangeRetention)
	if removedCount := model.RemoveExpiredNetworkScimChanges(clientSession.Ctx, minCreateTime); 0 < removedCount {
		glog.Infof("[scim]removed %d changes past their 7 days\n", removedCount)
	}
	return &RemoveExpiredNetworkScimChangesResult{}, nil
}

// Schedules the next sweep.
func RemoveExpiredNetworkScimChangesPost(
	removeExpiredNetworkScimChanges *RemoveExpiredNetworkScimChangesArgs,
	removeExpiredNetworkScimChangesResult *RemoveExpiredNetworkScimChangesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveExpiredNetworkScimChanges(clientSession, tx)
	return nil
}
