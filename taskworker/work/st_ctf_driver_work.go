package work

// st_ctf_driver_work.go — the capture the flag driver's tasks (sn
// CAPTURETHEFLAG.md §11.2; controller/st_ctf_driver.go). They are their own
// chains, never part of a provider pool step, so nothing here delays the
// provider pool's close or commit. Each StSyncChain turn asks the controller
// for the due steps and starts a chain for any step that has none; a step
// retries itself through its Post until it concludes. With capture the flag
// off nothing is planned.

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
	stCtfDriverStepMaxAttempts = 200
	stCtfDriverStepRetryDelay  = 1 * time.Minute
	// A step may build a ledger over a large census and wait for several
	// finalized sends.
	stCtfDriverStepMaxTime = 2 * time.Hour
)

// scheduleStCtfDriverSteps starts a task chain for each due capture the flag step
// that has none. Nothing it does can reach the sync turn's other work: a
// database or chain failure here ends here.
func scheduleStCtfDriverSteps(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, state *controller.StEpochState) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]ctf sync failed: %v\n", r)
		}
	}()
	steps, err := controller.StCtfDriverSync(clientSession.Ctx, state)
	if err != nil {
		glog.Infof("[st]ctf sync failed: %s\n", err)
		return
	}
	for _, step := range steps {
		args := &StCtfDriverStepArgs{DeploymentKey: string(deploymentKey), Kind: step.Kind, Epoch: step.Epoch, Seq: step.Seq, RootIndex: step.RootIndex, Index: step.Index}
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			task.ScheduleTaskInTxIfAbsent(tx, StCtfDriverStep, args, clientSession, stCtfDriverStepRunOnce(args), task.RunAt(step.RunAt), task.MaxTime(stCtfDriverStepMaxTime))
		})
	}
}

func stCtfDriverStepRunOnce(args *StCtfDriverStepArgs) *task.RunOnceOption {
	return task.RunOnce("st_ctf_driver_step", args.DeploymentKey, args.Kind, args.Epoch, args.Seq, args.RootIndex, args.Index)
}

// StCtfDriverStep(kind, epoch, seq, rootIndex, index)

type StCtfDriverStepArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Kind          string `json:"kind"`
	Epoch         uint64 `json:"epoch"`
	Seq           uint64 `json:"seq"`
	RootIndex     uint64 `json:"root_index"`
	Index         uint64 `json:"index"`
	Attempt       int    `json:"attempt"`
}

type StCtfDriverStepResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func StCtfDriverStep(
	stCtfStep *StCtfDriverStepArgs,
	clientSession *session.ClientSession,
) (*StCtfDriverStepResult, error) {
	if _, current := stTaskDeploymentCurrent(stCtfStep.DeploymentKey); !current {
		return &StCtfDriverStepResult{}, nil
	}
	outcome, err := controller.StCtfDriverRunStep(clientSession.Ctx, &controller.StCtfDriverStep{
		Kind: stCtfStep.Kind, Epoch: stCtfStep.Epoch, Seq: stCtfStep.Seq, RootIndex: stCtfStep.RootIndex, Index: stCtfStep.Index,
	})
	if err != nil {
		glog.Infof("[st]ctf %s step epoch %d seq %d root %d index %d failed (attempt %d): %s\n", stCtfStep.Kind, stCtfStep.Epoch, stCtfStep.Seq, stCtfStep.RootIndex, stCtfStep.Index, stCtfStep.Attempt, err)
		return &StCtfDriverStepResult{Retry: true}, nil
	}
	glog.Infof("[st]ctf %s step epoch %d seq %d root %d index %d: %s\n", stCtfStep.Kind, stCtfStep.Epoch, stCtfStep.Seq, stCtfStep.RootIndex, stCtfStep.Index, outcome)
	result := &StCtfDriverStepResult{Retry: outcome.Retry}
	if outcome.Retry && !outcome.RetryAt.IsZero() {
		retryAt := outcome.RetryAt
		result.RetryAt = &retryAt
	}
	return result, nil
}

func StCtfDriverStepPost(
	stCtfStep *StCtfDriverStepArgs,
	stCtfStepResult *StCtfDriverStepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stCtfStep.DeploymentKey); !current {
		return nil
	}
	if stCtfStepResult.Retry && stCtfStep.Attempt+1 < stCtfDriverStepMaxAttempts {
		runAt := server.NowUtc().Add(stCtfDriverStepRetryDelay)
		if stCtfStepResult.RetryAt != nil && runAt.Before(*stCtfStepResult.RetryAt) {
			runAt = *stCtfStepResult.RetryAt
		}
		next := *stCtfStep
		next.Attempt += 1
		task.ScheduleTaskInTx(tx, StCtfDriverStep, &next, clientSession, stCtfDriverStepRunOnce(&next), task.RunAt(runAt), task.MaxTime(stCtfDriverStepMaxTime))
	}
	return nil
}
