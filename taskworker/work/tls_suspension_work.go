package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §13.9's sweep (TLS-M5-13): each run lifts the kills whose until has passed,
// row and key together, so a time-boxed kill ends without its case. A kill
// with no until waits for its case's lift. A binary that does not link MG5
// neither schedules the chain nor, should an older row run, touches anything.

// Cadences of the sweep.
type TlsSuspensionSweepSettings struct {
	// The sweep runs again this long after a run, and RetryInterval after one
	// that filled its batch or failed. A reader already takes a kill past its
	// until as lifted, so the sweep only tidies, and a minute is soon enough.
	RunInterval   time.Duration
	RetryInterval time.Duration
	// Each run's task max time.
	MaxTime time.Duration
}

func DefaultTlsSuspensionSweepSettings() *TlsSuspensionSweepSettings {
	return &TlsSuspensionSweepSettings{
		RunInterval:   1 * time.Minute,
		RetryInterval: 10 * time.Second,
		MaxTime:       5 * time.Minute,
	}
}

type TlsSuspensionSweepArgs struct {
}

type TlsSuspensionSweepResult struct {
	LiftedCount int  `json:"lifted_count"`
	More        bool `json:"more"`
}

// Schedules the sweep's first run. A binary that does not link MG5 schedules
// nothing.
func ScheduleTlsSuspensionSweep(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleTlsSuspensionSweep(clientSession, tx, server.NowUtc().Add(DefaultTlsSuspensionSweepSettings().RunInterval))
}

func scheduleTlsSuspensionSweep(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		TlsSuspensionSweep,
		&TlsSuspensionSweepArgs{},
		clientSession,
		task.RunOnce("tls_suspension_sweep"),
		task.RunAt(runAt),
		task.MaxTime(DefaultTlsSuspensionSweepSettings().MaxTime),
	)
}

// One run of the sweep.
func TlsSuspensionSweep(
	_ *TlsSuspensionSweepArgs,
	clientSession *session.ClientSession,
) (*TlsSuspensionSweepResult, error) {
	if !server.SidecarMg5Linked() {
		return &TlsSuspensionSweepResult{}, nil
	}
	settings := model.DefaultTlsSuspensionSettings()
	liftedCount, err := model.LiftExpiredTlsSuspensionsWithSettings(clientSession.Ctx, server.NowUtc(), settings)
	result := &TlsSuspensionSweepResult{
		LiftedCount: liftedCount,
		More:        err != nil || settings.SweepBatchCount <= liftedCount,
	}
	if err != nil {
		// the error may name a key, which names a network; the count does not
		glog.Warningf("[tls]suspension sweep lifted %d and failed\n", liftedCount)
	}
	return result, nil
}

// Schedules the next run, sooner when this one left work behind.
func TlsSuspensionSweepPost(
	_ *TlsSuspensionSweepArgs,
	result *TlsSuspensionSweepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultTlsSuspensionSweepSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if result.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	scheduleTlsSuspensionSweep(clientSession, tx, runAt)
	return nil
}
