package work

import (
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The partition registry's two task chains (PARTITION-MAINTENANCE-PLAN.md A7,
// server/db_partition_registry.go). The runway chain creates every registered
// family's partitions ahead on its own short cadence, so a slow archive or a
// stuck retention pass never leaves an insert without a partition; after
// downtime its first run catches up at once. The retention chain analyzes the
// parents and closed leaves and retires expired ranges, carrying each family's
// resume state from run to run. A family with its own task leaves either part
// to it (AnalyzeByOwnTask, RetireByOwnTask).

// Cadences and budgets of the two chains.
type PartitionRegistryTaskSettings struct {
	// The runway chain runs again this long after each run.
	RunwayInterval time.Duration
	// The retention chain runs again this long after a run that finished its
	// work, and RetentionRetryInterval after one that left work behind.
	RetentionInterval      time.Duration
	RetentionRetryInterval time.Duration
	// A retention run's work budget, within MaxTime.
	RetentionBudget time.Duration
	// Each run's task max time.
	MaxTime time.Duration
}

// Ten minutes between runway runs: the families keep days or weeks ahead, so
// a run per ten minutes catches a gap long before it matters. Retention every
// 15 minutes, as the contract task, with a five-minute budget.
func DefaultPartitionRegistryTaskSettings() *PartitionRegistryTaskSettings {
	return &PartitionRegistryTaskSettings{
		RunwayInterval:         10 * time.Minute,
		RetentionInterval:      15 * time.Minute,
		RetentionRetryInterval: 1 * time.Minute,
		RetentionBudget:        5 * time.Minute,
		MaxTime:                30 * time.Minute,
	}
}

type MaintainPartitionRunwayArgs struct {
}

type MaintainPartitionRunwayResult struct {
}

func ScheduleMaintainPartitionRunway(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleMaintainPartitionRunway(clientSession, tx, server.NowUtc().Add(1*time.Minute))
}

func scheduleMaintainPartitionRunway(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		MaintainPartitionRunway,
		&MaintainPartitionRunwayArgs{},
		clientSession,
		task.RunOnce("maintain_partition_runway"),
		task.RunAt(runAt),
		task.MaxTime(DefaultPartitionRegistryTaskSettings().MaxTime),
	)
}

// Creates ahead for every registered family. A family whose current range or
// runway is not covered is an error line; the monitor pages on it
// (SIGNALS.md §2.4p).
func MaintainPartitionRunway(
	_ *MaintainPartitionRunwayArgs,
	clientSession *session.ClientSession,
) (*MaintainPartitionRunwayResult, error) {
	for _, report := range server.MaintainPartitionRunways(clientSession.Ctx, server.NowUtc()) {
		logPartitionRunwayReport(report)
	}
	return &MaintainPartitionRunwayResult{}, nil
}

func logPartitionRunwayReport(report *server.PartitionRunwayReport) {
	if 0 < len(report.Created) {
		glog.Infof("[partitions]%s created %v\n", report.Family, report.Created)
	}
	if 0 < len(report.Errors) {
		glog.Errorf("[partitions]%s runway: %s\n", report.Family, strings.Join(report.Errors, "; "))
	}
	if report.Partitioned && !report.CurrentCovered {
		glog.Errorf("[partitions]%s has no partition for the current range\n", report.Family)
	}
}

func MaintainPartitionRunwayPost(
	_ *MaintainPartitionRunwayArgs,
	_ *MaintainPartitionRunwayResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleMaintainPartitionRunway(clientSession, tx, server.NowUtc().Add(DefaultPartitionRegistryTaskSettings().RunwayInterval))
	return nil
}

type MaintainPartitionRetentionArgs struct {
	State server.PartitionMaintenanceState `json:"state"`
}

type MaintainPartitionRetentionResult struct {
	State server.PartitionMaintenanceState `json:"state"`
	More  bool                             `json:"more"`
}

func ScheduleMaintainPartitionRetention(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleMaintainPartitionRetention(clientSession, tx, &MaintainPartitionRetentionArgs{}, server.NowUtc().Add(2*time.Minute))
}

func scheduleMaintainPartitionRetention(clientSession *session.ClientSession, tx server.PgTx, args *MaintainPartitionRetentionArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		MaintainPartitionRetention,
		args,
		clientSession,
		task.RunOnce("maintain_partition_retention"),
		task.RunAt(runAt),
		task.MaxTime(DefaultPartitionRegistryTaskSettings().MaxTime),
	)
}

// Analyzes and retires every registered family within the budget, and returns
// where the next run resumes.
func MaintainPartitionRetention(
	maintainPartitionRetention *MaintainPartitionRetentionArgs,
	clientSession *session.ClientSession,
) (*MaintainPartitionRetentionResult, error) {
	report, state := server.MaintainPartitionRetention(
		clientSession.Ctx,
		server.NowUtc(),
		maintainPartitionRetention.State,
		DefaultPartitionRegistryTaskSettings().RetentionBudget,
	)
	for family, analyzed := range report.Analyzed {
		glog.Infof("[partitions]%s analyzed %v\n", family, analyzed)
	}
	for _, retired := range report.Retired {
		if 0 < len(retired.Dropped) || 0 < len(retired.Archived) {
			glog.Infof("[partitions]%s dropped %v, archived %v\n", retired.Family, retired.Dropped, retired.Archived)
		}
		for leaf, reason := range retired.Blocked {
			glog.V(1).Infof("[partitions]%s expired %s stays: %s\n", retired.Family, leaf, reason)
		}
	}
	return &MaintainPartitionRetentionResult{
		State: state,
		More:  report.More,
	}, nil
}

// A run that left work behind continues a minute later.
func MaintainPartitionRetentionPost(
	_ *MaintainPartitionRetentionArgs,
	maintainPartitionRetentionResult *MaintainPartitionRetentionResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultPartitionRegistryTaskSettings()
	runAt := server.NowUtc().Add(settings.RetentionInterval)
	if maintainPartitionRetentionResult.More {
		runAt = server.NowUtc().Add(settings.RetentionRetryInterval)
	}
	scheduleMaintainPartitionRetention(
		clientSession,
		tx,
		&MaintainPartitionRetentionArgs{State: maintainPartitionRetentionResult.State},
		runAt,
	)
	return nil
}
