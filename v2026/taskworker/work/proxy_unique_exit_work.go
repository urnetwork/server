package work

import (
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The unique-exit sweeper (model.SweepProxyExits) runs every
// UniqueExitSettings.SweepInterval: it releases removed proxies' exits,
// replaces failed ones (a bad verdict or a key collision at once, a lost
// provider after the grace), retries held proxies, and sets the unique-exit
// gauges. Each run schedules the next, like the hosted proxy reap.
//
// A run is bounded by UniqueExitSettings.SweepMaxTime (10 minutes), and its
// walk stops by itself at SweepWalkTime, inside that bound, so the run always
// returns through its Post. The Post carries the walk's cursor into the next
// run's args, and the next run resumes after the last assignment walked:
// every live assignment is reached however long a pass takes. Until 11-15 a
// pass that outgrew the bound was cut every run, never reached its Post, and
// its rerun started again at the oldest assignment, so the newest were never
// swept (CPUOPT6 C #9). A run the bound still cuts is rerun with its own args,
// from where it started, not from the oldest.

type SweepProxyExitsArgs struct {
	// where the previous run stopped; nil starts at the oldest assignment
	Cursor *model.ProxyExitSweepCursor `json:"cursor,omitempty"`
}

type SweepProxyExitsResult struct {
	Sweep *model.ProxyExitSweepResult `json:"sweep"`
}

func ScheduleSweepProxyExits(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSweepProxyExits(
		clientSession,
		tx,
		server.NowUtc().Add(model.GetUniqueExitSettings().SweepInterval),
		nil,
	)
}

func scheduleSweepProxyExits(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time, cursor *model.ProxyExitSweepCursor) {
	task.ScheduleTaskInTx(
		tx,
		SweepProxyExits,
		&SweepProxyExitsArgs{Cursor: cursor},
		clientSession,
		task.RunOnce("sweep_proxy_exits"),
		task.RunAt(runAt),
		task.MaxTime(model.GetUniqueExitSettings().SweepMaxTime),
	)
}

func SweepProxyExits(
	sweepProxyExits *SweepProxyExitsArgs,
	clientSession *session.ClientSession,
) (*SweepProxyExitsResult, error) {
	sweep := model.SweepProxyExits(clientSession.Ctx, server.NowUtc(), model.GetUniqueExitSettings(), sweepProxyExits.Cursor)
	if 0 < sweep.ReplacedCount+sweep.HeldCount+sweep.ReleasedCount+sweep.MarkedFailingCount || sweep.Next != nil {
		glog.Infof(
			"[upe]sweep: live=%d walked=%d resumed=%t more=%t failing+%d replaced=%d held=%d waiting=%d released=%d keys=%d cleared=%d deleted=%d\n",
			sweep.LiveCount,
			sweep.WalkedCount,
			sweepProxyExits.Cursor != nil,
			sweep.Next != nil,
			sweep.MarkedFailingCount,
			sweep.ReplacedCount,
			sweep.HeldCount,
			sweep.HeldWaitingCount,
			sweep.ReleasedCount,
			sweep.KeyUpdatedCount,
			sweep.ClearedCount,
			sweep.DeletedCount,
		)
	}
	return &SweepProxyExitsResult{Sweep: sweep}, nil
}

func SweepProxyExitsPost(
	sweepProxyExits *SweepProxyExitsArgs,
	sweepProxyExitsResult *SweepProxyExitsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	var cursor *model.ProxyExitSweepCursor
	if sweepProxyExitsResult != nil && sweepProxyExitsResult.Sweep != nil {
		cursor = sweepProxyExitsResult.Sweep.Next
	}
	scheduleSweepProxyExits(
		clientSession,
		tx,
		server.NowUtc().Add(model.GetUniqueExitSettings().SweepInterval),
		cursor,
	)
	return nil
}
