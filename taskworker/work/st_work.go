package work

// st_work.go — the subtensor (st) epoch pipeline (sn/PLAN.md §6), modeled
// on the Schedule/Task/TaskPost trio pattern of account_payment_work.go
// with per-epoch RunOnce keys:
//
//   - StSyncChain (periodic ~1min): pokes rollEpochs when the lazy counter
//     is behind, mirrors the contract epoch state into `st_epoch` + the
//     redis summary cache, advances the event mirror in bounded ranges,
//     and schedules the per-epoch tasks below. Task RunAt values are only
//     derived from contract block deadlines (~12s/block estimate) — the
//     contract clock stays authoritative; every deadline decision is
//     re-checked in blocks inside the controller flows.
//   - StEpochClose(e): compute + store the payout leaves/root; mark closed.
//   - StCommitRoot(e): idempotent commitOperator within the +commitWindow
//     deadline; bounded retries via task reschedule; T-2h alert (D-11).
//   - StDeposit(e): reference-rate × previous-epoch-usage sizing, capped
//     per epoch (D-3); push-then-credit, both publish kinds recorded.
//   - StFinalizePoke(e): permissionless finalizeEpoch at/after the
//     finalize block; marks the epoch finalized.
//
// Task errors are folded into results (the Payout precedent) so the Post
// hooks control every reschedule. All tasks no-op when `st.yml` is absent
// or disabled (controller.StEnabled).

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
	// stSyncChainInterval is the periodic chain sync cadence.
	stSyncChainInterval = 1 * time.Minute

	// bounded retry budgets per epoch task chain. Commit retries are
	// additionally cut off by the on-chain commit window; finalize by the
	// sync task re-seeding the chain when it lapses.
	stEpochCloseMaxAttempts   = 100
	stCommitRootMaxAttempts   = 100
	stDepositMaxAttempts      = 8
	stFinalizePokeMaxAttempts = 100

	stEpochCloseRetryDelay   = 2 * time.Minute
	stCommitRootRetryDelay   = 5 * time.Minute
	stDepositRetryDelay      = 15 * time.Minute
	stFinalizePokeRetryDelay = 5 * time.Minute
)

// StSyncChain (periodic)

type StSyncChainArgs struct {
	DeploymentKey string `json:"deployment_key"`
}

type StSyncChainResult struct {
}

func ScheduleStSyncChain(clientSession *session.ClientSession, tx server.PgTx) {
	deploymentKey, _ := controller.StDeploymentKey()
	task.ScheduleTaskInTx(
		tx,
		StSyncChain,
		&StSyncChainArgs{DeploymentKey: string(deploymentKey)},
		clientSession,
		task.RunOnce("st_sync_chain", deploymentKey),
		task.RunAt(server.NowUtc().Add(stSyncChainInterval)),
		task.MaxTime(15*time.Minute),
	)
}

func StSyncChain(
	stSyncChain *StSyncChainArgs,
	clientSession *session.ClientSession,
) (*StSyncChainResult, error) {
	deploymentKey, current := stTaskDeploymentCurrent(stSyncChain.DeploymentKey)
	if !current {
		return &StSyncChainResult{}, nil
	}
	ctx := clientSession.Ctx

	state, err := controller.StSyncChainState(ctx)
	if err != nil {
		glog.Infof("[st]sync chain state failed: %s\n", err)
		return &StSyncChainResult{}, nil
	}

	if _, err := controller.StSyncChainEvents(ctx, state.HeadBlock); err != nil {
		glog.Infof("[st]sync chain events failed: %s\n", err)
	}

	// mirror an epoch the worker slept through (down across a boundary)
	if 0 < state.Epoch {
		if backfilled, err := controller.StBackfillEpochRow(ctx, state, state.Epoch-1); err == nil && backfilled {
			glog.Infof("[st]backfilled epoch %d mirror row\n", state.Epoch-1)
		}
	}

	// closed-but-still-open rows -> compute leaves + root
	for _, stEpoch := range model.GetStEpochsWithStatus(ctx, deploymentKey, model.StEpochStatusOpen) {
		if stEpoch.Epoch < state.Epoch {
			scheduleStEpochClose(clientSession, deploymentKey, stEpoch.Epoch, 0, server.NowUtc())
		}
	}

	// closed rows -> commit within the window; finalize poke at the
	// finalize block either way (a missed commit still finalizes — the
	// pool total carries, D-11)
	for _, stEpoch := range model.GetStEpochsWithStatus(ctx, deploymentKey, model.StEpochStatusClosed) {
		if state.HeadBlock <= stEpoch.CommitDeadlineBlock {
			scheduleStCommitRoot(clientSession, deploymentKey, stEpoch.Epoch, 0, server.NowUtc())
		}
		scheduleStFinalizePoke(clientSession, deploymentKey, stEpoch.Epoch, 0, stBlockRunAt(state, stEpoch.FinalizeBlock))
	}
	for _, stEpoch := range model.GetStEpochsWithStatus(ctx, deploymentKey, model.StEpochStatusCommitted) {
		scheduleStFinalizePoke(clientSession, deploymentKey, stEpoch.Epoch, 0, stBlockRunAt(state, stEpoch.FinalizeBlock))
	}

	// one automated deposit chain per (current) epoch
	if !stHasPublishKind(ctx, deploymentKey, state.Epoch, model.StPublishKindDeposit) {
		scheduleStDeposit(clientSession, deploymentKey, state.Epoch, 0, server.NowUtc())
	}

	return &StSyncChainResult{}, nil
}

func StSyncChainPost(
	stSyncChain *StSyncChainArgs,
	stSyncChainResult *StSyncChainResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleStSyncChain(clientSession, tx)
	return nil
}

// stBlockRunAt estimates the wall-clock RunAt for a contract block deadline
// (never in the past). The estimate only times the task wake-up; the
// controller re-checks the actual block window on chain.
func stBlockRunAt(state *controller.StEpochState, block uint64) time.Time {
	runAt := controller.StEstimateBlockTime(state.HeadBlock, state.HeadBlockTime, block)
	now := server.NowUtc()
	if runAt.Before(now) {
		return now
	}
	return runAt
}

// stHasPublishKind reports whether any publish row of the kind exists for
// the epoch (used to issue exactly one automated deposit chain per epoch —
// retries beyond the first chain go through StDepositPost or the
// `bringyourctl st deposit` manual fallback, D-3).
func stHasPublishKind(ctx context.Context, deploymentKey model.StDeploymentKey, epoch uint64, kind string) bool {
	for _, publish := range model.GetStPublishes(ctx, deploymentKey, epoch) {
		if publish.Kind == kind {
			return true
		}
	}
	return false
}

// stTaskDeploymentCurrent rejects persisted work from an earlier coordinator
// before it can read or write the active chain. Old JSON payloads have an empty
// key and therefore fail closed after an upgrade.
func stTaskDeploymentCurrent(taskDeploymentKey string) (model.StDeploymentKey, bool) {
	if !controller.StEnabled() || taskDeploymentKey == "" {
		return "", false
	}
	deploymentKey, ok := controller.StDeploymentKey()
	if !ok || taskDeploymentKey != string(deploymentKey) {
		return "", false
	}
	return deploymentKey, true
}

// StEpochClose(epoch)

type StEpochCloseArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Epoch         uint64 `json:"epoch"`
	Attempt       int    `json:"attempt"`
}

type StEpochCloseResult struct {
	Retry bool `json:"retry"`
}

func scheduleStEpochClose(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, epoch uint64, attempt int, runAt time.Time) {
	task.ScheduleTask(
		StEpochClose,
		&StEpochCloseArgs{DeploymentKey: string(deploymentKey), Epoch: epoch, Attempt: attempt},
		clientSession,
		task.RunOnce("st_epoch_close", deploymentKey, epoch),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

func StEpochClose(
	stEpochClose *StEpochCloseArgs,
	clientSession *session.ClientSession,
) (*StEpochCloseResult, error) {
	deploymentKey, current := stTaskDeploymentCurrent(stEpochClose.DeploymentKey)
	if !current {
		return &StEpochCloseResult{}, nil
	}
	ctx := clientSession.Ctx

	root, leafCount, err := controller.StComputeEpochPayout(ctx, stEpochClose.Epoch)
	if err != nil {
		glog.Infof("[st]epoch %d close failed (attempt %d): %s\n", stEpochClose.Epoch, stEpochClose.Attempt, err)
		return &StEpochCloseResult{Retry: true}, nil
	}
	model.SetStEpochStatus(ctx, deploymentKey, stEpochClose.Epoch, model.StEpochStatusClosed)
	glog.Infof("[st]epoch %d closed: %d leaves root 0x%x\n", stEpochClose.Epoch, leafCount, root)
	return &StEpochCloseResult{}, nil
}

func StEpochClosePost(
	stEpochClose *StEpochCloseArgs,
	stEpochCloseResult *StEpochCloseResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stEpochClose.DeploymentKey); !current {
		return nil
	}
	if stEpochCloseResult.Retry {
		if stEpochClose.Attempt+1 < stEpochCloseMaxAttempts {
			task.ScheduleTaskInTx(
				tx,
				StEpochClose,
				&StEpochCloseArgs{DeploymentKey: stEpochClose.DeploymentKey, Epoch: stEpochClose.Epoch, Attempt: stEpochClose.Attempt + 1},
				clientSession,
				task.RunOnce("st_epoch_close", stEpochClose.DeploymentKey, stEpochClose.Epoch),
				task.RunAt(server.NowUtc().Add(stEpochCloseRetryDelay)),
				task.MaxTime(1*time.Hour),
			)
		}
		return nil
	}
	// the commit window opened at the epoch boundary — commit immediately
	task.ScheduleTaskInTx(
		tx,
		StCommitRoot,
		&StCommitRootArgs{DeploymentKey: stEpochClose.DeploymentKey, Epoch: stEpochClose.Epoch},
		clientSession,
		task.RunOnce("st_commit_root", stEpochClose.DeploymentKey, stEpochClose.Epoch),
		task.MaxTime(30*time.Minute),
	)
	return nil
}

// StCommitRoot(epoch)

type StCommitRootArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Epoch         uint64 `json:"epoch"`
	Attempt       int    `json:"attempt"`
}

type StCommitRootResult struct {
	Retry bool `json:"retry"`
}

func scheduleStCommitRoot(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, epoch uint64, attempt int, runAt time.Time) {
	task.ScheduleTask(
		StCommitRoot,
		&StCommitRootArgs{DeploymentKey: string(deploymentKey), Epoch: epoch, Attempt: attempt},
		clientSession,
		task.RunOnce("st_commit_root", deploymentKey, epoch),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
	)
}

func StCommitRoot(
	stCommitRoot *StCommitRootArgs,
	clientSession *session.ClientSession,
) (*StCommitRootResult, error) {
	if _, current := stTaskDeploymentCurrent(stCommitRoot.DeploymentKey); !current {
		return &StCommitRootResult{}, nil
	}
	ctx := clientSession.Ctx

	outcome, err := controller.StCommitEpochRoot(ctx, stCommitRoot.Epoch)
	if err != nil {
		glog.Infof("[st]epoch %d commit failed (attempt %d): %s\n", stCommitRoot.Epoch, stCommitRoot.Attempt, err)
		return &StCommitRootResult{Retry: true}, nil
	}
	if glog.V(1) {
		glog.Infof("[st]epoch %d commit: %s\n", stCommitRoot.Epoch, outcome)
	}
	return &StCommitRootResult{Retry: outcome.Retry}, nil
}

func StCommitRootPost(
	stCommitRoot *StCommitRootArgs,
	stCommitRootResult *StCommitRootResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stCommitRoot.DeploymentKey); !current {
		return nil
	}
	if stCommitRootResult.Retry && stCommitRoot.Attempt+1 < stCommitRootMaxAttempts {
		task.ScheduleTaskInTx(
			tx,
			StCommitRoot,
			&StCommitRootArgs{DeploymentKey: stCommitRoot.DeploymentKey, Epoch: stCommitRoot.Epoch, Attempt: stCommitRoot.Attempt + 1},
			clientSession,
			task.RunOnce("st_commit_root", stCommitRoot.DeploymentKey, stCommitRoot.Epoch),
			task.RunAt(server.NowUtc().Add(stCommitRootRetryDelay)),
			task.MaxTime(30*time.Minute),
		)
	}
	return nil
}

// StDeposit(epoch)

type StDepositArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Epoch         uint64 `json:"epoch"`
	Attempt       int    `json:"attempt"`
}

type StDepositResult struct {
	Retry bool `json:"retry"`
}

func scheduleStDeposit(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, epoch uint64, attempt int, runAt time.Time) {
	task.ScheduleTask(
		StDeposit,
		&StDepositArgs{DeploymentKey: string(deploymentKey), Epoch: epoch, Attempt: attempt},
		clientSession,
		task.RunOnce("st_deposit", deploymentKey, epoch),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
	)
}

func StDeposit(
	stDeposit *StDepositArgs,
	clientSession *session.ClientSession,
) (*StDepositResult, error) {
	if _, current := stTaskDeploymentCurrent(stDeposit.DeploymentKey); !current {
		return &StDepositResult{}, nil
	}
	ctx := clientSession.Ctx

	outcome, err := controller.StDepositForEpoch(ctx, stDeposit.Epoch, nil)
	if err != nil {
		glog.Infof("[st]epoch %d deposit failed (attempt %d): %s\n", stDeposit.Epoch, stDeposit.Attempt, err)
		return &StDepositResult{Retry: true}, nil
	}
	if glog.V(1) {
		glog.Infof("[st]epoch %d deposit: %s\n", stDeposit.Epoch, outcome)
	}
	return &StDepositResult{Retry: outcome.Retry}, nil
}

func StDepositPost(
	stDeposit *StDepositArgs,
	stDepositResult *StDepositResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stDeposit.DeploymentKey); !current {
		return nil
	}
	if stDepositResult.Retry && stDeposit.Attempt+1 < stDepositMaxAttempts {
		task.ScheduleTaskInTx(
			tx,
			StDeposit,
			&StDepositArgs{DeploymentKey: stDeposit.DeploymentKey, Epoch: stDeposit.Epoch, Attempt: stDeposit.Attempt + 1},
			clientSession,
			task.RunOnce("st_deposit", stDeposit.DeploymentKey, stDeposit.Epoch),
			task.RunAt(server.NowUtc().Add(stDepositRetryDelay)),
			task.MaxTime(30*time.Minute),
		)
	}
	return nil
}

// StFinalizePoke(epoch)

type StFinalizePokeArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Epoch         uint64 `json:"epoch"`
	Attempt       int    `json:"attempt"`
}

type StFinalizePokeResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func scheduleStFinalizePoke(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, epoch uint64, attempt int, runAt time.Time) {
	task.ScheduleTask(
		StFinalizePoke,
		&StFinalizePokeArgs{DeploymentKey: string(deploymentKey), Epoch: epoch, Attempt: attempt},
		clientSession,
		task.RunOnce("st_finalize_poke", deploymentKey, epoch),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
	)
}

func StFinalizePoke(
	stFinalizePoke *StFinalizePokeArgs,
	clientSession *session.ClientSession,
) (*StFinalizePokeResult, error) {
	if _, current := stTaskDeploymentCurrent(stFinalizePoke.DeploymentKey); !current {
		return &StFinalizePokeResult{}, nil
	}
	ctx := clientSession.Ctx

	outcome, err := controller.StFinalizeEpochPoke(ctx, stFinalizePoke.Epoch)
	if err != nil {
		glog.Infof("[st]epoch %d finalize failed (attempt %d): %s\n", stFinalizePoke.Epoch, stFinalizePoke.Attempt, err)
		return &StFinalizePokeResult{Retry: true}, nil
	}
	if glog.V(1) {
		glog.Infof("[st]epoch %d finalize: %s\n", stFinalizePoke.Epoch, outcome)
	}
	result := &StFinalizePokeResult{Retry: outcome.Retry}
	if outcome.Retry && !outcome.RetryAt.IsZero() {
		retryAt := outcome.RetryAt
		result.RetryAt = &retryAt
	}
	return result, nil
}

func StFinalizePokePost(
	stFinalizePoke *StFinalizePokeArgs,
	stFinalizePokeResult *StFinalizePokeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stFinalizePoke.DeploymentKey); !current {
		return nil
	}
	if stFinalizePokeResult.Retry && stFinalizePoke.Attempt+1 < stFinalizePokeMaxAttempts {
		runAt := server.NowUtc().Add(stFinalizePokeRetryDelay)
		if stFinalizePokeResult.RetryAt != nil && runAt.Before(*stFinalizePokeResult.RetryAt) {
			runAt = *stFinalizePokeResult.RetryAt
		}
		task.ScheduleTaskInTx(
			tx,
			StFinalizePoke,
			&StFinalizePokeArgs{DeploymentKey: stFinalizePoke.DeploymentKey, Epoch: stFinalizePoke.Epoch, Attempt: stFinalizePoke.Attempt + 1},
			clientSession,
			task.RunOnce("st_finalize_poke", stFinalizePoke.DeploymentKey, stFinalizePoke.Epoch),
			task.RunAt(runAt),
			task.MaxTime(30*time.Minute),
		)
	}
	return nil
}
