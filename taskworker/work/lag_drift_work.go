package work

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

// The drift sweep (lag.dev/DRIFTTEST.md E5, K3, R1). A drift run completes as
// its last report arrives, and as the widget or the customer reads it; every
// sweep_interval (15 s) this completes the rest: a run whose report wait is
// over, and one that never finished, after processing_after and its report
// wait (P6). It also deletes the client_ip hash of every token that can start
// no more runs (K3), deletes drift tests and their runs past the drift
// retention, 7 days (R1; their tracks, tickets and results follow the lag
// sweep's), and sets urnetwork_lag_drift_stuck_runs to the runs left open a
// minute past every deadline, which only a sweep that can't complete them
// leaves: a taskworker without the borders, say (G1).

// The drift tests one sweep moves along at most; a backlog is the next
// sweep's.
const lagDriftSweepBatchSize = 200

// The sweep's task limit.
const lagDriftSweepMaxTime = 5 * time.Minute

type SweepLagDriftArgs struct {
}

type SweepLagDriftResult struct {
	AdvancedTests         int `json:"advanced_tests"`
	RemovedClientIpHashes int `json:"removed_client_ip_hashes"`
	RemovedTests          int `json:"removed_tests"`
	RemovedRuns           int `json:"removed_runs"`
	StuckRuns             int `json:"stuck_runs"`
}

// Schedules the next sweep one sweep interval from now, once per chain.
func ScheduleSweepLagDrift(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SweepLagDrift,
		&SweepLagDriftArgs{},
		clientSession,
		task.RunOnce("sweep_lag_drift"),
		task.RunAt(server.NowUtc().Add(controller.GetLagSettings().Drift.SweepInterval)),
		task.MaxTime(lagDriftSweepMaxTime),
	)
}

// Moves the drift tests along as the clock reads now.
func SweepLagDrift(
	_ *SweepLagDriftArgs,
	clientSession *session.ClientSession,
) (*SweepLagDriftResult, error) {
	return sweepLagDrift(clientSession.Ctx, server.NowUtc(), controller.GetLagSettings()), nil
}

// The sweep as the clock reads `now`; a test moves the clock past a deadline.
func sweepLagDrift(ctx context.Context, now time.Time, settings *controller.LagSettings) *SweepLagDriftResult {
	drift := &settings.Drift
	result := &SweepLagDriftResult{}
	for _, driftTestId := range model.GetDueLagDriftTests(
		ctx,
		now.Add(-drift.ProcessingAfter),
		now.Add(-drift.ReportWait()),
		lagDriftSweepBatchSize,
	) {
		controller.AdvanceLagDriftTest(ctx, driftTestId, now, settings)
		result.AdvancedTests += 1
	}
	result.RemovedClientIpHashes = model.RemoveClosedLagDriftClientIpHashes(ctx, now, drift.MaxRuns)
	removed := model.RemoveExpiredLagDriftTests(ctx, now.Add(-drift.Retention))
	result.RemovedTests = removed.Tests
	result.RemovedRuns = removed.Runs
	result.StuckRuns = model.CountStuckLagDriftRuns(ctx, now.Add(-(drift.ProcessingAfter + drift.ReportWait() + time.Minute)))
	controller.SetLagDriftStuckRuns(result.StuckRuns)
	// the pass is over: an alert reads the stuck runs of the newest pass, and
	// sees a sweep that stopped
	controller.SetLagDriftSweepTimestamp(server.NowUtc())

	if 0 < result.RemovedTests || 0 < result.RemovedRuns || 0 < result.StuckRuns {
		glog.Infof(
			"[lag]drift sweep: moved %d tests along, deleted %d client_ip hashes, %d tests and %d runs; %d runs stuck\n",
			result.AdvancedTests,
			result.RemovedClientIpHashes,
			result.RemovedTests,
			result.RemovedRuns,
			result.StuckRuns,
		)
	}
	return result
}

// Re-arms the chain in the transaction that finishes this sweep.
func SweepLagDriftPost(
	_ *SweepLagDriftArgs,
	_ *SweepLagDriftResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSweepLagDrift(clientSession, tx)
	return nil
}
