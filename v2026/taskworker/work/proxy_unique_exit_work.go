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

// bounds a stranded claim; a run walks at most SweepBatchSize assignments
const sweepProxyExitsMaxTime = 10 * time.Minute

type SweepProxyExitsArgs struct{}

type SweepProxyExitsResult struct {
	Sweep *model.ProxyExitSweepResult `json:"sweep"`
}

func ScheduleSweepProxyExits(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSweepProxyExits(
		clientSession,
		tx,
		server.NowUtc().Add(model.GetUniqueExitSettings().SweepInterval),
	)
}

func scheduleSweepProxyExits(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		SweepProxyExits,
		&SweepProxyExitsArgs{},
		clientSession,
		task.RunOnce("sweep_proxy_exits"),
		task.RunAt(runAt),
		task.MaxTime(sweepProxyExitsMaxTime),
	)
}

func SweepProxyExits(
	sweepProxyExits *SweepProxyExitsArgs,
	clientSession *session.ClientSession,
) (*SweepProxyExitsResult, error) {
	sweep := model.SweepProxyExits(clientSession.Ctx, server.NowUtc(), model.GetUniqueExitSettings())
	if 0 < sweep.ReplacedCount+sweep.HeldCount+sweep.ReleasedCount+sweep.MarkedFailingCount {
		glog.Infof(
			"[upe]sweep: live=%d failing+%d replaced=%d held=%d released=%d keys=%d cleared=%d deleted=%d\n",
			sweep.LiveCount,
			sweep.MarkedFailingCount,
			sweep.ReplacedCount,
			sweep.HeldCount,
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
	scheduleSweepProxyExits(
		clientSession,
		tx,
		server.NowUtc().Add(model.GetUniqueExitSettings().SweepInterval),
	)
	return nil
}
