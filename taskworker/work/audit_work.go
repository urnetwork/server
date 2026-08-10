package work

import (
	"context"
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

// exportStatsDisabled halts the audit stats export loop. Re-enabled
// 2026-08-08 (user decision) together with the real provider event feed
// (SweepProviderAuditEvents / STATS3.md): the public /stats/last-90 blob now
// refreshes continuously instead of requiring a manual
// `bringyourctl stats export`.
//
// The original gating reason was CADENCE, not the export itself:
// ComputeStats90 runs its 90-day aggregate passes (four, since the
// extender/packets/superspeed series were removed -- see STATS3.md), and the
// old loop ran them every 30 SECONDS against the primary (ReplicaDb still
// lands on the primary until a replica is attached). The series has daily
// granularity, so the loop now runs every exportStatsInterval instead -- the
// same load argument that gated it, answered by cadence rather than by
// disabling.
const exportStatsDisabled = false

// exportStatsInterval is how often the public stats blob recomputes. Daily
// granularity data + a 15-minute provider sweep cadence means anything
// tighter than minutes is waste; hourly keeps the primary load negligible
// (4 aggregate passes/hour vs the old 30-second loop) while the public feed
// stays fresh well within a day's resolution.
const exportStatsInterval = 1 * time.Hour

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
		task.RunAt(server.NowUtc().Add(exportStatsInterval)),
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

// SweepProviderAuditEvents appends real provider online/offline transitions
// to the audit_provider_event feed by diffing the live provider set (public
// provide key + connected connection) against the last emitted state
// (audit_provider_state). Transitions-only, so a quiet sweep writes nothing;
// the cadence bounds transition detection latency, which only needs to be
// well under a day (the stats resolution). The first run seeds the feed with
// the current provider snapshot. See model.SweepProviderAuditEvents.

type SweepProviderAuditEventsArgs struct {
}

type SweepProviderAuditEventsResult struct {
	OnlineCount        int `json:"online_count"`
	OfflineCount       int `json:"offline_count"`
	DeviceAddedCount   int `json:"device_added_count"`
	DeviceRemovedCount int `json:"device_removed_count"`
}

func ScheduleSweepProviderAuditEvents(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SweepProviderAuditEvents,
		&SweepProviderAuditEventsArgs{},
		clientSession,
		task.RunOnce("sweep_provider_audit_events"),
		task.RunAt(server.NowUtc().Add(15*time.Minute)),
		task.MaxTime(1*time.Hour),
	)
}

func SweepProviderAuditEvents(
	sweepProviderAuditEvents *SweepProviderAuditEventsArgs,
	clientSession *session.ClientSession,
) (*SweepProviderAuditEventsResult, error) {
	onlineCount, offlineCount := model.SweepProviderAuditEvents(clientSession.Ctx)
	if 0 < onlineCount || 0 < offlineCount {
		glog.Infof("[audit]provider sweep: %d online, %d offline\n", onlineCount, offlineCount)
	}
	// the devices series (connected-per-day, ALL connected clients) rides the
	// same cadence
	deviceAddedCount, deviceRemovedCount := model.SweepDeviceAuditEvents(clientSession.Ctx)
	if 0 < deviceAddedCount || 0 < deviceRemovedCount {
		glog.Infof("[audit]device sweep: %d added, %d removed\n", deviceAddedCount, deviceRemovedCount)
	}
	return &SweepProviderAuditEventsResult{
		OnlineCount:        onlineCount,
		OfflineCount:       offlineCount,
		DeviceAddedCount:   deviceAddedCount,
		DeviceRemovedCount: deviceRemovedCount,
	}, nil
}

func SweepProviderAuditEventsPost(
	sweepProviderAuditEvents *SweepProviderAuditEventsArgs,
	sweepProviderAuditEventsResult *SweepProviderAuditEventsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSweepProviderAuditEvents(clientSession, tx)
	return nil
}

// RollupTransferAuditEvents maintains the daily settled-transfer rollup rows
// in audit_contract_event (one aggregate row per complete UTC day, from
// transfer_contract x contract_close). Re-rolling the last few days each run
// picks up late closes; each day is idempotent by replacement.

type RollupTransferAuditEventsArgs struct {
}

type RollupTransferAuditEventsResult struct {
	DayCount int `json:"day_count"`
}

func ScheduleRollupTransferAuditEvents(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RollupTransferAuditEvents,
		&RollupTransferAuditEventsArgs{},
		clientSession,
		task.RunOnce("rollup_transfer_audit_events"),
		task.RunAt(server.NowUtc().Add(6*time.Hour)),
		task.MaxTime(1*time.Hour),
	)
}

func RollupTransferAuditEvents(
	rollupTransferAuditEvents *RollupTransferAuditEventsArgs,
	clientSession *session.ClientSession,
) (*RollupTransferAuditEventsResult, error) {
	now := server.NowUtc()
	dayCount := model.RollupTransferAuditEvents(clientSession.Ctx, now.Add(-3*24*time.Hour), now)
	return &RollupTransferAuditEventsResult{
		DayCount: dayCount,
	}, nil
}

func RollupTransferAuditEventsPost(
	rollupTransferAuditEvents *RollupTransferAuditEventsArgs,
	rollupTransferAuditEventsResult *RollupTransferAuditEventsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRollupTransferAuditEvents(clientSession, tx)
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

type RemoveOldAuditEventsArgs struct {
}

type RemoveOldAuditEventsResult struct {
}

func ScheduleRemoveOldAuditEvents(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldAuditEvents,
		&RemoveOldAuditEventsArgs{},
		clientSession,
		task.RunOnce("remove_old_audit_events"),
		task.RunAt(server.NowUtc().Add(1*time.Hour)),
		task.MaxTime(1*time.Hour),
	)
}

// RemoveOldAuditEvents reaps the provider/extender/device/contract audit feeds
// (audit_network_event has its own task). Each is batched so the initial backlog
// drains without one giant delete.
func RemoveOldAuditEvents(
	removeOldAuditEvents *RemoveOldAuditEventsArgs,
	clientSession *session.ClientSession,
) (*RemoveOldAuditEventsResult, error) {
	limit := 50000
	reap := func(name string, remove func(context.Context, time.Time, int) int64) {
		var total int64
		for {
			removedCount := remove(clientSession.Ctx, server.NowUtc(), limit)
			total += removedCount
			if removedCount < int64(limit) {
				break
			}
		}
		if 0 < total {
			glog.Infof("[audit]removed %d old audit %s events.\n", total, name)
		}
	}
	reap("provider", model.RemoveOldAuditProviderEvents)
	reap("extender", model.RemoveOldAuditExtenderEvents)
	reap("device", model.RemoveOldAuditDeviceEvents)
	reap("contract", model.RemoveOldAuditContractEvents)
	return &RemoveOldAuditEventsResult{}, nil
}

func RemoveOldAuditEventsPost(
	removeOldAuditEvents *RemoveOldAuditEventsArgs,
	removeOldAuditEventsResult *RemoveOldAuditEventsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldAuditEvents(clientSession, tx)
	return nil
}
