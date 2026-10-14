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

// A chain row that a newer config wrote stays that config's while its stamp
// is this recent. The rollup and retention chains run for up to an hour and
// repeat a minute apart, so their instances restamp them at least that often;
// twice that covers a claim handed back in between and a restart.
const verifyNewerConfigTimeout = 2 * (time.Hour + time.Minute)

// The run-once keys of the verification chains.
func verifyTaskRunOnceKeys() []string {
	return []string{
		task.RunOnce("sweep_verify_trails").String(),
		task.RunOnce("rollup_verify_provider_stats").String(),
		task.RunOnce("remove_old_verify_provider_stats").String(),
		task.RunOnce("refresh_verify_proxy_egress").String(),
	}
}

// Whether a chain row was written by a newer config within its timeout. st.yml
// lives in the vault, which has no version of its own, so the config version
// orders taskworkers instead: it keeps a straggler that rejoins on an older
// config, as one did on 2026-10-13, from ending or removing the chains of the
// instances on the newer config, while taskworkers on the same config disable
// the chains at once, as before (config_generation.go).
func verifyChainNewerLive(stamp ConfigStamp, now time.Time) bool {
	return stamp.newerWithin(now, verifyNewerConfigTimeout)
}

// RemoveDisabledVerifyTasks removes an old generation's pending chains after
// the subnet is disabled. Merely skipping seeding is insufficient because a
// RunOnce row survives deploys and its Post hook would otherwise perpetuate
// the chain. Enabling later seeds fresh rows through InitTasks. While any
// chain row carries a newer config's live stamp, every row stays: an older
// config does not disable the chains that config runs.
func RemoveDisabledVerifyTasks(ctx context.Context, tx server.PgTx) int64 {
	if controller.StEnabled() {
		return 0
	}
	now := server.NowUtc()
	for _, stamp := range readConfigStampsInTx(ctx, tx, verifyTaskRunOnceKeys()) {
		if verifyChainNewerLive(stamp, now) {
			return 0
		}
	}
	var removed int64
	for _, functionName := range VerifyTaskFunctionNames() {
		removed += task.RemovePendingTasksForFunctionInTx(ctx, tx, functionName)
	}
	return removed
}

type SweepVerifyTrailsArgs struct {
	// The config version that wrote the chain's row, and when.
	ConfigStamp
}

type SweepVerifyTrailsResult struct {
	// st.yml disables verification here, but a newer config's live chain was
	// claimed: nothing ran, and the post-step reschedules it unchanged
	HandedBack bool `json:"handed_back,omitempty"`
}

func ScheduleSweepVerifyTrails(clientSession *session.ClientSession, tx server.PgTx) {
	if !controller.StEnabled() {
		return
	}
	scheduleSweepVerifyTrailsAt(clientSession, tx, &SweepVerifyTrailsArgs{ConfigStamp: newConfigStamp(server.NowUtc())},
		server.NowUtc().Add(15*time.Second))
}

func scheduleSweepVerifyTrailsAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	sweepVerifyTrails *SweepVerifyTrailsArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		SweepVerifyTrails,
		sweepVerifyTrails,
		clientSession,
		task.RunOnce("sweep_verify_trails"),
		task.RunAt(runAt),
		task.MaxTime(15*time.Minute),
	)
}

func SweepVerifyTrails(
	sweepVerifyTrails *SweepVerifyTrailsArgs,
	clientSession *session.ClientSession,
) (*SweepVerifyTrailsResult, error) {
	if !controller.StEnabled() {
		return &SweepVerifyTrailsResult{
			HandedBack: sweepVerifyTrails != nil && verifyChainNewerLive(sweepVerifyTrails.ConfigStamp, server.NowUtc()),
		}, nil
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
	if sweepVerifyTrailsResult.HandedBack {
		runAt := server.NowUtc().Add(configHandbackDelay)
		rescheduleHandedBackTaskInTx(clientSession, tx, task.RunOnce("sweep_verify_trails"), runAt, func() {
			scheduleSweepVerifyTrailsAt(clientSession, tx, sweepVerifyTrails, runAt)
		})
		return nil
	}
	ScheduleSweepVerifyTrails(clientSession, tx)
	return nil
}

type RollupVerifyProviderStatsArgs struct {
	// The config version that wrote the chain's row, and when.
	ConfigStamp
}

type RollupVerifyProviderStatsResult struct {
	// st.yml disables verification here, but a newer config's live chain was
	// claimed: nothing ran, and the post-step reschedules it unchanged
	HandedBack bool `json:"handed_back,omitempty"`
}

func ScheduleRollupVerifyProviderStats(clientSession *session.ClientSession, tx server.PgTx) {
	if !controller.StEnabled() {
		return
	}
	scheduleRollupVerifyProviderStatsAt(clientSession, tx, &RollupVerifyProviderStatsArgs{ConfigStamp: newConfigStamp(server.NowUtc())},
		server.NowUtc().Add(1*time.Minute))
}

func scheduleRollupVerifyProviderStatsAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	rollupVerifyProviderStats *RollupVerifyProviderStatsArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		RollupVerifyProviderStats,
		rollupVerifyProviderStats,
		clientSession,
		task.RunOnce("rollup_verify_provider_stats"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

func RollupVerifyProviderStats(
	rollupVerifyProviderStats *RollupVerifyProviderStatsArgs,
	clientSession *session.ClientSession,
) (*RollupVerifyProviderStatsResult, error) {
	if !controller.StEnabled() {
		return &RollupVerifyProviderStatsResult{
			HandedBack: rollupVerifyProviderStats != nil && verifyChainNewerLive(rollupVerifyProviderStats.ConfigStamp, server.NowUtc()),
		}, nil
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
	if rollupVerifyProviderStatsResult.HandedBack {
		runAt := server.NowUtc().Add(configHandbackDelay)
		rescheduleHandedBackTaskInTx(clientSession, tx, task.RunOnce("rollup_verify_provider_stats"), runAt, func() {
			scheduleRollupVerifyProviderStatsAt(clientSession, tx, rollupVerifyProviderStats, runAt)
		})
		return nil
	}
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
	// The config version that wrote the chain's row, and when.
	ConfigStamp
}

type RemoveOldVerifyProviderStatsResult struct {
	// st.yml disables verification here, but a newer config's live chain was
	// claimed: nothing ran, and the post-step reschedules it unchanged
	HandedBack bool `json:"handed_back,omitempty"`
}

func ScheduleRemoveOldVerifyProviderStats(clientSession *session.ClientSession, tx server.PgTx) {
	if !controller.StEnabled() {
		return
	}
	scheduleRemoveOldVerifyProviderStatsAt(clientSession, tx, &RemoveOldVerifyProviderStatsArgs{ConfigStamp: newConfigStamp(server.NowUtc())},
		server.NowUtc().Add(1*time.Minute))
}

func scheduleRemoveOldVerifyProviderStatsAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	removeOldVerifyProviderStats *RemoveOldVerifyProviderStatsArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldVerifyProviderStats,
		removeOldVerifyProviderStats,
		clientSession,
		task.RunOnce("remove_old_verify_provider_stats"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

func RemoveOldVerifyProviderStats(
	removeOldVerifyProviderStats *RemoveOldVerifyProviderStatsArgs,
	clientSession *session.ClientSession,
) (*RemoveOldVerifyProviderStatsResult, error) {
	if !controller.StEnabled() {
		return &RemoveOldVerifyProviderStatsResult{
			HandedBack: removeOldVerifyProviderStats != nil && verifyChainNewerLive(removeOldVerifyProviderStats.ConfigStamp, server.NowUtc()),
		}, nil
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
	if removeOldVerifyProviderStatsResult.HandedBack {
		runAt := server.NowUtc().Add(configHandbackDelay)
		rescheduleHandedBackTaskInTx(clientSession, tx, task.RunOnce("remove_old_verify_provider_stats"), runAt, func() {
			scheduleRemoveOldVerifyProviderStatsAt(clientSession, tx, removeOldVerifyProviderStats, runAt)
		})
		return nil
	}
	ScheduleRemoveOldVerifyProviderStats(clientSession, tx)
	return nil
}
