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
// keeps their old position unread, for a build from before then.

type MaintainContractPartitionsArgs struct {
	Cursor server.ContractRetentionCursor `json:"cursor"`
}

type MaintainContractPartitionsResult struct {
	Cursor server.ContractRetentionCursor `json:"cursor"`
	More   bool                           `json:"more"`
}

// A run's work budget, within its task max time.
const maintainContractPartitionsBudget = 10 * time.Minute

func ScheduleMaintainContractPartitions(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleMaintainContractPartitions(clientSession, tx, &MaintainContractPartitionsArgs{}, server.NowUtc().Add(1*time.Minute))
}

func scheduleMaintainContractPartitions(clientSession *session.ClientSession, tx server.PgTx, args *MaintainContractPartitionsArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		MaintainContractPartitions,
		args,
		clientSession,
		task.RunOnce("maintain_contract_partitions"),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
	)
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
		maintainContractPartitionsBudget,
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
	for partition, reason := range report.Blocked {
		glog.V(1).Infof("[cp]expired %s stays: %s\n", partition, reason)
	}
	if report.Partitioned && report.FutureDays < 2 {
		glog.Errorf("[cp]only %d future contract partition day(s) exist\n", report.FutureDays)
	}
	return &MaintainContractPartitionsResult{
		Cursor: cursor,
		More:   report.More,
	}, nil
}

// A run that left work behind continues a minute later.
func MaintainContractPartitionsPost(
	maintainContractPartitions *MaintainContractPartitionsArgs,
	maintainContractPartitionsResult *MaintainContractPartitionsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	runAt := server.NowUtc().Add(15 * time.Minute)
	if maintainContractPartitionsResult.More {
		runAt = server.NowUtc().Add(1 * time.Minute)
	}
	scheduleMaintainContractPartitions(
		clientSession,
		tx,
		&MaintainContractPartitionsArgs{Cursor: maintainContractPartitionsResult.Cursor},
		runAt,
	)
	return nil
}
