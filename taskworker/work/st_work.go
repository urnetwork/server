package work

// st_work.go — the subtensor (st) epoch pipeline (sn/PLAN.md §6), modeled
// on the Schedule/Task/TaskPost trio pattern of account_payment_work.go
// with per-epoch RunOnce keys:
//
//   - StSyncChain (periodic ~5s): pokes rollEpochs when the lazy counter
//     is behind, mirrors the contract epoch state into `st_epoch` + the
//     redis summary cache, advances the event mirror in bounded ranges,
//     and schedules the per-epoch tasks below. Task RunAt values are only
//     derived from contract block deadlines (~12s/block estimate) — the
//     contract clock stays authoritative; every deadline decision is
//     re-checked in blocks inside the controller flows.
//   - StEpochClose(e): compute + store the payout leaves/root; mark closed.
//     Past its commit window an epoch closes without leaves and finalizes
//     as a missed root.
//   - StCommitRoot(e): idempotent commitOperator within the +commitWindow
//     deadline; bounded retries via task reschedule; T-2h alert (D-11).
//   - StDeposit(e): the demand deposit task. It pays the epoch's deposit as
//     early as it can and keeps trying until the deposit lands or the epoch
//     ends: the previous epoch's signed usage (bytes and users) at the signed
//     price sheet's rates for this epoch and the conviction-snapshot tier,
//     capped per epoch (D-3); convert-then-push-then-credit, both publish kinds
//     recorded. Under a zero-price schedule nothing is owed: the task records a
//     skipped publish, sends no transaction and raises no alert.
//   - StFinalizePoke(e): permissionless finalizeEpoch at/after the
//     finalize block; marks the epoch finalized.
//
// Task errors are folded into results (the Payout precedent) so the Post
// hooks control every reschedule. All tasks no-op when `st.yml` is absent
// or disabled (controller.StEnabled).

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const (
	// Poll below the block cadence so discovery and queueing do not consume
	// the five-block emission close window after its finalized boundary.
	stSyncChainInterval = 5 * time.Second

	// bounded retry budgets per epoch task chain. Commit retries are
	// additionally cut off by the on-chain commit window; finalize by the
	// sync task re-seeding the chain when it lapses.
	stEpochCloseMaxAttempts   = 100
	stCommitRootMaxAttempts   = 100
	stFinalizePokeMaxAttempts = 100

	stEpochCloseRetryDelay = 5 * time.Second
	stCommitRootRetryDelay = 5 * time.Minute
	// A refusal, such as a USDC wallet too small for the deposit, retries after
	// this unless its outcome names a time. A deposit retries until it lands or its
	// epoch ends, so it has no attempt budget.
	stDepositRetryDelay = 15 * time.Minute
	// The least delay between a deposit's attempts, whatever the outcome names.
	stDepositMinRetryDelay   = 30 * time.Second
	stFinalizePokeRetryDelay = 5 * time.Minute

	// One attempt can block on the settle before it sizes (8 minutes for the send in
	// flight), wait out a conversion in Ensure (its 12-minute budget, then the unwrap and
	// the stake of the step it ends on, 8 each), then staging and the credit (8 each):
	// 52 minutes. Past this the task's context stops the attempt, whose every send is
	// durable, and the next one goes on from there.
	stDepositMaxTime = 60 * time.Minute

	// The close reads every contract closed in the epoch in one snapshot. On
	// Main the 24-hour epoch 0 closed 9.2M contracts; the 7-day epoch 3 closed
	// ~400M, and its census statement, reading them in close-time index order
	// (a random heap page per row), ran past this bound twice on 2026-10-30.
	// A window past the census record bound now reads its usage alone in one
	// block-ordered pass (model/st_provider_usage.go). Three hours stays inside
	// the 4-hour root commit window and leaves an hour to commit.
	stEpochCloseMaxTime = 3 * time.Hour
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
		if stEpoch.Epoch < state.Epoch && stV1Epoch(ctx, stEpoch.Epoch) {
			scheduleStEpochClose(clientSession, deploymentKey, stEpoch.Epoch, 0, server.NowUtc())
		}
	}

	// closed rows -> commit within the window; finalize poke at the
	// finalize block either way (a missed commit still finalizes — the
	// pool total carries, D-11)
	for _, stEpoch := range model.GetStEpochsWithStatus(ctx, deploymentKey, model.StEpochStatusClosed) {
		if state.HeadBlock <= stEpoch.CommitDeadlineBlock && stV1Epoch(ctx, stEpoch.Epoch) {
			scheduleStCommitRoot(clientSession, deploymentKey, stEpoch.Epoch, 0, server.NowUtc())
		}
		scheduleStFinalizePoke(clientSession, deploymentKey, stEpoch.Epoch, 0, stBlockRunAt(state, stEpoch.FinalizeBlock))
	}
	for _, stEpoch := range model.GetStEpochsWithStatus(ctx, deploymentKey, model.StEpochStatusCommitted) {
		scheduleStFinalizePoke(clientSession, deploymentKey, stEpoch.Epoch, 0, stBlockRunAt(state, stEpoch.FinalizeBlock))
	}

	// one automated deposit chain per (current) epoch, until it concludes
	if !stDepositConcluded(ctx, deploymentKey, state.Epoch) {
		scheduleStDeposit(clientSession, deploymentKey, state.Epoch)
	}
	// a price published after the epoch's zero-price skip (st_deposit_late_entry_work.go)
	scheduleStDepositLateEntry(clientSession, deploymentKey, state.Epoch)

	// Before the epoch's close and census: the users count is exact only at the
	// boundary. It cannot stop the steps below.
	scheduleStLedgerV2UsageSteps(clientSession, deploymentKey, state)
	scheduleStLedgerV2Steps(clientSession, deploymentKey, state)
	// Capture the flag's own chains, off unless st.yml turns them on.
	scheduleStCtfDriverSteps(clientSession, deploymentKey, state)
	scheduleStV1Sweep(clientSession, deploymentKey)

	return &StSyncChainResult{}, nil
}

func StSyncChainPost(
	stSyncChain *StSyncChainArgs,
	stSyncChainResult *StSyncChainResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(stSyncChain.DeploymentKey); !current {
		return nil
	}
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

// stDepositConcludes reports whether a deposit publish row ends the epoch's chain:
// the deposit landed, was found already applied or owes nothing, or can never succeed
// this epoch. A failed row does not: the chain keeps trying until the epoch ends.
func stDepositConcludes(publish *model.StPublish) bool {
	if publish.Kind != model.StPublishKindDeposit {
		return false
	}
	switch publish.Status {
	case model.StPublishStatusConfirmed, model.StPublishStatusSkipped, model.StPublishStatusRefused:
		return true
	}
	return false
}

// stDepositConcluded reports whether the epoch's deposit chain has concluded, and so
// needs no chain seeded (D-3).
func stDepositConcluded(ctx context.Context, deploymentKey model.StDeploymentKey, epoch uint64) bool {
	for _, publish := range model.GetStPublishes(ctx, deploymentKey, epoch) {
		if stDepositConcludes(publish) {
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
		task.MaxTime(stEpochCloseMaxTime),
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
	if errors.Is(err, controller.ErrStCommitWindowClosed) {
		// Closed without leaves, the sync finalizes it like any missed commit
		// so the funded pool total carries (D-11). Retrying cannot commit.
		model.SetStEpochStatus(ctx, deploymentKey, stEpochClose.Epoch, model.StEpochStatusClosed)
		glog.Errorf("[st]epoch %d closed without a payout: %s\n", stEpochClose.Epoch, err)
		return &StEpochCloseResult{}, nil
	}
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
	allowed, err := controller.StV1TaskContinuationAllowedInTx(clientSession.Ctx, tx, stEpochClose.Epoch)
	if err != nil || !allowed {
		return err
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
				task.MaxTime(stEpochCloseMaxTime),
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
	allowed, err := controller.StV1TaskContinuationAllowedInTx(clientSession.Ctx, tx, stCommitRoot.Epoch)
	if err != nil || !allowed {
		return err
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

// StDeposit(epoch), the demand deposit task

type StDepositArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Epoch         uint64 `json:"epoch"`
	Attempt       int    `json:"attempt"`
}

type StDepositResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func stDepositRunOnce(deploymentKey string, epoch uint64) *task.RunOnceOption {
	return task.RunOnce("st_deposit", deploymentKey, epoch)
}

// scheduleStDeposit starts the epoch's chain if none is pending. A pending chain
// keeps its own schedule: a RunOnce schedule would pull its retry forward to now,
// every sync turn, and a refusal would loop back to back for the whole epoch. It does
// take this binary's run time, which a chain an earlier binary left pending lacks.
func scheduleStDeposit(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, epoch uint64) {
	runOnce := stDepositRunOnce(string(deploymentKey), epoch)
	task.ScheduleTaskIfAbsent(
		StDeposit,
		&StDepositArgs{DeploymentKey: string(deploymentKey), Epoch: epoch},
		clientSession,
		runOnce,
		task.RunAt(server.NowUtc()),
		task.MaxTime(stDepositMaxTime),
	)
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		task.RaiseRunOnceMaxTimeInTx(clientSession.Ctx, tx, runOnce, stDepositMaxTime)
	})
}

// StDeposit runs one attempt of the demand deposit for its epoch. The chain ends when
// the deposit is confirmed, nothing is owed, it can never succeed this epoch, or the
// epoch has passed.
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
		glog.Errorf("[st]epoch %d deposit failed (attempt %d): %s\n", stDeposit.Epoch, stDeposit.Attempt, err)
		// An error can come before the attempt reaches its epoch check, and nothing
		// else would end the chain of a passed epoch.
		passed, passedErr := controller.StDepositEpochPassed(ctx, stDeposit.Epoch)
		return &StDepositResult{Retry: passedErr != nil || !passed}, nil
	}
	if outcome.Status == model.StPublishStatusRefused {
		glog.Errorf("[st]epoch %d deposit refused for the epoch (attempt %d): %s\n", stDeposit.Epoch, stDeposit.Attempt, outcome)
	} else if outcome.Retry || outcome.Status == model.StPublishStatusFailed {
		glog.Infof("[st]epoch %d deposit (attempt %d): %s\n", stDeposit.Epoch, stDeposit.Attempt, outcome)
	} else if glog.V(1) {
		glog.Infof("[st]epoch %d deposit: %s\n", stDeposit.Epoch, outcome)
	}
	result := &StDepositResult{Retry: outcome.Retry}
	if outcome.Retry && !outcome.RetryAt.IsZero() {
		retryAt := outcome.RetryAt
		result.RetryAt = &retryAt
	}
	return result, nil
}

// stDepositNextRun is when a deposit retries: when its outcome named, or after the
// refusal delay, and never sooner than the least delay.
func stDepositNextRun(now time.Time, retryAt *time.Time) time.Time {
	runAt := now.Add(stDepositRetryDelay)
	if retryAt != nil {
		runAt = *retryAt
	}
	if earliest := now.Add(stDepositMinRetryDelay); runAt.Before(earliest) {
		return earliest
	}
	return runAt
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
	if stDepositResult.Retry {
		task.ScheduleTaskInTx(
			tx,
			StDeposit,
			&StDepositArgs{DeploymentKey: stDeposit.DeploymentKey, Epoch: stDeposit.Epoch, Attempt: stDeposit.Attempt + 1},
			clientSession,
			stDepositRunOnce(stDeposit.DeploymentKey, stDeposit.Epoch),
			task.RunAt(stDepositNextRun(server.NowUtc(), stDepositResult.RetryAt)),
			task.MaxTime(stDepositMaxTime),
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
