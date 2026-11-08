package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The daily sweep of §8.9's acks: rows past their 7 days go (§2.8 api-15). It
// does nothing in a binary that does not link MG2.

type RemoveOldNetworkAclAcksArgs struct {
}

type RemoveOldNetworkAclAcksResult struct {
}

// Tunables of the sweep.
type RemoveOldNetworkAclAcksSettings struct {
	// Between two runs. Hourly keeps a row at most an hour past its 7 days.
	RunInterval time.Duration
	// One run's bound; the retention batches stop well inside it.
	MaxTime time.Duration
}

func DefaultRemoveOldNetworkAclAcksSettings() *RemoveOldNetworkAclAcksSettings {
	return &RemoveOldNetworkAclAcksSettings{
		RunInterval: time.Hour,
		MaxTime:     10 * time.Minute,
	}
}

func ScheduleRemoveOldNetworkAclAcks(clientSession *session.ClientSession, tx server.PgTx) {
	settings := DefaultRemoveOldNetworkAclAcksSettings()
	task.ScheduleTaskInTx(
		tx,
		RemoveOldNetworkAclAcks,
		&RemoveOldNetworkAclAcksArgs{},
		clientSession,
		task.RunOnce("remove_old_network_acl_acks"),
		task.RunAt(server.NowUtc().Add(settings.RunInterval)),
		task.MaxTime(settings.MaxTime),
	)
}

func RemoveOldNetworkAclAcks(
	removeOldNetworkAclAcks *RemoveOldNetworkAclAcksArgs,
	clientSession *session.ClientSession,
) (*RemoveOldNetworkAclAcksResult, error) {
	minTime := server.NowUtc().Add(-model.DefaultNetworkAclAckSettings().Retention)
	removedCount := model.RemoveOldNetworkAclAcks(clientSession.Ctx, minTime)
	if 0 < removedCount {
		glog.Infof("[acl]removed %d acks past their retention\n", removedCount)
	}
	return &RemoveOldNetworkAclAcksResult{}, nil
}

func RemoveOldNetworkAclAcksPost(
	removeOldNetworkAclAcks *RemoveOldNetworkAclAcksArgs,
	removeOldNetworkAclAcksResult *RemoveOldNetworkAclAcksResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldNetworkAclAcks(clientSession, tx)
	return nil
}
