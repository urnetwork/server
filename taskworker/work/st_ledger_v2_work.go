package work

// st_ledger_v2_work.go — the settlement v2 driver's tasks (VERSION2-FINAL.md
// §8.1). Each StSyncChain turn asks the controller for the due steps and
// starts a chain for any step that has none; a step retries itself through
// its Post until it concludes. The v1 tasks keep only the epochs below the
// pool's first v2 epoch.

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const (
	stLedgerV2StepMaxAttempts = 100
	stLedgerV2StepRetryDelay  = 1 * time.Minute
	// A step may build a ledger and wait for several finalized sends.
	stLedgerV2StepMaxTime = 2 * time.Hour
)

// stV1Epoch reports whether the v1 tasks still own an epoch (SV-4). An
// unknown answer leaves the epoch unscheduled for this turn.
func stV1Epoch(ctx context.Context, epoch uint64) bool {
	allowed, err := controller.StV1EpochAllowed(ctx, epoch)
	if err != nil {
		glog.Infof("[st]v1 cutoff for epoch %d unknown: %s\n", epoch, err)
	}
	return err == nil && allowed
}

// scheduleStLedgerV2Steps starts a task chain for each due v2 step that has
// none; a pending or running chain keeps its own retry schedule.
func scheduleStLedgerV2Steps(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, state *controller.StEpochState) {
	steps, err := controller.StLedgerV2Sync(clientSession.Ctx, state)
	if err != nil {
		glog.Infof("[st]v2 sync failed: %s\n", err)
		return
	}
	for _, step := range steps {
		args := &StLedgerV2StepArgs{DeploymentKey: string(deploymentKey), Kind: step.Kind, Epoch: step.Epoch, Seq: step.Seq, RootIndex: step.RootIndex, Index: step.Index}
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			task.ScheduleTaskInTxIfAbsent(tx, StLedgerV2Step, args, clientSession, stLedgerV2StepRunOnce(args), task.RunAt(step.RunAt), task.MaxTime(stLedgerV2StepMaxTime))
		})
	}
}

func stLedgerV2StepRunOnce(args *StLedgerV2StepArgs) *task.RunOnceOption {
	return task.RunOnce("st_ledger_v2_step", args.DeploymentKey, args.Kind, args.Epoch, args.Seq, args.RootIndex, args.Index)
}

// StLedgerV2Step(kind, epoch, seq, rootIndex, index)

type StLedgerV2StepArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Kind          string `json:"kind"`
	Epoch         uint64 `json:"epoch"`
	Seq           uint64 `json:"seq"`
	RootIndex     uint64 `json:"root_index"`
	Index         uint64 `json:"index"`
	Attempt       int    `json:"attempt"`
}

type StLedgerV2StepResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func StLedgerV2Step(
	stLedgerV2Step *StLedgerV2StepArgs,
	clientSession *session.ClientSession,
) (*StLedgerV2StepResult, error) {
	if _, current := stTaskDeploymentCurrent(stLedgerV2Step.DeploymentKey); !current {
		return &StLedgerV2StepResult{}, nil
	}
	outcome, err := controller.StLedgerV2RunStep(clientSession.Ctx, &controller.StLedgerV2Step{
		Kind: stLedgerV2Step.Kind, Epoch: stLedgerV2Step.Epoch, Seq: stLedgerV2Step.Seq, RootIndex: stLedgerV2Step.RootIndex, Index: stLedgerV2Step.Index,
	})
	if err != nil {
		glog.Infof("[st]v2 %s step epoch %d seq %d root %d index %d failed (attempt %d): %s\n", stLedgerV2Step.Kind, stLedgerV2Step.Epoch, stLedgerV2Step.Seq, stLedgerV2Step.RootIndex, stLedgerV2Step.Index, stLedgerV2Step.Attempt, err)
		return &StLedgerV2StepResult{Retry: true}, nil
	}
	glog.Infof("[st]v2 %s step epoch %d seq %d root %d index %d: %s\n", stLedgerV2Step.Kind, stLedgerV2Step.Epoch, stLedgerV2Step.Seq, stLedgerV2Step.RootIndex, stLedgerV2Step.Index, outcome)
	result := &StLedgerV2StepResult{Retry: outcome.Retry}
	if outcome.Retry && !outcome.RetryAt.IsZero() {
		retryAt := outcome.RetryAt
		result.RetryAt = &retryAt
	}
	return result, nil
}

func StLedgerV2StepPost(
	stLedgerV2Step *StLedgerV2StepArgs,
	stLedgerV2StepResult *StLedgerV2StepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stLedgerV2Step.DeploymentKey); !current {
		return nil
	}
	if stLedgerV2StepResult.Retry && stLedgerV2Step.Attempt+1 < stLedgerV2StepMaxAttempts {
		runAt := server.NowUtc().Add(stLedgerV2StepRetryDelay)
		if stLedgerV2StepResult.RetryAt != nil && runAt.Before(*stLedgerV2StepResult.RetryAt) {
			runAt = *stLedgerV2StepResult.RetryAt
		}
		next := *stLedgerV2Step
		next.Attempt += 1
		task.ScheduleTaskInTx(tx, StLedgerV2Step, &next, clientSession, stLedgerV2StepRunOnce(&next), task.RunAt(runAt), task.MaxTime(stLedgerV2StepMaxTime))
	}
	return nil
}
