package work

// st_v1_sweep_work.go — the phase C v1 carry sweep chain
// (controller/st_v1_sweep.go). Each StSyncChain turn starts the chain while a
// sweep is configured and has not concluded; the chain continues itself
// through its Post. It is its own task, never part of a v1 epoch task or a
// v2 step, so the v2 epoch step at the same boundary never waits on it.

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
	// The chain spans days (close, finalize after 14,400 blocks, claim,
	// absorb, assign); a chain that reaches the cap is restarted by the next
	// StSyncChain turn.
	stV1SweepMaxAttempts = 1000
	// One turn sends at most one transaction and waits for its finality.
	stV1SweepMaxTime = 30 * time.Minute
	// The least delay between turns.
	stV1SweepRetryDelay = 15 * time.Second
)

type StV1SweepArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Epoch         uint64 `json:"epoch"`
	Attempt       int    `json:"attempt"`
}

type StV1SweepResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func stV1SweepRunOnce(args *StV1SweepArgs) *task.RunOnceOption {
	return task.RunOnce("st_v1_sweep", args.DeploymentKey, args.Epoch)
}

// scheduleStV1Sweep starts the sweep chain when one is due and none is
// pending; a pending or running chain keeps its own schedule.
func scheduleStV1Sweep(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey) {
	epoch, due := controller.StV1SweepDue(clientSession.Ctx)
	if !due {
		return
	}
	args := &StV1SweepArgs{DeploymentKey: string(deploymentKey), Epoch: epoch}
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		task.ScheduleTaskInTxIfAbsent(tx, StV1Sweep, args, clientSession, stV1SweepRunOnce(args), task.RunAt(server.NowUtc()), task.MaxTime(stV1SweepMaxTime))
	})
}

func StV1Sweep(
	stV1Sweep *StV1SweepArgs,
	clientSession *session.ClientSession,
) (*StV1SweepResult, error) {
	if _, current := stTaskDeploymentCurrent(stV1Sweep.DeploymentKey); !current {
		return &StV1SweepResult{}, nil
	}
	outcome, err := controller.StV1SweepTurn(clientSession.Ctx)
	if err != nil {
		glog.Infof("[st]v1 sweep epoch %d turn failed (attempt %d): %s\n", stV1Sweep.Epoch, stV1Sweep.Attempt, err)
		return &StV1SweepResult{Retry: true}, nil
	}
	glog.Infof("[st]v1 sweep epoch %d: %s\n", stV1Sweep.Epoch, outcome)
	result := &StV1SweepResult{Retry: outcome.Retry}
	if outcome.Retry && !outcome.RetryAt.IsZero() {
		retryAt := outcome.RetryAt
		result.RetryAt = &retryAt
	}
	return result, nil
}

func StV1SweepPost(
	stV1Sweep *StV1SweepArgs,
	stV1SweepResult *StV1SweepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stV1Sweep.DeploymentKey); !current {
		return nil
	}
	if stV1SweepResult.Retry && stV1Sweep.Attempt+1 < stV1SweepMaxAttempts {
		runAt := server.NowUtc().Add(stV1SweepRetryDelay)
		if stV1SweepResult.RetryAt != nil && runAt.Before(*stV1SweepResult.RetryAt) {
			runAt = *stV1SweepResult.RetryAt
		}
		next := *stV1Sweep
		next.Attempt += 1
		task.ScheduleTaskInTx(tx, StV1Sweep, &next, clientSession, stV1SweepRunOnce(&next), task.RunAt(runAt), task.MaxTime(stV1SweepMaxTime))
	}
	return nil
}
