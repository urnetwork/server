package work

// approval_use's sweep (§16.6's retention row; §2.8 api-10; QD-100): each hour,
// the rows a day past their decision's not_after, done and cancelled alike, read
// by not_after and never by create_time. The hourly account expiry runs it
// (RemoveExpiredAuthCodes), as it does admin token uses. A no-op in a binary that
// does not link MG5.

import (
	"context"
	"time"

	"github.com/urnetwork/server/model"
)

// The rows removed at now.
func removeExpiredApprovalUses(ctx context.Context, now time.Time) int64 {
	return model.RemoveExpiredApprovalUses(ctx, now.Add(-model.DefaultApprovalUseSettings().Retention))
}
