package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// tls-3's roll-up chain (TLS-M5-12). Each run retires the hours of
// tls_connection_record past their retention, the family's own task rather
// than the registry's (RetireByOwnTask), and each retirement adds its hour's
// counts to tls_connection_day in the transaction that drops it
// (model.RollUpTlsConnectionHours). Then the run deletes daily counts past
// their 13 months, a held network's staying (§21.9). An hour a hold keeps is a
// warning on every run until its copy lands, never a lost row, and holds back
// no other hour. A binary that does not link MG5 neither schedules the chain
// nor, should an older row run, touches anything.

// Cadences and budgets of the chain.
type TlsConnectionRollupSettings struct {
	// The chain runs again this long after a run that finished its work, and
	// RetryInterval after one that left work behind.
	RunInterval   time.Duration
	RetryInterval time.Duration
	// How long one run's retirements may take before the rest waits for the
	// next run. The day sweep keeps its own budget in the model.
	RetireBudget time.Duration
	// Each run's task max time, past both budgets.
	MaxTime time.Duration
}

// Every 5 minutes, so an hour goes within 5 minutes of its 24 h: the ranges
// are hours, and the registry's 15-minute chain would keep each a quarter hour
// longer than its ttl. A run that left work behind continues a minute later.
func DefaultTlsConnectionRollupSettings() *TlsConnectionRollupSettings {
	return &TlsConnectionRollupSettings{
		RunInterval:   5 * time.Minute,
		RetryInterval: 1 * time.Minute,
		RetireBudget:  2 * time.Minute,
		MaxTime:       10 * time.Minute,
	}
}

type TlsConnectionRollupArgs struct {
}

type TlsConnectionRollupResult struct {
	// the leaves dropped, their hours rolled up
	RolledUp []string `json:"rolled_up,omitempty"`
	// the expired hours that stay, by leaf, with why
	Kept            map[string]string `json:"kept,omitempty"`
	RemovedDayCount int64             `json:"removed_day_count"`
	More            bool              `json:"more"`
}

// Schedules the chain's first run. A binary that does not link MG5 schedules
// nothing.
func ScheduleTlsConnectionRollup(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleTlsConnectionRollup(clientSession, tx, server.NowUtc().Add(DefaultTlsConnectionRollupSettings().RunInterval))
}

func scheduleTlsConnectionRollup(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		TlsConnectionRollup,
		&TlsConnectionRollupArgs{},
		clientSession,
		task.RunOnce("tls_connection_rollup"),
		task.RunAt(runAt),
		task.MaxTime(DefaultTlsConnectionRollupSettings().MaxTime),
	)
}

// One run: the hours past their retention, then the days past 13 months.
func TlsConnectionRollup(
	_ *TlsConnectionRollupArgs,
	clientSession *session.ClientSession,
) (*TlsConnectionRollupResult, error) {
	if !server.SidecarMg5Linked() {
		return &TlsConnectionRollupResult{}, nil
	}
	settings := DefaultTlsConnectionRollupSettings()
	now := server.NowUtc()
	report := model.RollUpTlsConnectionHours(clientSession.Ctx, now, time.Now().Add(settings.RetireBudget))
	if 0 < len(report.Dropped) {
		glog.Infof("[tlsconnection]rolled up and dropped %v\n", report.Dropped)
	}
	for leaf, reason := range report.Blocked {
		// a hold whose copy has not landed, or a failed drop: each run says so
		// until it goes (§21.9)
		glog.Warningf("[tlsconnection]expired %s stays: %s\n", leaf, reason)
	}
	removedDayCount, moreDays := model.RemoveExpiredTlsConnectionDays(clientSession.Ctx, now)
	if 0 < removedDayCount {
		glog.Infof("[tlsconnection]removed %d daily counts past their 13 months\n", removedDayCount)
	}
	return &TlsConnectionRollupResult{
		RolledUp:        report.Dropped,
		Kept:            report.Blocked,
		RemovedDayCount: removedDayCount,
		More:            report.More || moreDays,
	}, nil
}

// Schedules the next run, sooner when this one left work behind.
func TlsConnectionRollupPost(
	_ *TlsConnectionRollupArgs,
	result *TlsConnectionRollupResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultTlsConnectionRollupSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if result.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	scheduleTlsConnectionRollup(clientSession, tx, runAt)
	return nil
}
