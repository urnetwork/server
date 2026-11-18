package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Two of smtpedge's four taskworker jobs (§14.14; SMTP-M9-17), each a fleet-wide run-once chain seeded at
// taskworker's start; controller/smtp_task_controller.go holds retention and the RUA check:
//
//	smtp_sealed_reports   smtp_complaint's sealed copies cleared 30 days after their report arrived, the rows
//	                      kept (owner decision 78; model.ClearExpiredSmtpSealedReports)
//	smtp_dkim_expiry      a retired selector taken out of DNS 7 days after its retirement and its row deleted 30
//	                      days after that (§14.6, §14.14; model.ExpireSmtpDkimKeys)
//
// A held network keeps both until its hold ends (§21.9). A run never fails its task: a failed step is logged and
// the next run, a minute later, starts it again. A binary that does not link MG5 and MG9 neither schedules the
// chains nor, should an older row run, touches anything.

// Cadences of the two chains.
type SmtpCleanupSettings struct {
	// Each runs again this long after a run that finished its work: hourly, so a copy clears and a selector
	// leaves DNS at most an hour late.
	RunInterval time.Duration
	// And this long after one that left work behind.
	RetryInterval time.Duration
	// Each run's task max time, past the sweeps' budgets.
	MaxTime   time.Duration
	Retention *model.SmtpRetentionSettings
}

func DefaultSmtpCleanupSettings() *SmtpCleanupSettings {
	return &SmtpCleanupSettings{
		RunInterval:   1 * time.Hour,
		RetryInterval: 1 * time.Minute,
		MaxTime:       10 * time.Minute,
		Retention:     model.DefaultSmtpRetentionSettings(),
	}
}

func smtpCleanupLinked() bool {
	return server.SidecarMg5Linked() && server.SidecarMg9Linked()
}

type SmtpSealedReportsArgs struct {
}

type SmtpSealedReportsResult struct {
	ClearedCount int64 `json:"cleared_count"`
	More         bool  `json:"more"`
}

// Schedules the sealed copies' chain. A binary that does not link MG5 and MG9 schedules nothing.
func ScheduleSmtpSealedReports(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSmtpSealedReports(clientSession, tx, server.NowUtc().Add(DefaultSmtpCleanupSettings().RetryInterval))
}

func scheduleSmtpSealedReports(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !smtpCleanupLinked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		SmtpSealedReports,
		&SmtpSealedReportsArgs{},
		clientSession,
		task.RunOnce("smtp_sealed_reports"),
		task.RunAt(runAt),
		task.MaxTime(DefaultSmtpCleanupSettings().MaxTime),
	)
}

// One run: the sealed copies past their 30 days cleared.
func SmtpSealedReports(_ *SmtpSealedReportsArgs, clientSession *session.ClientSession) (*SmtpSealedReportsResult, error) {
	result := &SmtpSealedReportsResult{}
	if !smtpCleanupLinked() {
		return result, nil
	}
	settings := DefaultSmtpCleanupSettings()
	if r := server.HandleError(func() {
		result.ClearedCount, result.More = model.ClearExpiredSmtpSealedReports(clientSession.Ctx, server.NowUtc(), settings.Retention)
	}); r != nil {
		result.More = true
		glog.Warningf("[smtpsealed]a run failed, to run again: %v\n", r)
	}
	if 0 < result.ClearedCount {
		glog.Infof("[smtpsealed]cleared %d sealed copies 30 days after their reports\n", result.ClearedCount)
	}
	return result, nil
}

func SmtpSealedReportsPost(
	_ *SmtpSealedReportsArgs,
	result *SmtpSealedReportsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultSmtpCleanupSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if result != nil && result.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	scheduleSmtpSealedReports(clientSession, tx, runAt)
	return nil
}

type SmtpDkimExpiryArgs struct {
}

type SmtpDkimExpiryResult struct {
	RemovedFromDnsCount int   `json:"removed_from_dns_count"`
	DeletedCount        int64 `json:"deleted_count"`
	FailedCount         int   `json:"failed_count"`
	More                bool  `json:"more"`
}

// Schedules the key expiry chain. A binary that does not link MG5 and MG9 schedules nothing.
func ScheduleSmtpDkimExpiry(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSmtpDkimExpiry(clientSession, tx, server.NowUtc().Add(DefaultSmtpCleanupSettings().RetryInterval))
}

func scheduleSmtpDkimExpiry(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !smtpCleanupLinked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		SmtpDkimExpiry,
		&SmtpDkimExpiryArgs{},
		clientSession,
		task.RunOnce("smtp_dkim_expiry"),
		task.RunAt(runAt),
		task.MaxTime(DefaultSmtpCleanupSettings().MaxTime),
	)
}

// One run: retired selectors past their 7 days out of DNS, then rows past their 30 days out of it deleted.
func SmtpDkimExpiry(_ *SmtpDkimExpiryArgs, clientSession *session.ClientSession) (*SmtpDkimExpiryResult, error) {
	result := &SmtpDkimExpiryResult{}
	if !smtpCleanupLinked() {
		return result, nil
	}
	settings := DefaultSmtpCleanupSettings()
	if r := server.HandleError(func() {
		expiry := model.ExpireSmtpDkimKeys(clientSession.Ctx, server.NowUtc(), settings.Retention)
		result.RemovedFromDnsCount = expiry.RemovedFromDnsCount
		result.DeletedCount = expiry.DeletedCount
		result.FailedCount = expiry.FailedCount
		// a selector whose write fails waits for the hourly run, never a minute's loop
		result.More = expiry.More
	}); r != nil {
		result.More = true
		glog.Warningf("[smtpdkimexpiry]a run failed, to run again: %v\n", r)
	}
	if 0 < result.RemovedFromDnsCount || 0 < result.DeletedCount {
		glog.Infof("[smtpdkimexpiry]%d retired selectors left dns, %d rows deleted\n", result.RemovedFromDnsCount, result.DeletedCount)
	}
	return result, nil
}

func SmtpDkimExpiryPost(
	_ *SmtpDkimExpiryArgs,
	result *SmtpDkimExpiryResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultSmtpCleanupSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if result != nil && result.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	scheduleSmtpDkimExpiry(clientSession, tx, runAt)
	return nil
}
