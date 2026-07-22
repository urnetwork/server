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
// whose bucket is safely in the past. ReserveBulkClientRemovalSlot only ever
// looks forward from the current hour, so a bucket older than the current
// hour is already irrelevant to any future reservation -- the 2-day margin
// here is just to keep a short audit trail, not because older rows are ever
// consulted again.
func RemoveExpiredBulkClientRemovalQuota(
	_ *RemoveExpiredBulkClientRemovalQuotaArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredBulkClientRemovalQuotaResult, error) {
	minBucketStart := server.NowUtc().Add(-48 * time.Hour)
	model.RemoveExpiredBulkClientRemovalQuota(clientSession.Ctx, minBucketStart)
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
