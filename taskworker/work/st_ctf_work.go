package work

// st_ctf_work.go — capture the flag's presence samples and census
// (controller/ctf_presence.go, controller/ctf_census.go). They are their own
// chains, never part of a settlement step, so a failure here delays no close,
// commit, deposit or usage statement. Each StSyncChain turn starts a chain for
// a due step that has none: the quarter hour's sample, and the census of the
// epoch that has just ended. A step retries itself through its Post until it
// concludes. Nothing is due while ctf.yml leaves capture the flag off.

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
	stCtfStepMaxAttempts = 100
	// a sample retries within its quarter hour; a census while its boundary is
	// not final, at most for its capture window
	stCtfStepRetryDelay = 15 * time.Second
	// a census reads every member's boundary facts and publishes its objects
	stCtfStepMaxTime = 30 * time.Minute
)

// scheduleStCtfSteps starts a task chain for each due capture the flag step that
// has none. Nothing it does can reach the sync turn's other work: a database or
// chain failure here, including tables not migrated yet, ends here.
func scheduleStCtfSteps(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, state *controller.StEpochState) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]ctf sync failed: %v\n", r)
		}
	}()
	steps, err := controller.StCtfSync(clientSession.Ctx, state)
	if err != nil {
		glog.Infof("[st]ctf sync failed: %s\n", err)
		return
	}
	for _, step := range steps {
		args := &StCtfStepArgs{
			DeploymentKey: string(deploymentKey),
			Kind:          step.Kind,
			Epoch:         step.Epoch,
			Index:         step.Index,
			HeadBlock:     step.HeadBlock,
			PlannedTime:   step.PlannedTime,
		}
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			task.ScheduleTaskInTxIfAbsent(tx, StCtfStep, args, clientSession, stCtfStepRunOnce(args), task.RunAt(step.RunAt), task.MaxTime(stCtfStepMaxTime))
		})
	}
}

func stCtfStepRunOnce(args *StCtfStepArgs) *task.RunOnceOption {
	return task.RunOnce("st_ctf_step", args.DeploymentKey, args.Kind, args.Epoch, args.Index)
}

// StCtfStep(kind, epoch, index)

type StCtfStepArgs struct {
	DeploymentKey string    `json:"deployment_key"`
	Kind          string    `json:"kind"`
	Epoch         uint64    `json:"epoch"`
	Index         int       `json:"index"`
	HeadBlock     uint64    `json:"head_block"`
	PlannedTime   time.Time `json:"planned_time"`
	Attempt       int       `json:"attempt"`
}

type StCtfStepResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func StCtfStep(
	stCtfStep *StCtfStepArgs,
	clientSession *session.ClientSession,
) (*StCtfStepResult, error) {
	if _, current := stTaskDeploymentCurrent(stCtfStep.DeploymentKey); !current {
		return &StCtfStepResult{}, nil
	}
	outcome, err := controller.StCtfRunStep(clientSession.Ctx, &controller.StCtfStep{
		Kind:        stCtfStep.Kind,
		Epoch:       stCtfStep.Epoch,
		Index:       stCtfStep.Index,
		HeadBlock:   stCtfStep.HeadBlock,
		PlannedTime: stCtfStep.PlannedTime,
	})
	if err != nil {
		glog.Infof("[st]%s step epoch %d/%d failed (attempt %d): %s\n", stCtfStep.Kind, stCtfStep.Epoch, stCtfStep.Index, stCtfStep.Attempt, err)
		return &StCtfStepResult{Retry: true}, nil
	}
	glog.Infof("[st]%s step epoch %d/%d: %s\n", stCtfStep.Kind, stCtfStep.Epoch, stCtfStep.Index, outcome)
	result := &StCtfStepResult{Retry: outcome.Retry}
	if outcome.Retry && !outcome.RetryAt.IsZero() {
		retryAt := outcome.RetryAt
		result.RetryAt = &retryAt
	}
	return result, nil
}

func StCtfStepPost(
	stCtfStep *StCtfStepArgs,
	stCtfStepResult *StCtfStepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stCtfStep.DeploymentKey); !current {
		return nil
	}
	if stCtfStepResult.Retry && stCtfStep.Attempt+1 < stCtfStepMaxAttempts {
		runAt := server.NowUtc().Add(stCtfStepRetryDelay)
		if stCtfStepResult.RetryAt != nil && runAt.Before(*stCtfStepResult.RetryAt) {
			runAt = *stCtfStepResult.RetryAt
		}
		next := *stCtfStep
		next.Attempt += 1
		task.ScheduleTaskInTx(tx, StCtfStep, &next, clientSession, stCtfStepRunOnce(&next), task.RunAt(runAt), task.MaxTime(stCtfStepMaxTime))
	}
	return nil
}
