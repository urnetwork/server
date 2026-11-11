package work

// §20.14's usage jobs (QUOTA-M2-12), each a once-keyed chain that reschedules itself, bounded and resumable
// by its cursor (CODESTYLE's bound work examined), production-only as the other quota chains are:
//
//	RollupServiceUsage          after each UTC day closes: each network's stored bytes for that day
//	                            (model.RollupServiceUsage)
//	ReconcileSidecarCounters    nightly: storage_over, the connected sets and the spent marks
//	                            (model.ReconcileSidecarCounters)
//	RemoveExpiredServiceUsage   hourly: network_service_usage past 13 months and service_usage_batch past
//	                            24 h, a page a run, at once again while a page came back full
//
// A run that stops short hands its cursor to the next, which runs at once. A binary that does not link MG2
// neither schedules the chains nor, should an older row run, touches anything.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type QuotaUsageWorkSettings struct {
	// How long after 00:00 UTC a closed day rolls up: 10 min, past the day's last batches' flush.
	RollupDelay time.Duration
	// When in each UTC day the reconcile starts: 02:00, after the rollup and away from the day's turn.
	ReconcileOffset time.Duration
	// How often the retention pass runs once it finds nothing past its ttl: 1 h.
	RetentionInterval time.Duration
	// Networks a rollup or reconcile run takes, and rows a retention run deletes from each table.
	RollupPageCount    int
	ReconcilePageCount int
	RetentionPageCount int
	// Each run's task max time.
	MaxTime time.Duration
}

func DefaultQuotaUsageWorkSettings() *QuotaUsageWorkSettings {
	return &QuotaUsageWorkSettings{
		RollupDelay:        10 * time.Minute,
		ReconcileOffset:    2 * time.Hour,
		RetentionInterval:  time.Hour,
		RollupPageCount:    5000,
		ReconcilePageCount: 500,
		RetentionPageCount: 10000,
		MaxTime:            5 * time.Minute,
	}
}

// The next instant at offset past a UTC midnight, after now.
func nextUtcDayOffset(now time.Time, offset time.Duration) time.Time {
	now = now.UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(offset)
	if !at.After(now) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

type RollupServiceUsageArgs struct {
	// The closed UTC day the run rolls up.
	UsageDay time.Time `json:"usage_day"`
	// Where the run starts; nil for the first network.
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

type RollupServiceUsageResult struct {
	model.RollupServiceUsageResult
}

// Schedules the rollup of the day closing next. A binary that does not link MG2 schedules nothing.
func ScheduleRollupServiceUsage(clientSession *session.ClientSession, tx server.PgTx) {
	settings := DefaultQuotaUsageWorkSettings()
	runAt := nextUtcDayOffset(server.NowUtc(), settings.RollupDelay)
	scheduleRollupServiceUsage(clientSession, tx, &RollupServiceUsageArgs{UsageDay: runAt.AddDate(0, 0, -1).Truncate(24 * time.Hour)}, runAt)
}

func scheduleRollupServiceUsage(clientSession *session.ClientSession, tx server.PgTx, args *RollupServiceUsageArgs, runAt time.Time) {
	if !server.SidecarMg2Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		RollupServiceUsage,
		args,
		clientSession,
		task.RunOnce("quota_rollup_service_usage"),
		task.RunAt(runAt),
		task.MaxTime(DefaultQuotaUsageWorkSettings().MaxTime),
	)
}

func RollupServiceUsage(args *RollupServiceUsageArgs, clientSession *session.ClientSession) (*RollupServiceUsageResult, error) {
	if !server.SidecarMg2Linked() {
		return &RollupServiceUsageResult{}, nil
	}
	result, err := model.RollupServiceUsage(clientSession.Ctx, args.UsageDay, args.AfterNetworkId, DefaultQuotaUsageWorkSettings().RollupPageCount)
	if err != nil {
		return nil, err
	}
	if glog.V(1) {
		glog.Infof("[quota]usage rollup wrote %d of %d networks' days\n", result.WrittenCount, result.LookedCount)
	}
	return &RollupServiceUsageResult{RollupServiceUsageResult: result}, nil
}

// The same day again at once from the run's cursor when it stopped short, else the next day's rollup.
func RollupServiceUsagePost(args *RollupServiceUsageArgs, result *RollupServiceUsageResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if result.LastNetworkId != nil {
		scheduleRollupServiceUsage(clientSession, tx, &RollupServiceUsageArgs{UsageDay: args.UsageDay, AfterNetworkId: result.LastNetworkId}, server.NowUtc())
		return nil
	}
	ScheduleRollupServiceUsage(clientSession, tx)
	return nil
}

type ReconcileSidecarCountersArgs struct {
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

type ReconcileSidecarCountersResult struct {
	model.ReconcileSidecarCountersResult
}

// Schedules the next night's reconcile. A binary that does not link MG2 schedules nothing.
func ScheduleReconcileSidecarCounters(clientSession *session.ClientSession, tx server.PgTx) {
	settings := DefaultQuotaUsageWorkSettings()
	scheduleReconcileSidecarCounters(clientSession, tx, &ReconcileSidecarCountersArgs{}, nextUtcDayOffset(server.NowUtc(), settings.ReconcileOffset))
}

func scheduleReconcileSidecarCounters(clientSession *session.ClientSession, tx server.PgTx, args *ReconcileSidecarCountersArgs, runAt time.Time) {
	if !server.SidecarMg2Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		ReconcileSidecarCounters,
		args,
		clientSession,
		task.RunOnce("quota_reconcile_sidecar_counters"),
		task.RunAt(runAt),
		task.MaxTime(DefaultQuotaUsageWorkSettings().MaxTime),
	)
}

func ReconcileSidecarCounters(args *ReconcileSidecarCountersArgs, clientSession *session.ClientSession) (*ReconcileSidecarCountersResult, error) {
	if !server.SidecarMg2Linked() {
		return &ReconcileSidecarCountersResult{}, nil
	}
	result, err := model.ReconcileSidecarCounters(clientSession.Ctx, server.NowUtc(), args.AfterNetworkId, DefaultQuotaUsageWorkSettings().ReconcilePageCount)
	if err != nil {
		return nil, err
	}
	return &ReconcileSidecarCountersResult{ReconcileSidecarCountersResult: result}, nil
}

func ReconcileSidecarCountersPost(args *ReconcileSidecarCountersArgs, result *ReconcileSidecarCountersResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if result.LastNetworkId != nil {
		scheduleReconcileSidecarCounters(clientSession, tx, &ReconcileSidecarCountersArgs{AfterNetworkId: result.LastNetworkId}, server.NowUtc())
		return nil
	}
	ScheduleReconcileSidecarCounters(clientSession, tx)
	return nil
}

type RemoveExpiredServiceUsageArgs struct{}

type RemoveExpiredServiceUsageResult struct {
	model.RemoveExpiredServiceUsageResult
}

// Schedules the retention pass a RetentionInterval from now. A binary that does not link MG2 schedules nothing.
func ScheduleRemoveExpiredServiceUsage(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleRemoveExpiredServiceUsage(clientSession, tx, server.NowUtc().Add(DefaultQuotaUsageWorkSettings().RetentionInterval))
}

func scheduleRemoveExpiredServiceUsage(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !server.SidecarMg2Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredServiceUsage,
		&RemoveExpiredServiceUsageArgs{},
		clientSession,
		task.RunOnce("quota_remove_expired_service_usage"),
		task.RunAt(runAt),
		task.MaxTime(DefaultQuotaUsageWorkSettings().MaxTime),
	)
}

func RemoveExpiredServiceUsage(args *RemoveExpiredServiceUsageArgs, clientSession *session.ClientSession) (*RemoveExpiredServiceUsageResult, error) {
	if !server.SidecarMg2Linked() {
		return &RemoveExpiredServiceUsageResult{}, nil
	}
	result, err := model.RemoveExpiredServiceUsage(clientSession.Ctx, server.NowUtc(), DefaultQuotaUsageWorkSettings().RetentionPageCount)
	if err != nil {
		return nil, err
	}
	return &RemoveExpiredServiceUsageResult{RemoveExpiredServiceUsageResult: result}, nil
}

// At once again while a page came back full, else a RetentionInterval from now.
func RemoveExpiredServiceUsagePost(args *RemoveExpiredServiceUsageArgs, result *RemoveExpiredServiceUsageResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if result.More {
		scheduleRemoveExpiredServiceUsage(clientSession, tx, server.NowUtc())
		return nil
	}
	ScheduleRemoveExpiredServiceUsage(clientSession, tx)
	return nil
}
