package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	// "github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

type RemoveExpiredAuthCodesArgs struct {
}

type RemoveExpiredAuthCodesResult struct {
}

// Each account expiry below is bounded by its own run budget (about 30 s while
// a backlog drains), so the run stays well inside this.
const removeExpiredAuthCodesMaxTime = 10 * time.Minute

func ScheduleRemoveExpiredAuthCodes(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredAuthCodes,
		&RemoveExpiredAuthCodesArgs{},
		clientSession,
		task.RunOnce("remove_expired_auth_codes"),
		task.RunAt(server.NowUtc().Add(1*time.Hour)),
		task.MaxTime(removeExpiredAuthCodesMaxTime),
	)
}

// The hourly account expiry: auth codes and verify codes, then the rate limit
// histories and the codes and cool-downs that end, each past the longest
// reader of its rows. Deleted rows are counted by table
// (urnetwork_retention_deleted_rows_total), not logged per row.
func RemoveExpiredAuthCodes(
	removeExpiredAuthCodes *RemoveExpiredAuthCodesArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredAuthCodesResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	minTime := now.Add(-24 * time.Hour)
	authCodeCount := model.RemoveExpiredAuthCodes(ctx, minTime)
	glog.Infof("[authw]removed %d auth codes.\n", authCodeCount)
	model.RemoveExpiredVerifyCodes(ctx, minTime)

	attemptCount := model.RemoveExpiredAccountActionAttempts(ctx, now.Add(-model.AccountActionAttemptRetention))
	createAttemptCount := model.RemoveExpiredNetworkCreateAttempts(ctx, now.Add(-model.NetworkCreateAttemptRetention))
	resetCount := model.RemoveExpiredUserAuthResets(ctx, now.Add(-model.UserAuthResetRetention))
	deviceAddCount := model.RemoveExpiredDeviceAddHistory(ctx, now.Add(-model.DeviceAddHistoryRetention))
	nameReclaimCount := model.RemoveExpiredNetworkNameReclaims(ctx, now.Add(-model.NetworkNameReclaimRetention))
	if 0 < attemptCount+createAttemptCount+resetCount+deviceAddCount+nameReclaimCount {
		glog.Infof(
			"[authw]removed %d account action attempts, %d network create attempts, %d reset codes, %d device adds, %d name cool-downs\n",
			attemptCount,
			createAttemptCount,
			resetCount,
			deviceAddCount,
			nameReclaimCount,
		)
	}
	return &RemoveExpiredAuthCodesResult{}, nil
}

func RemoveExpiredAuthCodesPost(
	removeExpiredAuthCodes *RemoveExpiredAuthCodesArgs,
	removeExpiredAuthCodesResult *RemoveExpiredAuthCodesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveExpiredAuthCodes(clientSession, tx)
	return nil
}
