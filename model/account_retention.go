// Retention of the account tables whose rows end: issued password reset
// codes, the device-add rate limit's history and released network names'
// cool-downs. Each is deleted well past the last reader that counts it.
// Functions are safe concurrently.
package model

import (
	"context"
	"time"
)

// A reset code is valid 4 hours after it was issued (AuthPasswordSet), and
// issuing a new one marks the user's earlier codes used, so a week past issue
// no reader can match it.
const UserAuthResetRetention = 7 * 24 * time.Hour

// Device adds are counted over NetworkAddLimitTimeout (30 minutes).
const DeviceAddHistoryRetention = 7 * 24 * time.Hour

// A released name is refused only until its cool_down_until; after it the row
// answers nothing. Kept a week past it.
const NetworkNameReclaimRetention = 7 * 24 * time.Hour

var userAuthResetRetention = newTimeOrderedRetention(
	RetentionTableUserAuthReset,
	"user_auth_reset_id",
	"reset_time",
)

// device_add_history is keyed by (network_id, add_time, device_add_id) and holds
// a row per device add; its rows are read whole.
var deviceAddHistoryRetention = newScannedRetention(RetentionTableDeviceAddHistory, "add_time")

// network_name_reclaim is keyed by the released name, one row per name.
var networkNameReclaimRetention = newScannedRetention(RetentionTableNetworkNameReclaim, "cool_down_until")

// Removes the reset codes issued before minTime.
func RemoveExpiredUserAuthResets(ctx context.Context, minTime time.Time) (removedCount int64) {
	removedCount, _ = userAuthResetRetention.RemoveBefore(ctx, minTime, retentionBatchSize, time.Now().Add(retentionRunBudget))
	return
}

// Removes the device adds made before minTime.
func RemoveExpiredDeviceAddHistory(ctx context.Context, minTime time.Time) (removedCount int64) {
	removedCount, _ = deviceAddHistoryRetention.RemoveBefore(ctx, minTime, retentionBatchSize, time.Now().Add(retentionRunBudget))
	return
}

// Removes the released names whose cool-down ended before minTime.
func RemoveExpiredNetworkNameReclaims(ctx context.Context, minTime time.Time) (removedCount int64) {
	removedCount, _ = networkNameReclaimRetention.RemoveBefore(ctx, minTime, retentionBatchSize, time.Now().Add(retentionRunBudget))
	return
}
