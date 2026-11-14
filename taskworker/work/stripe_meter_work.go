package work

// Team's meter jobs (QUOTA-M6-04; DESIGN.md 20.9's Meters), production-only as the other quota chains
// are, each one chain that reschedules itself:
//
//	ReportStripeMeters   every ReportInterval: sends the stripe_meter_report rows that are due, bounded
//	                     by the controller's MaxSendCount, and at once again while rows stay due past it
//	                     (controller.ReportStripeMeters). The rows carry their own attempts and next
//	                     time, so a run killed anywhere is resumed by the next
//	AuditStripeMeters    daily at AuditOffset past 00:00 UTC: sent rows against Stripe's summaries, a
//	                     page of Teams a run from its cursor (controller.AuditStripeMeters). It only
//	                     counts and logs
//
// Neither is scheduled, and each returns at once, in a binary that does not link MG6.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type StripeMeterWorkSettings struct {
	// How often the sender runs once nothing is left due: 10 min.
	ReportInterval time.Duration
	// How far past 00:00 UTC the audit runs: 6 h, after the day's rows are written and sent.
	AuditOffset time.Duration
	// Each run's task max time.
	ReportMaxTime time.Duration
	AuditMaxTime  time.Duration
}

func DefaultStripeMeterWorkSettings() *StripeMeterWorkSettings {
	return &StripeMeterWorkSettings{
		ReportInterval: 10 * time.Minute,
		AuditOffset:    6 * time.Hour,
		ReportMaxTime:  30 * time.Minute,
		AuditMaxTime:   30 * time.Minute,
	}
}

type ReportStripeMetersArgs struct{}

type ReportStripeMetersResult struct {
	controller.ReportStripeMetersResult
}

// Schedules the sender a ReportInterval from now. A binary that does not link MG6 schedules nothing.
func ScheduleReportStripeMeters(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleReportStripeMeters(clientSession, tx, server.NowUtc().Add(DefaultStripeMeterWorkSettings().ReportInterval))
}

func scheduleReportStripeMeters(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !server.SidecarMg6Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		ReportStripeMeters,
		&ReportStripeMetersArgs{},
		clientSession,
		task.RunOnce("stripe_meter_report"),
		task.RunAt(runAt),
		task.MaxTime(DefaultStripeMeterWorkSettings().ReportMaxTime),
	)
}

func ReportStripeMeters(args *ReportStripeMetersArgs, clientSession *session.ClientSession) (*ReportStripeMetersResult, error) {
	if !server.SidecarMg6Linked() {
		return &ReportStripeMetersResult{}, nil
	}
	result, err := controller.ReportStripeMeters(clientSession.Ctx)
	if err != nil {
		return nil, err
	}
	if glog.V(1) {
		glog.Infof(
			"[stripe][meter]report sent %d, uncertain %d, failed %d, missed %d\n",
			result.SentCount,
			result.UncertainCount,
			result.FailedCount,
			result.MissedCount,
		)
	}
	return &ReportStripeMetersResult{ReportStripeMetersResult: *result}, nil
}

// At once again while rows stay due past the run's bound, else a ReportInterval from now.
func ReportStripeMetersPost(args *ReportStripeMetersArgs, result *ReportStripeMetersResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if result.More {
		scheduleReportStripeMeters(clientSession, tx, server.NowUtc())
		return nil
	}
	ScheduleReportStripeMeters(clientSession, tx)
	return nil
}

type AuditStripeMetersArgs struct {
	// Where the run starts; nil for the first Team.
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

type AuditStripeMetersResult struct {
	controller.AuditStripeMetersResult
}

// Schedules the next day's audit. A binary that does not link MG6 schedules nothing.
func ScheduleAuditStripeMeters(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleAuditStripeMeters(clientSession, tx, &AuditStripeMetersArgs{}, nextUtcDayOffset(server.NowUtc(), DefaultStripeMeterWorkSettings().AuditOffset))
}

func scheduleAuditStripeMeters(clientSession *session.ClientSession, tx server.PgTx, args *AuditStripeMetersArgs, runAt time.Time) {
	if !server.SidecarMg6Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		AuditStripeMeters,
		args,
		clientSession,
		task.RunOnce("stripe_meter_audit"),
		task.RunAt(runAt),
		task.MaxTime(DefaultStripeMeterWorkSettings().AuditMaxTime),
	)
}

func AuditStripeMeters(args *AuditStripeMetersArgs, clientSession *session.ClientSession) (*AuditStripeMetersResult, error) {
	if !server.SidecarMg6Linked() {
		return &AuditStripeMetersResult{}, nil
	}
	result, err := controller.AuditStripeMeters(clientSession.Ctx, args.AfterNetworkId)
	if err != nil {
		return nil, err
	}
	if 0 < result.DifferCount+result.AbsentCount {
		glog.Errorf("[stripe][meter]audit: %d of %d days differ from Stripe's summaries\n", result.DifferCount+result.AbsentCount, result.DayCount)
	}
	return &AuditStripeMetersResult{AuditStripeMetersResult: *result}, nil
}

// The next page at once from the run's cursor when it stopped short, else the next day's audit.
func AuditStripeMetersPost(args *AuditStripeMetersArgs, result *AuditStripeMetersResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if result.LastNetworkId != nil {
		scheduleAuditStripeMeters(clientSession, tx, &AuditStripeMetersArgs{AfterNetworkId: result.LastNetworkId}, server.NowUtc())
		return nil
	}
	ScheduleAuditStripeMeters(clientSession, tx)
	return nil
}
