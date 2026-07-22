package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type RemoveExpiredBulkClientRemovalQuotaArgs struct{}

type RemoveExpiredBulkClientRemovalQuotaResult struct{}

func ScheduleRemoveExpiredBulkClientRemovalQuota(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredBulkClientRemovalQuota,
		&RemoveExpiredBulkClientRemovalQuotaArgs{},
		clientSession,
		task.RunOnce("remove_expired_bulk_client_removal_quota"),
		task.RunAt(server.NowUtc().Add(1*time.Hour)),
	)
}

// RemoveExpiredBulkClientRemovalQuota reaps bulk_client_removal_quota rows
// older than model.BulkClientRemovalWindow, which no longer contribute to
// the trailing-window sum model.CheckAndRecordBulkClientRemovalQuota checks.
func RemoveExpiredBulkClientRemovalQuota(
	_ *RemoveExpiredBulkClientRemovalQuotaArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredBulkClientRemovalQuotaResult, error) {
	minTime := server.NowUtc().Add(-model.BulkClientRemovalWindow)
	model.RemoveExpiredBulkClientRemovalQuota(clientSession.Ctx, minTime)
	return &RemoveExpiredBulkClientRemovalQuotaResult{}, nil
}

func RemoveExpiredBulkClientRemovalQuotaPost(
	_ *RemoveExpiredBulkClientRemovalQuotaArgs,
	_ *RemoveExpiredBulkClientRemovalQuotaResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveExpiredBulkClientRemovalQuota(clientSession, tx)
	return nil
}
