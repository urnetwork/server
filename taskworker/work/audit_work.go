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

type ExportStatsArgs struct {
}

type ExportStatsResult struct {
}

// exportStatsDisabled temporarily halts the audit stats export loop:
// ComputeStats90 runs seven 90-day aggregate passes every 30 seconds. It is
// now tagged with server.ReplicaDb, but until an actual replica is attached
// the load still lands on the primary, so the loop stays gated. While
// disabled, /stats/last-90 keeps serving the last exported redis blob
// (stats.last-90 has no ttl); refresh it manually with
// `bringyourctl stats export` if needed. Set false to resume the loop
// (InitTasks reseeds it at taskworker startup).
const exportStatsDisabled = true

func ScheduleExportStats(clientSession *session.ClientSession, tx server.PgTx) {
	if exportStatsDisabled {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		ExportStats,
		&ExportStatsArgs{},
		clientSession,
		task.RunOnce("export_stats"),
		task.RunAt(server.NowUtc().Add(30*time.Second)),
	)
}

func ExportStats(
	exportStats *ExportStatsArgs,
	clientSession *session.ClientSession,
) (*ExportStatsResult, error) {
	if exportStatsDisabled {
		// an already-pending task row runs once as a no-op; the post hook
		// does not reschedule, which ends the chain
		return &ExportStatsResult{}, nil
	}
	stats := model.ComputeStats90(clientSession.Ctx)
	model.ExportStats(clientSession.Ctx, stats)
	return &ExportStatsResult{}, nil
}

func ExportStatsPost(
	exportStats *ExportStatsArgs,
	exportStatsResult *ExportStatsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleExportStats(clientSession, tx)
	return nil
}

type RemoveOldAuditNetworkEventsArgs struct {
}

type RemoveOldAuditNetworkEventsResult struct {
}

func ScheduleRemoveOldAuditNetworkEvents(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldAuditNetworkEvents,
		&RemoveOldAuditNetworkEventsArgs{},
		clientSession,
		task.RunOnce("remove_old_audit_network_events"),
		task.RunAt(server.NowUtc().Add(1*time.Hour)),
		task.MaxTime(1*time.Hour),
	)
}

func RemoveOldAuditNetworkEvents(
	removeOldAuditNetworkEvents *RemoveOldAuditNetworkEventsArgs,
	clientSession *session.ClientSession,
) (*RemoveOldAuditNetworkEventsResult, error) {
	// batched so the initial backlog drains without one giant delete
	limit := 50000
	var totalRemovedCount int64
	for {
		removedCount := model.RemoveOldAuditNetworkEvents(clientSession.Ctx, server.NowUtc(), limit)
		totalRemovedCount += removedCount
		if removedCount < int64(limit) {
			break
		}
	}
	if 0 < totalRemovedCount {
		glog.Infof("[audit]removed %d old audit network events.\n", totalRemovedCount)
	}
	return &RemoveOldAuditNetworkEventsResult{}, nil
}

func RemoveOldAuditNetworkEventsPost(
	removeOldAuditNetworkEvents *RemoveOldAuditNetworkEventsArgs,
	removeOldAuditNetworkEventsResult *RemoveOldAuditNetworkEventsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldAuditNetworkEvents(clientSession, tx)
	return nil
}
