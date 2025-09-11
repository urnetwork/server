package work

import (
	"time"

	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

type SyncInitialProductUpdatesArgs struct {
}

type SyncInitialProductUpdatesResult struct {
}

func ScheduleSyncInitialProductUpdates(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SyncInitialProductUpdates,
		&SyncInitialProductUpdatesArgs{},
		clientSession,
		task.RunOnce("sync_initial_product_updates"),
		task.RunAt(server.NowUtc().Add(5*time.Minute)),
	)
}

// these set the initial product updates for new networks and users
func SyncInitialProductUpdates(
	syncInitialProductUpdates *SyncInitialProductUpdatesArgs,
	clientSession *session.ClientSession,
) (*SyncInitialProductUpdatesResult, error) {

	err := controller.SyncInitialProductUpdates(clientSession.Ctx)
	if err != nil {
		return nil, err
	}

	return &SyncInitialProductUpdatesResult{}, nil
}

func SyncInitialProductUpdatesPost(
	syncInitialProductUpdates *SyncInitialProductUpdatesArgs,
	syncInitialProductUpdatesResult *SyncInitialProductUpdatesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSyncInitialProductUpdates(clientSession, tx)
	return nil
}
