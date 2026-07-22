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
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type SweepVerifyTrailsArgs struct {
}

type SweepVerifyTrailsResult struct {
}

func ScheduleSweepVerifyTrails(clientSession *session.ClientSession, tx server.PgTx) {
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
	model.SweepExpiredVerifyTrails(
		clientSession.Ctx,
		server.NowUtc(),
		model.DefaultVerifySettings(),
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
	model.RollupVerifyProviderStats(
		clientSession.Ctx,
		server.NowUtc(),
		model.DefaultVerifySettings(),
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
	task.ScheduleTaskInTx(
		tx,
		RefreshVerifyProxyEgress,
		&RefreshVerifyProxyEgressArgs{},
		clientSession,
		task.RunOnce("refresh_verify_proxy_egress"),
		task.RunAt(server.NowUtc().Add(model.DefaultVerifySettings().EgressRefreshInterval)),
		task.MaxTime(15*time.Minute),
	)
}

func RefreshVerifyProxyEgress(
	refreshVerifyProxyEgress *RefreshVerifyProxyEgressArgs,
	clientSession *session.ClientSession,
) (*RefreshVerifyProxyEgressResult, error) {
	model.RefreshVerifyProxyEgress(
		clientSession.Ctx,
		model.DefaultVerifySettings(),
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
