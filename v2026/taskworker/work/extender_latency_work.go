package work

import (
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Retention of the provider latency attestations
// (connect/DESIGNNOTES4.md §3). A row is a measurement of a path on a day;
// after the retention it describes nothing current and goes. The table lives
// only as long as the latency report does, one release beside the ping report
// (connect/GEOMAP.md D14); the pings' own day-long sweep is
// ping_retention_work.go.

// Tunables of the sweep.
type RemoveOldExtenderLatenciesSettings struct {
	// how often the sweep runs
	RunTimeout time.Duration
}

// Hourly.
func DefaultRemoveOldExtenderLatenciesSettings() *RemoveOldExtenderLatenciesSettings {
	return &RemoveOldExtenderLatenciesSettings{
		RunTimeout: 1 * time.Hour,
	}
}

// The sweep takes no arguments: its cut is the report's retention from now.
type RemoveOldExtenderLatenciesArgs struct {
}

// What one sweep removed.
type RemoveOldExtenderLatenciesResult struct {
	RemovedCount int `json:"removed"`
}

// Schedules the next sweep one run timeout from now, once per chain.
func ScheduleRemoveOldExtenderLatencies(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldExtenderLatencies,
		&RemoveOldExtenderLatenciesArgs{},
		clientSession,
		task.RunOnce("remove_old_extender_latencies"),
		task.RunAt(server.NowUtc().Add(DefaultRemoveOldExtenderLatenciesSettings().RunTimeout)),
	)
}

// Removes the attestations older than the latency report's retention.
func RemoveOldExtenderLatencies(
	_ *RemoveOldExtenderLatenciesArgs,
	clientSession *session.ClientSession,
) (*RemoveOldExtenderLatenciesResult, error) {
	minCreateTime := server.NowUtc().Add(-controller.DefaultExtenderLatencyReportSettings().Retention)
	removedCount := model.RemoveOldNetworkExtenderLatencies(clientSession.Ctx, minCreateTime)
	return &RemoveOldExtenderLatenciesResult{
		RemovedCount: removedCount,
	}, nil
}

// Re-arms the chain: the next sweep is scheduled in the same transaction that
// finishes this one.
func RemoveOldExtenderLatenciesPost(
	_ *RemoveOldExtenderLatenciesArgs,
	_ *RemoveOldExtenderLatenciesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldExtenderLatencies(clientSession, tx)
	return nil
}
