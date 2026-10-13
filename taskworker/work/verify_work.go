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

	"github.com/urnetwork/glog"

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

// A refresh run stops starting chunks after this, well inside its max time,
// and re-arms at once from the last client it fed.
const refreshVerifyProxyEgressBudget = time.Minute

// Only a stalled run reaches this.
const refreshVerifyProxyEgressMaxTime = 15 * time.Minute

// Spaces the chunks of a run. One chunk's commands reach every Redis node at
// once, so a sweep is spread out rather than sent back to back.
const refreshVerifyProxyEgressChunkPause = 50 * time.Millisecond

// A failed or timed-out refresh retries within this, from the position it
// started at.
const refreshVerifyProxyEgressErrorRetryCap = time.Minute

type RefreshVerifyProxyEgressArgs struct {
	// The last client the current sweep fed; nil starts a sweep at the first
	// allocation.
	After *server.Id `json:"after,omitempty"`
}

type RefreshVerifyProxyEgressResult struct {
	// Where the successor resumes; nil when the sweep fed the last allocation.
	Next     *server.Id `json:"next,omitempty"`
	FedCount int        `json:"fed_count"`
}

// Startup arms a sweep after the refresh interval. A pending run keeps its
// position and only moves earlier.
func ScheduleRefreshVerifyProxyEgress(clientSession *session.ClientSession, tx server.PgTx) {
	if !controller.StEnabled() {
		return
	}
	scheduleRefreshVerifyProxyEgress(
		clientSession,
		tx,
		&RefreshVerifyProxyEgressArgs{},
		server.NowUtc().Add(controller.VerifySettings().EgressRefreshInterval),
	)
}

func scheduleRefreshVerifyProxyEgress(
	clientSession *session.ClientSession,
	tx server.PgTx,
	refreshVerifyProxyEgress *RefreshVerifyProxyEgressArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		RefreshVerifyProxyEgress,
		refreshVerifyProxyEgress,
		clientSession,
		task.RunOnce("refresh_verify_proxy_egress"),
		task.RunAt(runAt),
		task.MaxTime(refreshVerifyProxyEgressMaxTime),
	)
}

// A context-local clock lets tests run the budget and the pacing on synthetic
// time.
type refreshVerifyProxyEgressClockKey struct{}

type refreshVerifyProxyEgressClock struct {
	now  func() time.Time
	wait func(time.Duration)
}

// One budgeted run of a sweep, from the position in its arguments. Every
// allocation has to be fed again inside the egress ttl, so a run never grows
// with the allocation count: it stops after its budget and its successor
// continues after the last client fed.
func RefreshVerifyProxyEgress(
	refreshVerifyProxyEgress *RefreshVerifyProxyEgressArgs,
	clientSession *session.ClientSession,
) (*RefreshVerifyProxyEgressResult, error) {
	if !controller.StEnabled() {
		return &RefreshVerifyProxyEgressResult{}, nil
	}
	settings := controller.VerifySettings()
	clock := &refreshVerifyProxyEgressClock{
		now: server.NowUtc,
		wait: func(duration time.Duration) {
			select {
			case <-clientSession.Ctx.Done():
				server.Raise(clientSession.Ctx.Err())
			case <-time.After(duration):
			}
		},
	}
	if testClock, ok := clientSession.Ctx.Value(refreshVerifyProxyEgressClockKey{}).(*refreshVerifyProxyEgressClock); ok {
		clock = testClock
	}
	startTime := clock.now()
	after := server.Id{}
	if refreshVerifyProxyEgress.After != nil {
		after = *refreshVerifyProxyEgress.After
	}
	result := &RefreshVerifyProxyEgressResult{}
	for {
		last, clientCount := model.RefreshVerifyProxyEgressAfter(clientSession.Ctx, after, settings)
		result.FedCount += clientCount
		if clientCount < model.VerifyProxyEgressRefreshChunkSize {
			break
		}
		after = last
		if refreshVerifyProxyEgressBudget <= clock.now().Sub(startTime) {
			result.Next = &after
			break
		}
		clock.wait(refreshVerifyProxyEgressChunkPause)
	}
	glog.Infof("[verify]proxy egress refresh fed=%d complete=%t\n", result.FedCount, result.Next == nil)
	return result, nil
}

// A run the budget stopped continues at once from its position; a complete
// sweep starts the next one at the first allocation after the refresh interval.
func RefreshVerifyProxyEgressPost(
	refreshVerifyProxyEgress *RefreshVerifyProxyEgressArgs,
	refreshVerifyProxyEgressResult *RefreshVerifyProxyEgressResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if refreshVerifyProxyEgressResult.Next == nil {
		ScheduleRefreshVerifyProxyEgress(clientSession, tx)
		return nil
	}
	if !controller.StEnabled() {
		return nil
	}
	scheduleRefreshVerifyProxyEgress(
		clientSession,
		tx,
		&RefreshVerifyProxyEgressArgs{After: refreshVerifyProxyEgressResult.Next},
		server.NowUtc(),
	)
	return nil
}

// The registered refresh target. A failed or timed-out run retries within the
// error retry cap. The generic backoff reaches an hour after a dozen
// consecutive failures, and every lease lapses one egress ttl after its feed.
func NewRefreshVerifyProxyEgressTaskTarget() task.Target {
	return task.WithErrorRetryCap(
		task.NewTaskTargetWithPost(RefreshVerifyProxyEgress, RefreshVerifyProxyEgressPost),
		refreshVerifyProxyEgressErrorRetryCap,
	)
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
