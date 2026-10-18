// CheckContractDegradation is the 15 minute run-once owner of the contract
// degradation valve (model/network_degradation_model.go). Each run measures the
// last hour's contract creations against terminal outcomes and publishes
// whether the valve is open, which lets a payer whose balance escrow finds
// exhausted get a free contract. config/<env>/degraded.yml enables it:
// startup seeds the owner only while enabled and reaps a left-over row while
// disabled, and a run while disabled measures and writes nothing and schedules
// no successor. The owner carries the config stamp of the taskworker that
// wrote it (config_generation.go): a taskworker whose older config disables
// the check neither reaps nor retires an owner that a newer config's
// instances still restamp, and hands a claimed one back unchanged.
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

// An owner that a newer config wrote stays that config's while its stamp is
// this recent. That config's instances restamp it at every check, one check
// period apart; twice the period and the check's max time covers a claim
// handed back in between and a restart.
const checkContractDegradationNewerConfigTimeout = 2 * (model.ContractDegradationCheckInterval + checkContractDegradationMaxTime)

type CheckContractDegradationArgs struct {
	// The config version that wrote the owner, and when.
	ConfigStamp
}

type CheckContractDegradationResult struct {
	// degraded.yml did not enable the check, so nothing ran or was written
	Disabled bool `json:"disabled,omitempty"`
	// degraded.yml disables the check here, but a newer config's live owner
	// was claimed: nothing ran, and the post-step reschedules it unchanged
	HandedBack bool `json:"handed_back,omitempty"`
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
func checkContractDegradationRunOnce() *task.RunOnceOption {
	return task.RunOnce("check_contract_degradation")
}

// Schedules the owner, stamped by this process.
func scheduleCheckContractDegradationAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	scheduleCheckContractDegradationArgsAt(clientSession, tx, &CheckContractDegradationArgs{ConfigStamp: newConfigStamp(server.NowUtc())}, runAt)
}

// Schedules the owner with the given arguments, merging into a pending owner.
func scheduleCheckContractDegradationArgsAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	checkContractDegradation *CheckContractDegradationArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		CheckContractDegradation,
		checkContractDegradation,
		clientSession,
		checkContractDegradationRunOnce(),
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
		// An older config does not disable a check that a newer config enables.
		if checkContractDegradation != nil &&
			checkContractDegradation.newerWithin(server.NowUtc(), checkContractDegradationNewerConfigTimeout) {
			configHandbacks.WithLabelValues("contract_degradation").Inc()
			return &CheckContractDegradationResult{HandedBack: true}, nil
		}
		return &CheckContractDegradationResult{Disabled: true}, nil
	}
	model.CheckContractDegradation(clientSession.Ctx, server.NowUtc())
	return &CheckContractDegradationResult{}, nil
}

// Schedules the next check one period later while the check stays enabled. A
// handed-back owner is due again shortly, with its arguments as stored.
func CheckContractDegradationPost(
	checkContractDegradation *CheckContractDegradationArgs,
	checkContractDegradationResult *CheckContractDegradationResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if checkContractDegradationResult != nil && checkContractDegradationResult.HandedBack {
		runAt := server.NowUtc().Add(configHandbackDelay)
		rescheduleHandedBackTaskInTx(clientSession, tx, checkContractDegradationRunOnce(), runAt, func() {
			scheduleCheckContractDegradationArgsAt(clientSession, tx, checkContractDegradation, runAt)
		})
		return nil
	}
	if (checkContractDegradationResult != nil && checkContractDegradationResult.Disabled) || !model.NetworkDegradationEnabled() {
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

// Removes a left-over owner after degraded.yml stops enabling the check. An
// owner that a newer config wrote within its timeout stays: an older config
// does not disable that config's check.
func RemoveDisabledContractDegradationTasks(ctx context.Context, tx server.PgTx) int64 {
	if model.NetworkDegradationEnabled() {
		return 0
	}
	now := server.NowUtc()
	for _, stamp := range readConfigStampsInTx(ctx, tx, []string{checkContractDegradationRunOnce().String()}) {
		if stamp.newerWithin(now, checkContractDegradationNewerConfigTimeout) {
			return 0
		}
	}
	var removedCount int64
	for _, functionName := range CheckContractDegradationTaskFunctionNames() {
		removedCount += task.RemovePendingTasksForFunctionInTx(ctx, tx, functionName)
	}
	return removedCount
}
