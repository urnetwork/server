package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// smtpedge's taskworker jobs (§14.14; SMTP-M9-17), each a fleet-wide run-once chain that taskworker seeds at its
// start and each run schedules again:
//
//	smtp_retention   smtp_send's runway and its days at 30 days once §21.9's holds have copied them, then
//	                 smtp_complaint, smtp_dmarc_daily and smtp_halt at 13 months (model.RetireSmtpSendDays,
//	                 model.RemoveExpiredSmtpRows); a held day or row stays and is said each run
//	smtp_reconcile   §14.8's RUA check (smtpedgeservice.SmtpReconcile) from the cursor its args carry, a day
//	                 that exceeds reaching §21 as a case with no halt (D-SMTP-23)
//
// work/smtp_work.go holds the other two, the sealed copies' clearing and key expiry. A run never fails its task:
// each step's error is logged and the next run starts that step again, so one failed step holds back neither the
// others nor the chain. A binary that does not link a chain's groups neither schedules it nor, should an older row
// run, touches anything.

// Cadences and budgets of the two chains.
type SmtpTaskSettings struct {
	// Retention runs again this long after a run that finished its work, and RetryInterval after one that left
	// work behind: hourly, since a day drops at most an hour late and the 13-month sweeps are coarse.
	RetentionRunInterval time.Duration
	// The RUA check runs again this long after a run that finished its work: a day settles once a day, so 6 h
	// judges each within a quarter of one.
	ReconcileRunInterval time.Duration
	// Either chain's next run after one that left work behind.
	RetryInterval time.Duration
	// How long one retention run's partition drops may take before the rest waits for the next run. The row
	// sweeps keep their own budget (model.SmtpRetentionSettings).
	RetireBudget time.Duration
	// Each run's task max time, past every budget.
	MaxTime   time.Duration
	Retention *model.SmtpRetentionSettings
	Reconcile *smtpedgeservice.SmtpReconcileSettings
}

func DefaultSmtpTaskSettings() *SmtpTaskSettings {
	return &SmtpTaskSettings{
		RetentionRunInterval: 1 * time.Hour,
		ReconcileRunInterval: 6 * time.Hour,
		RetryInterval:        1 * time.Minute,
		RetireBudget:         2 * time.Minute,
		MaxTime:              10 * time.Minute,
		Retention:            model.DefaultSmtpRetentionSettings(),
		Reconcile:            smtpedgeservice.DefaultSmtpReconcileSettings(),
	}
}

// Whether the chains run in this binary: smtp_send, smtp_halt and smtp_dkim_key are MG9's, the report tables and
// §21's holds and cases MG5's.
func smtpTasksLinked() bool {
	return server.SidecarMg5Linked() && server.SidecarMg9Linked()
}

type SmtpRetentionArgs struct {
}

type SmtpRetentionResult struct {
	// the runway's new leaves
	Created []string `json:"created,omitempty"`
	// the days dropped
	Dropped []string `json:"dropped,omitempty"`
	// the expired days that stay, by leaf, with why
	Kept          map[string]string `json:"kept,omitempty"`
	RemovedCounts map[string]int64  `json:"removed_counts,omitempty"`
	More          bool              `json:"more"`
}

// Schedules the retention chain's first run. A binary that does not link MG5 and MG9 schedules nothing.
func ScheduleSmtpRetention(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSmtpRetention(clientSession, tx, server.NowUtc().Add(DefaultSmtpTaskSettings().RetryInterval))
}

func scheduleSmtpRetention(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !smtpTasksLinked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		SmtpRetention,
		&SmtpRetentionArgs{},
		clientSession,
		task.RunOnce("smtp_retention"),
		task.RunAt(runAt),
		task.MaxTime(DefaultSmtpTaskSettings().MaxTime),
	)
}

// One retention run: smtp_send's runway and its expired days, then the 13-month rows.
func SmtpRetention(_ *SmtpRetentionArgs, clientSession *session.ClientSession) (*SmtpRetentionResult, error) {
	if !smtpTasksLinked() {
		return &SmtpRetentionResult{}, nil
	}
	return runSmtpRetention(clientSession.Ctx, server.NowUtc(), DefaultSmtpTaskSettings()), nil
}

// The run at now, which a test moves.
func runSmtpRetention(ctx context.Context, now time.Time, settings *SmtpTaskSettings) *SmtpRetentionResult {
	result := &SmtpRetentionResult{Kept: map[string]string{}}
	step := func(name string, callback func()) {
		if r := server.HandleError(callback); r != nil {
			// the step starts again at the next run, and the others go on
			result.More = true
			glog.Warningf("[smtpretention]%s failed, to run again: %v\n", name, r)
		}
	}
	step("the runway", func() {
		runway := model.EnsureSmtpSendRunway(ctx, now)
		result.Created = runway.Created
		for _, runwayErr := range runway.Errors {
			glog.Warningf("[smtpretention]runway: %s\n", runwayErr)
		}
	})
	step("the days", func() {
		report := model.RetireSmtpSendDays(ctx, now, time.Now().Add(settings.RetireBudget))
		result.Dropped = report.Dropped
		for leaf, reason := range report.Blocked {
			// a hold whose copy has not landed, or a failed drop: each run says so until it goes (§21.9)
			result.Kept[leaf] = reason
			glog.Warningf("[smtpretention]expired %s stays: %s\n", leaf, reason)
		}
		result.More = result.More || report.More
		if 0 < len(report.Dropped) {
			glog.Infof("[smtpretention]dropped %v\n", report.Dropped)
		}
	})
	step("the rows", func() {
		rows := model.RemoveExpiredSmtpRows(ctx, now, settings.Retention)
		result.RemovedCounts = rows.RemovedCounts
		result.More = result.More || rows.More
	})
	return result
}

// Schedules the next run, sooner when this one left work behind.
func SmtpRetentionPost(
	_ *SmtpRetentionArgs,
	result *SmtpRetentionResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultSmtpTaskSettings()
	runAt := server.NowUtc().Add(settings.RetentionRunInterval)
	if result != nil && result.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	scheduleSmtpRetention(clientSession, tx, runAt)
	return nil
}

// Where the RUA check resumes. A zero Day is a chain with no cursor yet, which starts at the newest settled day.
type SmtpReconcileArgs struct {
	Day            time.Time `json:"day,omitempty"`
	AfterNetworkId server.Id `json:"after_network_id"`
}

type SmtpReconcileResult struct {
	Next          SmtpReconcileArgs `json:"next"`
	JudgedCount   int               `json:"judged_count"`
	ExceededCount int               `json:"exceeded_count"`
	More          bool              `json:"more"`
	// a read or a case that failed, which the next run resumes at
	Failed bool `json:"failed,omitempty"`
}

// Schedules the RUA chain's first run. A binary that does not link MG5 and MG9 schedules nothing. A pending run
// keeps its own cursor (task.RunOnce keeps a pending row's args).
func ScheduleSmtpReconcile(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSmtpReconcile(clientSession, tx, &SmtpReconcileArgs{}, server.NowUtc().Add(DefaultSmtpTaskSettings().RetryInterval))
}

func scheduleSmtpReconcile(clientSession *session.ClientSession, tx server.PgTx, args *SmtpReconcileArgs, runAt time.Time) {
	if !smtpTasksLinked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		SmtpReconcileTask,
		args,
		clientSession,
		task.RunOnce("smtp_reconcile"),
		task.RunAt(runAt),
		task.MaxTime(DefaultSmtpTaskSettings().MaxTime),
	)
}

// One RUA pass from args' cursor, on model's store and §21's case opening.
func SmtpReconcileTask(args *SmtpReconcileArgs, clientSession *session.ClientSession) (*SmtpReconcileResult, error) {
	if !smtpTasksLinked() {
		return &SmtpReconcileResult{}, nil
	}
	return runSmtpReconcile(clientSession.Ctx, args, server.NowUtc(), model.NewSmtpEdgeStore(nil), NewSmtpReconcileCaseSink(nil), DefaultSmtpTaskSettings()), nil
}

// The pass at now on store and sink, which a test replaces.
func runSmtpReconcile(
	ctx context.Context,
	args *SmtpReconcileArgs,
	now time.Time,
	store smtpedgeservice.SmtpReconcileStore,
	sink smtpedgeservice.SmtpReconcileSink,
	settings *SmtpTaskSettings,
) *SmtpReconcileResult {
	if args == nil {
		args = &SmtpReconcileArgs{}
	}
	reconcile, err := smtpedgeservice.NewSmtpReconcile(store, sink, settings.Reconcile)
	if err != nil {
		// the defaults always validate; a refused setting keeps the cursor
		glog.Errorf("[smtpreconcile]%v\n", err)
		return &SmtpReconcileResult{Next: *args, Failed: true}
	}
	cursor := smtpedgeservice.SmtpReconcileCursor{Day: args.Day, AfterNetworkId: sidecar.Id(args.AfterNetworkId)}
	if args.Day.IsZero() {
		cursor = reconcile.StartCursor(now)
	}
	pass := reconcile.Pass(ctx, cursor, now)
	result := &SmtpReconcileResult{
		Next: SmtpReconcileArgs{
			Day:            pass.Next.Day,
			AfterNetworkId: server.Id(pass.Next.AfterNetworkId),
		},
		JudgedCount:   pass.JudgedCount,
		ExceededCount: pass.ExceededCount,
		More:          pass.More,
	}
	if pass.Err != nil {
		result.Failed = true
		glog.Warningf("[smtpreconcile]a pass stopped, to resume at its cursor: %v\n", pass.Err)
	}
	if 0 < pass.ExceededCount {
		glog.Infof("[smtpreconcile]%d network days for §21's review\n", pass.ExceededCount)
	}
	return result
}

// Schedules the next pass at the cursor this one reached, sooner when it left work behind or failed.
func SmtpReconcileTaskPost(
	args *SmtpReconcileArgs,
	result *SmtpReconcileResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultSmtpTaskSettings()
	next := args
	runAt := server.NowUtc().Add(settings.ReconcileRunInterval)
	if result != nil {
		next = &result.Next
		if result.More || result.Failed {
			runAt = server.NowUtc().Add(settings.RetryInterval)
		}
	}
	scheduleSmtpReconcile(clientSession, tx, next, runAt)
	return nil
}

// §21's half of the RUA check: a day that exceeds opens, or adds to, the network's spam case on §21.8's review
// clock of a low join, which reviews and never acts by machine (§14.8's "no halt"; D-SMTP-23), named by the
// network itself. The case keeps §21's fields alone: the day's counts stay in smtp_dmarc_daily and smtp_send for
// the reviewer, and no count, domain or address reaches the case or a log. ABUSE-M9-01 may take this over in its
// mail controller; until then it is this.
type smtpReconcileCaseSink struct {
	now      func() time.Time
	settings func() *model.AbuseSettings
}

var _ smtpedgeservice.SmtpReconcileSink = (*smtpReconcileCaseSink)(nil)

// The sink on §21's case opening. A nil now reads the process's clock.
func NewSmtpReconcileCaseSink(now func() time.Time) smtpedgeservice.SmtpReconcileSink {
	if now == nil {
		now = server.NowUtc
	}
	return &smtpReconcileCaseSink{
		now: now,
		settings: func() *model.AbuseSettings {
			settings := model.DefaultAbuseSettings()
			model.ApplyAbuseConfig(settings, server.GetAbuseConfig())
			return settings
		},
	}
}

// See smtpedgeservice.SmtpReconcileSink.
func (self *smtpReconcileCaseSink) OpenSmtpReconcileCase(ctx context.Context, day *smtpedgeservice.SmtpReconcileDay) (returnErr error) {
	if day == nil || day.NetworkId == (sidecar.Id{}) {
		return fmt.Errorf("%w: no network", model.ErrAbuseReportInvalid)
	}
	networkId := server.Id(day.NetworkId)
	if r := server.HandleError(func() {
		_, returnErr = model.OpenAbuseReportCase(
			ctx,
			&model.AbuseReport{
				Category:      model.AbuseCategorySpam,
				ReporterClass: model.AbuseReporterClassMailReport,
			},
			&model.AbuseReportJoin{
				NetworkId: networkId,
				JoinKind:  model.AbuseJoinKindNetwork,
				JoinKey:   networkId.String(),
				Target:    model.AbuseTarget{Kind: model.AbuseTargetKindNetwork, Name: networkId.String()},
			},
			model.AbuseSignalMailJoinLow,
			self.now(),
			self.settings(),
		)
	}); r != nil {
		// a raised read or write: the pass stops and resumes at this network
		return fmt.Errorf("abuse case: %v", r)
	}
	return returnErr
}
