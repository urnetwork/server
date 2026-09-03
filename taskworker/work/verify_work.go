package work

// verify_work.go — recurring tasks for the `/verify` routing-verification
// protocol (sn/VALIDATOR.md):
//   - SweepVerifyTrails: the trail reaper (§4.4/§6.1) — moves active trails
//     whose pending step deadline has passed to `expired`, persisting the
//     failure record; attribution goes to the pending hop by construction
//     (§7.2).
//   - RollupVerifyProviderStats: drains the redis per-provider counters and
//     latency histograms into `verify_provider_stats` (§7).
//   - RefreshVerifyProxyEgress: periodically re-feeds proxy-allocated
//     egresses into the bijection-gated egress index so entries stay live
//     while allocated and age out after release (§8.2).

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// VerifyTaskFunctionNames is the complete recurring-task surface owned by the
// verification subsystem.  It is derived from the same function values the
// worker registers, so disabled-subsystem cleanup cannot drift from the
// canonical version-normalized names stored in pending_task.
func VerifyTaskFunctionNames() []string {
	return []string{
		task.NewTaskTarget(SweepVerifyTrails).TargetFunctionName(),
		task.NewTaskTarget(RollupVerifyProviderStats).TargetFunctionName(),
		task.NewTaskTarget(RemoveOldVerifyProviderStats).TargetFunctionName(),
		task.NewTaskTarget(RefreshVerifyProxyEgress).TargetFunctionName(),
	}
}

// RemoveDisabledVerifyTasks removes an old generation's pending chains after
// the subnet is disabled. Merely skipping seeding is insufficient because a
// RunOnce row survives deploys and its Post hook would otherwise perpetuate
// the chain. Enabling later seeds fresh rows through InitTasks.
func RemoveDisabledVerifyTasks(ctx context.Context, tx server.PgTx) int64 {
	if controller.StEnabled() {
		return 0
	}
	var removed int64
	for _, functionName := range VerifyTaskFunctionNames() {
		removed += task.RemovePendingTasksForFunctionInTx(ctx, tx, functionName)
	}
	return removed
}

type SweepVerifyTrailsArgs struct {
}

type SweepVerifyTrailsResult struct {
}

func ScheduleSweepVerifyTrails(clientSession *session.ClientSession, tx server.PgTx) {
	if !controller.StEnabled() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		SweepVerifyTrails,
		&SweepVerifyTrailsArgs{},
		clientSession,
		task.RunOnce("sweep_verify_trails"),
		task.RunAt(server.NowUtc().Add(15*time.Second)),
		task.MaxTime(15*time.Minute),
	)
}

func SweepVerifyTrails(
	sweepVerifyTrails *SweepVerifyTrailsArgs,
	clientSession *session.ClientSession,
) (*SweepVerifyTrailsResult, error) {
	if !controller.StEnabled() {
		return &SweepVerifyTrailsResult{}, nil
	}
	model.SweepExpiredVerifyTrails(
		clientSession.Ctx,
		server.NowUtc(),
		controller.VerifySettings(),
	)
	return &SweepVerifyTrailsResult{}, nil
}

func SweepVerifyTrailsPost(
	sweepVerifyTrails *SweepVerifyTrailsArgs,
	sweepVerifyTrailsResult *SweepVerifyTrailsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSweepVerifyTrails(clientSession, tx)
	return nil
}

type RollupVerifyProviderStatsArgs struct {
}

type RollupVerifyProviderStatsResult struct {
}

func ScheduleRollupVerifyProviderStats(clientSession *session.ClientSession, tx server.PgTx) {
	if !controller.StEnabled() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		RollupVerifyProviderStats,
		&RollupVerifyProviderStatsArgs{},
		clientSession,
		task.RunOnce("rollup_verify_provider_stats"),
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(1*time.Hour),
	)
}

func RollupVerifyProviderStats(
	rollupVerifyProviderStats *RollupVerifyProviderStatsArgs,
	clientSession *session.ClientSession,
) (*RollupVerifyProviderStatsResult, error) {
	if !controller.StEnabled() {
		return &RollupVerifyProviderStatsResult{}, nil
	}
	model.RollupVerifyProviderStats(
		clientSession.Ctx,
		server.NowUtc(),
		controller.VerifySettings(),
	)
	return &RollupVerifyProviderStatsResult{}, nil
}

func RollupVerifyProviderStatsPost(
	rollupVerifyProviderStats *RollupVerifyProviderStatsArgs,
	rollupVerifyProviderStatsResult *RollupVerifyProviderStatsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRollupVerifyProviderStats(clientSession, tx)
	return nil
}

type RefreshVerifyProxyEgressArgs struct {
}

type RefreshVerifyProxyEgressResult struct {
}

func ScheduleRefreshVerifyProxyEgress(clientSession *session.ClientSession, tx server.PgTx) {
	if !controller.StEnabled() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		RefreshVerifyProxyEgress,
		&RefreshVerifyProxyEgressArgs{},
		clientSession,
		task.RunOnce("refresh_verify_proxy_egress"),
		task.RunAt(server.NowUtc().Add(controller.VerifySettings().EgressRefreshInterval)),
		task.MaxTime(15*time.Minute),
	)
}

func RefreshVerifyProxyEgress(
	refreshVerifyProxyEgress *RefreshVerifyProxyEgressArgs,
	clientSession *session.ClientSession,
) (*RefreshVerifyProxyEgressResult, error) {
	if !controller.StEnabled() {
		return &RefreshVerifyProxyEgressResult{}, nil
	}
	model.RefreshVerifyProxyEgress(
		clientSession.Ctx,
		controller.VerifySettings(),
	)
	return &RefreshVerifyProxyEgressResult{}, nil
}

func RefreshVerifyProxyEgressPost(
	refreshVerifyProxyEgress *RefreshVerifyProxyEgressArgs,
	refreshVerifyProxyEgressResult *RefreshVerifyProxyEgressResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshVerifyProxyEgress(clientSession, tx)
	return nil
}

type RemoveOldVerifyProviderStatsArgs struct {
}

type RemoveOldVerifyProviderStatsResult struct {
}

func ScheduleRemoveOldVerifyProviderStats(clientSession *session.ClientSession, tx server.PgTx) {
	if !controller.StEnabled() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		RemoveOldVerifyProviderStats,
		&RemoveOldVerifyProviderStatsArgs{},
		clientSession,
		task.RunOnce("remove_old_verify_provider_stats"),
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
		task.MaxTime(1*time.Hour),
	)
}

func RemoveOldVerifyProviderStats(
	removeOldVerifyProviderStats *RemoveOldVerifyProviderStatsArgs,
	clientSession *session.ClientSession,
) (*RemoveOldVerifyProviderStatsResult, error) {
	if !controller.StEnabled() {
		return &RemoveOldVerifyProviderStatsResult{}, nil
	}
	limit := 50000
	model.RemoveOldVerifyProviderStats(clientSession.Ctx, server.NowUtc(), limit)
	return &RemoveOldVerifyProviderStatsResult{}, nil
}

func RemoveOldVerifyProviderStatsPost(
	removeOldVerifyProviderStats *RemoveOldVerifyProviderStatsArgs,
	removeOldVerifyProviderStatsResult *RemoveOldVerifyProviderStatsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldVerifyProviderStats(clientSession, tx)
	return nil
}
