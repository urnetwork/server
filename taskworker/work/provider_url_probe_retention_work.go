// Receipt storage cleanup has one durable owner, outside URL admission.
package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const providerUrlProbeRetentionPageSize = 5000

type RemoveExpiredProviderUrlProbeRunsArgs struct{}

// Counts only rows removed by the completed model transaction.
type RemoveExpiredProviderUrlProbeRunsResult struct {
	Removed int64 `json:"removed"`
}

// The global run-once key coalesces all workers. A full page schedules another
// bounded turn; a partial or locked page returns to the idle cadence.
func scheduleRemoveExpiredProviderUrlProbeRuns(clientSession *session.ClientSession, tx server.PgTx, full bool) {
	delay := time.Minute
	if full {
		delay = time.Second
	}
	task.ScheduleTaskInTx(tx, RemoveExpiredProviderUrlProbeRuns, &RemoveExpiredProviderUrlProbeRunsArgs{}, clientSession,
		task.RunOnce("remove_expired_provider_url_probe_runs"), task.RunAt(server.NowUtc().Add(delay)), task.MaxTime(30*time.Second))
}

// Startup and profile scheduling share the same durable identity.
func ScheduleRemoveExpiredProviderUrlProbeRuns(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleRemoveExpiredProviderUrlProbeRuns(clientSession, tx, false)
}

// A failed/canceled transaction does not return a successful result. The task
// engine retains that failure and retries; no request starts detached cleanup.
func RemoveExpiredProviderUrlProbeRuns(_ *RemoveExpiredProviderUrlProbeRunsArgs, clientSession *session.ClientSession) (*RemoveExpiredProviderUrlProbeRunsResult, error) {
	if err := clientSession.Ctx.Err(); err != nil {
		return nil, err
	}
	removed := model.RemoveExpiredProviderUrlProbeRuns(clientSession.Ctx, server.NowUtc(), providerUrlProbeRetentionPageSize)
	return &RemoveExpiredProviderUrlProbeRunsResult{Removed: removed}, nil
}

// Failed work never reaches the success post hook; successful retries remain
// idempotent because the model deletes only the selected old, uncounted rows.
func RemoveExpiredProviderUrlProbeRunsPost(_ *RemoveExpiredProviderUrlProbeRunsArgs, result *RemoveExpiredProviderUrlProbeRunsResult, clientSession *session.ClientSession, tx server.PgTx) error {
	scheduleRemoveExpiredProviderUrlProbeRuns(clientSession, tx, result.Removed == providerUrlProbeRetentionPageSize)
	return nil
}
