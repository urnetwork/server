// CheckContractDegradation is the 15 minute run-once owner of the contract
// degradation valve (model/network_degradation_model.go). Each run measures the
// last hour's contract creations against terminal outcomes and publishes
// whether the valve is open, which lets a payer whose balance escrow finds
// exhausted get a free contract. config/<env>/degraded.yml enables it:
// startup seeds the owner only while enabled and reaps a left-over row while
// disabled, and a run while disabled measures and writes nothing and schedules
// no successor.
package work

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A check holds no lock and each count has its own statement timeout.
const checkContractDegradationMaxTime = 5 * time.Minute

type CheckContractDegradationArgs struct {
}

type CheckContractDegradationResult struct {
	// degraded.yml did not enable the check, so nothing ran or was written
	Disabled bool `json:"disabled,omitempty"`
}

// Seeds the owner to run now, only while degraded.yml enables it. Run-once
// makes repeated initialization idempotent.
func ScheduleCheckContractDegradation(clientSession *session.ClientSession, tx server.PgTx) {
	if !model.NetworkDegradationEnabled() {
		return
	}
	scheduleCheckContractDegradationAt(clientSession, tx, server.NowUtc())
}

// The single run-once key, shared by startup and the post.
func scheduleCheckContractDegradationAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		CheckContractDegradation,
		&CheckContractDegradationArgs{},
		clientSession,
		task.RunOnce("check_contract_degradation"),
		task.RunAt(runAt),
		task.MaxTime(checkContractDegradationMaxTime),
	)
}

// One check. A failed count or Redis call is logged and counted by the model
// and publishes nothing; the run still succeeds so the 15 minute cadence holds,
// and the published state expires on its own if checks keep failing.
func CheckContractDegradation(
	checkContractDegradation *CheckContractDegradationArgs,
	clientSession *session.ClientSession,
) (*CheckContractDegradationResult, error) {
	if !model.NetworkDegradationEnabled() {
		return &CheckContractDegradationResult{Disabled: true}, nil
	}
	model.CheckContractDegradation(clientSession.Ctx, server.NowUtc())
	return &CheckContractDegradationResult{}, nil
}

// Schedules the next check one period later while the check stays enabled.
func CheckContractDegradationPost(
	checkContractDegradation *CheckContractDegradationArgs,
	checkContractDegradationResult *CheckContractDegradationResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if checkContractDegradationResult.Disabled || !model.NetworkDegradationEnabled() {
		return nil
	}
	scheduleCheckContractDegradationAt(clientSession, tx, server.NowUtc().Add(model.ContractDegradationCheckInterval))
	return nil
}

// The pending-task surface of the check, derived from the registered function.
func CheckContractDegradationTaskFunctionNames() []string {
	return []string{
		task.NewTaskTarget(CheckContractDegradation).TargetFunctionName(),
	}
}

// Removes a left-over owner after degraded.yml stops enabling the check.
func RemoveDisabledContractDegradationTasks(ctx context.Context, tx server.PgTx) int64 {
	if model.NetworkDegradationEnabled() {
		return 0
	}
	var removedCount int64
	for _, functionName := range CheckContractDegradationTaskFunctionNames() {
		removedCount += task.RemovePendingTasksForFunctionInTx(ctx, tx, functionName)
	}
	return removedCount
}
