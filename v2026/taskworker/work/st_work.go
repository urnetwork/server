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

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
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
}

type StSyncChainResult struct {
}

func ScheduleStSyncChain(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		StSyncChain,
		&StSyncChainArgs{},
		clientSession,
		task.RunOnce("st_sync_chain"),
		task.RunAt(server.NowUtc().Add(stSyncChainInterval)),
		task.MaxTime(15*time.Minute),
	)
}

func StSyncChain(
	stSyncChain *StSyncChainArgs,
	clientSession *session.ClientSession,
) (*StSyncChainResult, error) {
	if !controller.StEnabled() {
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
	for _, stEpoch := range model.GetStEpochsWithStatus(ctx, model.StEpochStatusOpen) {
		if stEpoch.Epoch < state.Epoch {
			scheduleStEpochClose(clientSession, stEpoch.Epoch, 0, server.NowUtc())
		}
	}

	// closed rows -> commit within the window; finalize poke at the
	// finalize block either way (a missed commit still finalizes — the
	// pool total carries, D-11)
	for _, stEpoch := range model.GetStEpochsWithStatus(ctx, model.StEpochStatusClosed) {
		if state.HeadBlock <= stEpoch.CommitDeadlineBlock {
			scheduleStCommitRoot(clientSession, stEpoch.Epoch, 0, server.NowUtc())
		}
		scheduleStFinalizePoke(clientSession, stEpoch.Epoch, 0, stBlockRunAt(state, stEpoch.FinalizeBlock))
	}
	for _, stEpoch := range model.GetStEpochsWithStatus(ctx, model.StEpochStatusCommitted) {
		scheduleStFinalizePoke(clientSession, stEpoch.Epoch, 0, stBlockRunAt(state, stEpoch.FinalizeBlock))
	}

	// one automated deposit chain per (current) epoch
	if !stHasPublishKind(ctx, state.Epoch, model.StPublishKindDeposit) {
		scheduleStDeposit(clientSession, state.Epoch, 0, server.NowUtc())
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
func stHasPublishKind(ctx context.Context, epoch uint64, kind string) bool {
	for _, publish := range model.GetStPublishes(ctx, epoch) {
		if publish.Kind == kind {
			return true
		}
	}
	return false
}

// StEpochClose(epoch)

type StEpochCloseArgs struct {
	Epoch   uint64 `json:"epoch"`
	Attempt int    `json:"attempt"`
}

type StEpochCloseResult struct {
	Retry bool `json:"retry"`
}

func scheduleStEpochClose(clientSession *session.ClientSession, epoch uint64, attempt int, runAt time.Time) {
	task.ScheduleTask(
		StEpochClose,
		&StEpochCloseArgs{Epoch: epoch, Attempt: attempt},
		clientSession,
		task.RunOnce("st_epoch_close", epoch),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

func StEpochClose(
	stEpochClose *StEpochCloseArgs,
	clientSession *session.ClientSession,
) (*StEpochCloseResult, error) {
	if !controller.StEnabled() {
		return &StEpochCloseResult{}, nil
	}
	ctx := clientSession.Ctx

	root, leafCount, err := controller.StComputeEpochPayout(ctx, stEpochClose.Epoch)
	if err != nil {
		glog.Infof("[st]epoch %d close failed (attempt %d): %s\n", stEpochClose.Epoch, stEpochClose.Attempt, err)
		return &StEpochCloseResult{Retry: true}, nil
	}
	model.SetStEpochStatus(ctx, stEpochClose.Epoch, model.StEpochStatusClosed)
	glog.Infof("[st]epoch %d closed: %d leaves root 0x%x\n", stEpochClose.Epoch, leafCount, root)
	return &StEpochCloseResult{}, nil
}

func StEpochClosePost(
	stEpochClose *StEpochCloseArgs,
	stEpochCloseResult *StEpochCloseResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if stEpochCloseResult.Retry {
		if stEpochClose.Attempt+1 < stEpochCloseMaxAttempts {
			task.ScheduleTaskInTx(
				tx,
				StEpochClose,
				&StEpochCloseArgs{Epoch: stEpochClose.Epoch, Attempt: stEpochClose.Attempt + 1},
				clientSession,
				task.RunOnce("st_epoch_close", stEpochClose.Epoch),
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
		&StCommitRootArgs{Epoch: stEpochClose.Epoch},
		clientSession,
		task.RunOnce("st_commit_root", stEpochClose.Epoch),
		task.MaxTime(30*time.Minute),
	)
	return nil
}

// StCommitRoot(epoch)

type StCommitRootArgs struct {
	Epoch   uint64 `json:"epoch"`
	Attempt int    `json:"attempt"`
}

type StCommitRootResult struct {
	Retry bool `json:"retry"`
}

func scheduleStCommitRoot(clientSession *session.ClientSession, epoch uint64, attempt int, runAt time.Time) {
	task.ScheduleTask(
		StCommitRoot,
		&StCommitRootArgs{Epoch: epoch, Attempt: attempt},
		clientSession,
		task.RunOnce("st_commit_root", epoch),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
	)
}

func StCommitRoot(
	stCommitRoot *StCommitRootArgs,
	clientSession *session.ClientSession,
) (*StCommitRootResult, error) {
	if !controller.StEnabled() {
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
	if stCommitRootResult.Retry && stCommitRoot.Attempt+1 < stCommitRootMaxAttempts {
		task.ScheduleTaskInTx(
			tx,
			StCommitRoot,
			&StCommitRootArgs{Epoch: stCommitRoot.Epoch, Attempt: stCommitRoot.Attempt + 1},
			clientSession,
			task.RunOnce("st_commit_root", stCommitRoot.Epoch),
			task.RunAt(server.NowUtc().Add(stCommitRootRetryDelay)),
			task.MaxTime(30*time.Minute),
		)
	}
	return nil
}

// StDeposit(epoch)

type StDepositArgs struct {
	Epoch   uint64 `json:"epoch"`
	Attempt int    `json:"attempt"`
}

type StDepositResult struct {
	Retry bool `json:"retry"`
}

func scheduleStDeposit(clientSession *session.ClientSession, epoch uint64, attempt int, runAt time.Time) {
	task.ScheduleTask(
		StDeposit,
		&StDepositArgs{Epoch: epoch, Attempt: attempt},
		clientSession,
		task.RunOnce("st_deposit", epoch),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
	)
}

func StDeposit(
	stDeposit *StDepositArgs,
	clientSession *session.ClientSession,
) (*StDepositResult, error) {
	if !controller.StEnabled() {
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
	if stDepositResult.Retry && stDeposit.Attempt+1 < stDepositMaxAttempts {
		task.ScheduleTaskInTx(
			tx,
			StDeposit,
			&StDepositArgs{Epoch: stDeposit.Epoch, Attempt: stDeposit.Attempt + 1},
			clientSession,
			task.RunOnce("st_deposit", stDeposit.Epoch),
			task.RunAt(server.NowUtc().Add(stDepositRetryDelay)),
			task.MaxTime(30*time.Minute),
		)
	}
	return nil
}

// StFinalizePoke(epoch)

type StFinalizePokeArgs struct {
	Epoch   uint64 `json:"epoch"`
	Attempt int    `json:"attempt"`
}

type StFinalizePokeResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func scheduleStFinalizePoke(clientSession *session.ClientSession, epoch uint64, attempt int, runAt time.Time) {
	task.ScheduleTask(
		StFinalizePoke,
		&StFinalizePokeArgs{Epoch: epoch, Attempt: attempt},
		clientSession,
		task.RunOnce("st_finalize_poke", epoch),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
	)
}

func StFinalizePoke(
	stFinalizePoke *StFinalizePokeArgs,
	clientSession *session.ClientSession,
) (*StFinalizePokeResult, error) {
	if !controller.StEnabled() {
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
	if stFinalizePokeResult.Retry && stFinalizePoke.Attempt+1 < stFinalizePokeMaxAttempts {
		runAt := server.NowUtc().Add(stFinalizePokeRetryDelay)
		if stFinalizePokeResult.RetryAt != nil && runAt.Before(*stFinalizePokeResult.RetryAt) {
			runAt = *stFinalizePokeResult.RetryAt
		}
		task.ScheduleTaskInTx(
			tx,
			StFinalizePoke,
			&StFinalizePokeArgs{Epoch: stFinalizePoke.Epoch, Attempt: stFinalizePoke.Attempt + 1},
			clientSession,
			task.RunOnce("st_finalize_poke", stFinalizePoke.Epoch),
			task.RunAt(runAt),
			task.MaxTime(30*time.Minute),
		)
	}
	return nil
}
