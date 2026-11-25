package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Partition maintenance and retention of the contract tables
// (CONTRACT-PARTITION.md; see server.MaintainContractPartitions). Before the
// cutover a run only guards a prepared cutover, so this build is safe on
// either side of the cutover. The contract days retire at the contract
// horizon, read from the horizon reaper's control once per run
// (model.WithContractHorizonControl; S1, server.ContractDayRetention): a day
// the reaper has emptied is compacted down to the rows the reaper keeps and
// dropped once it holds none. The close evidence row deletes it ran until
// 2026-11-23 are their own chain (RemoveExpiredCloseEvidence); its cursor
// keeps their old position unread, for a build from before then. A run
// reaches each day's partitions a minute into the day, to analyze them.

type MaintainContractPartitionsArgs struct {
	Cursor server.ContractRetentionCursor `json:"cursor"`
}

type MaintainContractPartitionsResult struct {
	Cursor server.ContractRetentionCursor `json:"cursor"`
	More   bool                           `json:"more"`
	// when the next day's partitions are due their first analyze
	// (server.ContractPartitionReport.OpenLeafAnalyzeAt); zero from a build
	// before it, or before the cutover
	OpenLeafAnalyzeAt time.Time `json:"open_leaf_analyze_at,omitzero"`
}

// The chain's cadence and budget.
type MaintainContractPartitionsTaskSettings struct {
	// The first run after the chain is scheduled fresh starts this long after.
	StartDelay time.Duration
	// The chain runs again this long after a run that finished its work, and
	// RetryInterval after one that left work behind; at the latest when the
	// next day's partitions are due their first analyze.
	Interval      time.Duration
	RetryInterval time.Duration
	// A run's work budget, within MaxTime.
	Budget time.Duration
	// Each run's task max time.
	MaxTime time.Duration
}

// Every 15 minutes with a ten-minute budget, as the chain has run since the
// cutover.
func DefaultMaintainContractPartitionsTaskSettings() *MaintainContractPartitionsTaskSettings {
	return &MaintainContractPartitionsTaskSettings{
		StartDelay:    1 * time.Minute,
		Interval:      15 * time.Minute,
		RetryInterval: 1 * time.Minute,
		Budget:        10 * time.Minute,
		MaxTime:       30 * time.Minute,
	}
}

func ScheduleMaintainContractPartitions(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleMaintainContractPartitions(clientSession, tx, &MaintainContractPartitionsArgs{}, server.NowUtc().Add(DefaultMaintainContractPartitionsTaskSettings().StartDelay))
}

func scheduleMaintainContractPartitions(clientSession *session.ClientSession, tx server.PgTx, args *MaintainContractPartitionsArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		MaintainContractPartitions,
		args,
		clientSession,
		task.RunOnce("maintain_contract_partitions"),
		task.RunAt(runAt),
		task.MaxTime(DefaultMaintainContractPartitionsTaskSettings().MaxTime),
	)
}

// The next run's time, at now: the cadence's, or the next day's first analyze
// of its partitions when that comes first, at once when it passed while the
// run ran. On the cadence alone the day's partitions waited up to an interval
// past 00:00 UTC, or for autovacuum, which took about 15 minutes on
// 2026-11-25.
func maintainContractPartitionsNextRunAt(settings *MaintainContractPartitionsTaskSettings, result *MaintainContractPartitionsResult, now time.Time) time.Time {
	runAt := now.Add(settings.Interval)
	if result.More {
		runAt = now.Add(settings.RetryInterval)
	}
	if !result.OpenLeafAnalyzeAt.IsZero() && result.OpenLeafAnalyzeAt.Before(runAt) {
		runAt = result.OpenLeafAnalyzeAt
		if runAt.Before(now) {
			runAt = now
		}
	}
	return runAt
}

func MaintainContractPartitions(
	maintainContractPartitions *MaintainContractPartitionsArgs,
	clientSession *session.ClientSession,
) (*MaintainContractPartitionsResult, error) {
	ctx := model.WithContractHorizonControl(clientSession.Ctx)
	report, cursor := server.MaintainContractPartitions(
		ctx,
		server.NowUtc(),
		maintainContractPartitions.Cursor,
		DefaultMaintainContractPartitionsTaskSettings().Budget,
	)
	if 0 < len(report.Created) || 0 < len(report.Compacted) || 0 < len(report.Dropped) || report.AbandonedCutover || report.AbandonedEvidenceCutover {
		glog.Infof(
			"[cp]created %v, compacted %v, dropped %v, rows kept by day %v, abandoned cutover %t, abandoned close evidence cutover %t\n",
			report.Created,
			report.Compacted,
			report.Dropped,
			report.Kept,
			report.AbandonedCutover,
			report.AbandonedEvidenceCutover,
		)
	}
	if 0 < len(report.Analyzed) {
		glog.Infof("[cp]analyzed %v\n", report.Analyzed)
	}
	for partition, reason := range report.Blocked {
		glog.V(1).Infof("[cp]expired %s stays: %s\n", partition, reason)
	}
	if report.Partitioned && report.FutureDays < 2 {
		glog.Errorf("[cp]only %d future contract partition day(s) exist\n", report.FutureDays)
	}
	return &MaintainContractPartitionsResult{
		Cursor:            cursor,
		More:              report.More,
		OpenLeafAnalyzeAt: report.OpenLeafAnalyzeAt,
	}, nil
}

// A run that left work behind continues a minute later, and a run reaches
// each day's partitions a minute into the day
// (maintainContractPartitionsNextRunAt).
func MaintainContractPartitionsPost(
	maintainContractPartitions *MaintainContractPartitionsArgs,
	maintainContractPartitionsResult *MaintainContractPartitionsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleMaintainContractPartitions(
		clientSession,
		tx,
		&MaintainContractPartitionsArgs{Cursor: maintainContractPartitionsResult.Cursor},
		maintainContractPartitionsNextRunAt(DefaultMaintainContractPartitionsTaskSettings(), maintainContractPartitionsResult, server.NowUtc()),
	)
	return nil
}
