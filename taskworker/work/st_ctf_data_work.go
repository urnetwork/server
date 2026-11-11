package work

// st_ctf_data_work.go — capture the flag's presence samples and census
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
	stCtfDataStepMaxAttempts = 100
	// a sample retries within its quarter hour; a census while its boundary is
	// not final, at most for its capture window
	stCtfDataStepRetryDelay = 15 * time.Second
	// a census reads every member's boundary facts and publishes its objects
	stCtfDataStepMaxTime = 30 * time.Minute
)

// scheduleStCtfDataSteps starts a task chain for each due capture the flag step that
// has none. Nothing it does can reach the sync turn's other work: a database or
// chain failure here, including tables not migrated yet, ends here.
func scheduleStCtfDataSteps(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, state *controller.StEpochState) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]ctf data sync failed: %v\n", r)
		}
	}()
	steps, err := controller.StCtfDataSync(clientSession.Ctx, state)
	if err != nil {
		glog.Infof("[st]ctf data sync failed: %s\n", err)
		return
	}
	for _, step := range steps {
		args := &StCtfDataStepArgs{
			DeploymentKey: string(deploymentKey),
			Kind:          step.Kind,
			Epoch:         step.Epoch,
			Index:         step.Index,
			HeadBlock:     step.HeadBlock,
			PlannedTime:   step.PlannedTime,
		}
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			task.ScheduleTaskInTxIfAbsent(tx, StCtfDataStep, args, clientSession, stCtfDataStepRunOnce(args), task.RunAt(step.RunAt), task.MaxTime(stCtfDataStepMaxTime))
		})
	}
}

func stCtfDataStepRunOnce(args *StCtfDataStepArgs) *task.RunOnceOption {
	return task.RunOnce("st_ctf_data_step", args.DeploymentKey, args.Kind, args.Epoch, args.Index)
}

// StCtfDataStep(kind, epoch, index)

type StCtfDataStepArgs struct {
	DeploymentKey string    `json:"deployment_key"`
	Kind          string    `json:"kind"`
	Epoch         uint64    `json:"epoch"`
	Index         int       `json:"index"`
	HeadBlock     uint64    `json:"head_block"`
	PlannedTime   time.Time `json:"planned_time"`
	Attempt       int       `json:"attempt"`
}

type StCtfDataStepResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func StCtfDataStep(
	stCtfDataStep *StCtfDataStepArgs,
	clientSession *session.ClientSession,
) (*StCtfDataStepResult, error) {
	if _, current := stTaskDeploymentCurrent(stCtfDataStep.DeploymentKey); !current {
		return &StCtfDataStepResult{}, nil
	}
	outcome, err := controller.StCtfDataRunStep(clientSession.Ctx, &controller.StCtfDataStep{
		Kind:        stCtfDataStep.Kind,
		Epoch:       stCtfDataStep.Epoch,
		Index:       stCtfDataStep.Index,
		HeadBlock:   stCtfDataStep.HeadBlock,
		PlannedTime: stCtfDataStep.PlannedTime,
	})
	if err != nil {
		glog.Infof("[st]%s step epoch %d/%d failed (attempt %d): %s\n", stCtfDataStep.Kind, stCtfDataStep.Epoch, stCtfDataStep.Index, stCtfDataStep.Attempt, err)
		return &StCtfDataStepResult{Retry: true}, nil
	}
	glog.Infof("[st]%s step epoch %d/%d: %s\n", stCtfDataStep.Kind, stCtfDataStep.Epoch, stCtfDataStep.Index, outcome)
	result := &StCtfDataStepResult{Retry: outcome.Retry}
	if outcome.Retry && !outcome.RetryAt.IsZero() {
		retryAt := outcome.RetryAt
		result.RetryAt = &retryAt
	}
	return result, nil
}

func StCtfDataStepPost(
	stCtfDataStep *StCtfDataStepArgs,
	stCtfDataStepResult *StCtfDataStepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stCtfDataStep.DeploymentKey); !current {
		return nil
	}
	if stCtfDataStepResult.Retry && stCtfDataStep.Attempt+1 < stCtfDataStepMaxAttempts {
		runAt := server.NowUtc().Add(stCtfDataStepRetryDelay)
		if stCtfDataStepResult.RetryAt != nil && runAt.Before(*stCtfDataStepResult.RetryAt) {
			runAt = *stCtfDataStepResult.RetryAt
		}
		next := *stCtfDataStep
		next.Attempt += 1
		task.ScheduleTaskInTx(tx, StCtfDataStep, &next, clientSession, stCtfDataStepRunOnce(&next), task.RunAt(runAt), task.MaxTime(stCtfDataStepMaxTime))
	}
	return nil
}
