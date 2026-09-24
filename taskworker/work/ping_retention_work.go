package work

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Retention of the ping measurement (connect/GEOMAP.md §5.7, D18, D26). A ping
// is a path measured on a day; past the ping report's Retention it describes
// nothing current and the derive phase stops reading it. Its row is kept a
// little longer, for KeepTimeout, the span over which the report could still
// accept another copy of its claim, so that a replay always finds the row it
// repeats. network_ping is partitioned by day, so the sweep keeps the days
// ahead of the inserts and drops the days behind whole: it never deletes a
// ping row, which at fleet scale would bloat the table the way row retention
// bloated client_reliability. The tallies the dashboard reads go with the
// ping days: the ones kept by day are dropped with them, and the small hour
// tally is deleted behind the oldest ping day. The job is named for the
// measurement rather than its one table because the derived locations the
// pings produce live a retention from the derivation that wrote them and are
// swept here too, row by row, as there is one row per node.

// Tunables of the sweep: the ping report's, which fix how often the sweep
// runs and how long a ping and a derived location are kept, and the partition
// statements' own.
type RemoveExpiredPingsSettings struct {
	ReportSettings    *controller.ExtenderPingReportSettings
	PartitionSettings *model.NetworkPingPartitionSettings
}

// The ping report's and the partition model's defaults.
func DefaultRemoveExpiredPingsSettings() *RemoveExpiredPingsSettings {
	return &RemoveExpiredPingsSettings{
		ReportSettings:    controller.DefaultExtenderPingReportSettings(),
		PartitionSettings: model.DefaultNetworkPingPartitionSettings(),
	}
}

// The sweep takes no arguments: its cuts are spans back from now.
type RemoveExpiredPingsArgs struct {
}

// What one sweep did: the partitions it created ahead and dropped behind, of
// network_ping and of the tallies kept by day, the rows of the fleet's hour
// tally it deleted behind the oldest ping day, and the derived rows it
// removed.
type RemoveExpiredPingsResult struct {
	CreatedPingPartitions   []string `json:"created_ping_partitions"`
	DroppedPingPartitions   []string `json:"dropped_ping_partitions"`
	RemovedPingHourTallies  int      `json:"removed_ping_hour_tallies"`
	RemovedDerivedLocations int      `json:"removed_derived_locations"`
}

// Schedules the next sweep one sweep interval from now, once per chain.
func ScheduleRemoveExpiredPings(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredPings,
		&RemoveExpiredPingsArgs{},
		clientSession,
		task.RunOnce("remove_expired_pings"),
		task.RunAt(server.NowUtc().Add(DefaultRemoveExpiredPingsSettings().ReportSettings.SweepTimeout)),
	)
}

// Keeps today's and the days ahead's ping and tally partitions, drops every
// day whose pings are all past their keep timeout with its tallies, and
// removes the derived locations past the retention, logging the partitions it created and dropped. A
// partition statement that failed is logged by the model and retried by the
// next sweep.
func RemoveExpiredPings(
	_ *RemoveExpiredPingsArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredPingsResult, error) {
	return removeExpiredPings(clientSession.Ctx, server.NowUtc(), DefaultRemoveExpiredPingsSettings()), nil
}

// The sweep as the clock reads `now`, under `settings`; a test moves the clock
// across days.
func removeExpiredPings(
	ctx context.Context,
	now time.Time,
	settings *RemoveExpiredPingsSettings,
) *RemoveExpiredPingsResult {
	createdPartitionNames, droppedPartitionNames, removedHourTallyCount := model.MaintainNetworkPingPartitions(
		ctx,
		now,
		settings.ReportSettings.KeepTimeout(),
		settings.PartitionSettings,
	)
	if 0 < len(createdPartitionNames) || 0 < len(droppedPartitionNames) {
		glog.Infof(
			"[ping]partitions created %v, dropped %v\n",
			createdPartitionNames,
			droppedPartitionNames,
		)
	}
	// A derived location lives as long as the pings it came from are read,
	// counted from the derivation that wrote it (§5.7): a node that stops
	// pinging drops out of every later derivation, and its row goes here, so
	// it falls back to its genesis without anyone noticing it stopped. Every
	// derivation also removes the rows it did not publish; this is what still
	// removes them when the derivations stop.
	removedDerivedLocations := model.RemoveExpiredDerivedLocations(
		ctx,
		now.Add(-settings.ReportSettings.Retention),
	)
	return &RemoveExpiredPingsResult{
		CreatedPingPartitions:   createdPartitionNames,
		DroppedPingPartitions:   droppedPartitionNames,
		RemovedPingHourTallies:  removedHourTallyCount,
		RemovedDerivedLocations: removedDerivedLocations,
	}
}

// Re-arms the chain: the next sweep is scheduled in the same transaction that
// finishes this one.
func RemoveExpiredPingsPost(
	_ *RemoveExpiredPingsArgs,
	_ *RemoveExpiredPingsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveExpiredPings(clientSession, tx)
	return nil
}
