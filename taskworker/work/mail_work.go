package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §15.15's retention chain (MAIL-M7-21). Each run retires the hours of
// mail_connection_record past mx-2's 24 h, the family's own task rather than
// the registry's (RetireByOwnTask), then deletes host keys 30 days after their
// retirement (mx-3), a held network's staying (§21.9). An hour a hold keeps is
// a warning on every run until its copy lands, never a lost row, and holds
// back no other hour. mail_network and mail_direct_mx have no ttl of their
// own to sweep: they go when mail is turned off, on DELETE, or with their
// network or receiver. A binary that does not link MG7 neither schedules the
// chain nor, should an older row run, touches anything.

// Cadences and budgets of the chain.
type MailRetentionSettings struct {
	// The chain runs again this long after a run that finished its work, and
	// RetryInterval after one that left work behind.
	RunInterval   time.Duration
	RetryInterval time.Duration
	// How long one run's retirements may take before the rest waits for the
	// next run. The host key sweep keeps its own budget in the model.
	RetireBudget time.Duration
	// Each run's task max time, past both budgets.
	MaxTime time.Duration
}

// Every 5 minutes, so an hour goes within 5 minutes of its 24 h. A run that
// left work behind continues a minute later.
func DefaultMailRetentionSettings() *MailRetentionSettings {
	return &MailRetentionSettings{
		RunInterval:   5 * time.Minute,
		RetryInterval: 1 * time.Minute,
		RetireBudget:  2 * time.Minute,
		MaxTime:       10 * time.Minute,
	}
}

type MailRetentionArgs struct {
}

type MailRetentionResult struct {
	// the leaves dropped
	Dropped []string `json:"dropped,omitempty"`
	// the expired hours that stay, by leaf, with why
	Kept                map[string]string `json:"kept,omitempty"`
	RemovedHostKeyCount int64             `json:"removed_host_key_count"`
	More                bool              `json:"more"`
}

// Schedules the chain's first run. A binary that does not link MG7 schedules
// nothing.
func ScheduleMailRetention(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleMailRetention(clientSession, tx, server.NowUtc().Add(DefaultMailRetentionSettings().RunInterval))
}

func scheduleMailRetention(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !server.SidecarMg7Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		MailRetention,
		&MailRetentionArgs{},
		clientSession,
		task.RunOnce("mail_retention"),
		task.RunAt(runAt),
		task.MaxTime(DefaultMailRetentionSettings().MaxTime),
	)
}

// One run: the hours past their retention, then the host keys past 30 days.
func MailRetention(
	_ *MailRetentionArgs,
	clientSession *session.ClientSession,
) (*MailRetentionResult, error) {
	if !server.SidecarMg7Linked() {
		return &MailRetentionResult{}, nil
	}
	settings := DefaultMailRetentionSettings()
	now := server.NowUtc()
	report := model.RetireMailConnectionHours(clientSession.Ctx, now, time.Now().Add(settings.RetireBudget))
	if 0 < len(report.Dropped) {
		glog.Infof("[mailretention]dropped %v\n", report.Dropped)
	}
	for leaf, reason := range report.Blocked {
		// a hold whose copy has not landed, or a failed drop: each run says so
		// until it goes (§21.9)
		glog.Warningf("[mailretention]expired %s stays: %s\n", leaf, reason)
	}
	removedHostKeyCount, moreHostKeys := model.RemoveRetiredMailHostKeys(clientSession.Ctx, now)
	if 0 < removedHostKeyCount {
		glog.Infof("[mailretention]removed %d host keys 30 days after their retirement\n", removedHostKeyCount)
	}
	return &MailRetentionResult{
		Dropped:             report.Dropped,
		Kept:                report.Blocked,
		RemovedHostKeyCount: removedHostKeyCount,
		More:                report.More || moreHostKeys,
	}, nil
}

// Schedules the next run, sooner when this one left work behind.
func MailRetentionPost(
	_ *MailRetentionArgs,
	result *MailRetentionResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultMailRetentionSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if result.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	scheduleMailRetention(clientSession, tx, runAt)
	return nil
}
