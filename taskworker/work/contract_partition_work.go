package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Partition maintenance and retention of the contract tables
// (CONTRACT-PARTITION.md; see server.MaintainContractPartitions). Before the
// cutover a run only guards a prepared cutover and removes expired close
// evidence, so this build is safe on either side of the cutover.

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
	report, cursor := server.MaintainContractPartitions(
		clientSession.Ctx,
		server.NowUtc(),
		maintainContractPartitions.Cursor,
		maintainContractPartitionsBudget,
	)
	if 0 < len(report.Created) || 0 < len(report.Dropped) || 0 < report.Archived || 0 < report.EvidenceRemoved || report.AbandonedCutover {
		glog.Infof(
			"[cp]created %v, dropped %v, archived %d contracts, removed %d close evidence rows, abandoned cutover %t\n",
			report.Created,
			report.Dropped,
			report.Archived,
			report.EvidenceRemoved,
			report.AbandonedCutover,
		)
	}
	for partition, reason := range report.Blocked {
		glog.V(1).Infof("[cp]expired %s stays: %s\n", partition, reason)
	}
	if report.Partitioned && report.FutureWeeks < 2 {
		glog.Errorf("[cp]only %d future contract partition week(s) exist\n", report.FutureWeeks)
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
