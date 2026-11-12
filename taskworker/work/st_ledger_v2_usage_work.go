package work

// st_ledger_v2_usage_work.go — the settlement v2 usage statement's tasks
// (controller/st_ledger_v2_usage.go). They are their own chain, never part of a v2
// driver step or a v1 task, so a failure here delays no close, commit or phase C
// sweep. Each StSyncChain turn starts a chain for a due step that has none; a step
// retries itself through its Post until it concludes, and a chain that reaches its
// cap is restarted by the next turn.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const (
	stLedgerV2UsageStepMaxAttempts = 100
	// The least delay between turns: users are captured as close to the boundary
	// as a failed read allows.
	stLedgerV2UsageStepRetryDelay = 15 * time.Second
	// A step reads the epoch's provider chunks and publishes one small object.
	stLedgerV2UsageStepMaxTime = 30 * time.Minute
	// An extender shadow step reads the epoch's contracts as a root's build
	// does, so it has the v2 driver's time.
	stLedgerV2UsageShadowStepMaxTime = 2 * time.Hour
)

// stLedgerV2UsageStepMaxTimeOf is a usage step's time limit by kind.
func stLedgerV2UsageStepMaxTimeOf(kind string) time.Duration {
	if kind == controller.StLedgerV2UsageStepExtenderShadow {
		return stLedgerV2UsageShadowStepMaxTime
	}
	return stLedgerV2UsageStepMaxTime
}

// scheduleStLedgerV2UsageSteps starts a task chain for each due usage step that has
// none. Nothing it does can reach the sync turn's other work: a database or chain
// failure here, including tables not migrated yet, ends here.
func scheduleStLedgerV2UsageSteps(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, state *controller.StEpochState) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]v2 usage sync failed: %v\n", r)
		}
	}()
	steps, err := controller.StLedgerV2UsageSync(clientSession.Ctx, state)
	if err != nil {
		glog.Infof("[st]v2 usage sync failed: %s\n", err)
		return
	}
	for _, step := range steps {
		args := &StLedgerV2UsageStepArgs{DeploymentKey: string(deploymentKey), Kind: step.Kind, Epoch: step.Epoch}
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			task.ScheduleTaskInTxIfAbsent(tx, StLedgerV2UsageStep, args, clientSession, stLedgerV2UsageStepRunOnce(args), task.RunAt(step.RunAt), task.MaxTime(stLedgerV2UsageStepMaxTimeOf(step.Kind)))
		})
	}
}

func stLedgerV2UsageStepRunOnce(args *StLedgerV2UsageStepArgs) *task.RunOnceOption {
	return task.RunOnce("st_ledger_v2_usage_step", args.DeploymentKey, args.Kind, args.Epoch)
}

// StLedgerV2UsageStep(kind, epoch)

type StLedgerV2UsageStepArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Kind          string `json:"kind"`
	Epoch         uint64 `json:"epoch"`
	Attempt       int    `json:"attempt"`
}

type StLedgerV2UsageStepResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func StLedgerV2UsageStep(
	stLedgerV2UsageStep *StLedgerV2UsageStepArgs,
	clientSession *session.ClientSession,
) (*StLedgerV2UsageStepResult, error) {
	if _, current := stTaskDeploymentCurrent(stLedgerV2UsageStep.DeploymentKey); !current {
		return &StLedgerV2UsageStepResult{}, nil
	}
	outcome, err := controller.StLedgerV2RunUsageStep(clientSession.Ctx, &controller.StLedgerV2UsageStep{
		Kind: stLedgerV2UsageStep.Kind, Epoch: stLedgerV2UsageStep.Epoch,
	})
	if err != nil {
		glog.Infof("[st]v2 %s step epoch %d failed (attempt %d): %s\n", stLedgerV2UsageStep.Kind, stLedgerV2UsageStep.Epoch, stLedgerV2UsageStep.Attempt, err)
		return &StLedgerV2UsageStepResult{Retry: true}, nil
	}
	glog.Infof("[st]v2 %s step epoch %d: %s\n", stLedgerV2UsageStep.Kind, stLedgerV2UsageStep.Epoch, outcome)
	result := &StLedgerV2UsageStepResult{Retry: outcome.Retry}
	if outcome.Retry && !outcome.RetryAt.IsZero() {
		retryAt := outcome.RetryAt
		result.RetryAt = &retryAt
	}
	return result, nil
}

func StLedgerV2UsageStepPost(
	stLedgerV2UsageStep *StLedgerV2UsageStepArgs,
	stLedgerV2UsageStepResult *StLedgerV2UsageStepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stLedgerV2UsageStep.DeploymentKey); !current {
		return nil
	}
	if stLedgerV2UsageStepResult.Retry && stLedgerV2UsageStep.Attempt+1 < stLedgerV2UsageStepMaxAttempts {
		runAt := server.NowUtc().Add(stLedgerV2UsageStepRetryDelay)
		if stLedgerV2UsageStepResult.RetryAt != nil && runAt.Before(*stLedgerV2UsageStepResult.RetryAt) {
			runAt = *stLedgerV2UsageStepResult.RetryAt
		}
		next := *stLedgerV2UsageStep
		next.Attempt += 1
		task.ScheduleTaskInTx(tx, StLedgerV2UsageStep, &next, clientSession, stLedgerV2UsageStepRunOnce(&next), task.RunAt(runAt), task.MaxTime(stLedgerV2UsageStepMaxTimeOf(next.Kind)))
	}
	return nil
}
