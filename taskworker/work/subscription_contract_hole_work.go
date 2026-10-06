package work

// One durable task chain refreshes the expiring authorization projection. Pages
// persist their cursor; packet workers never run a source query to fill a miss.

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

var contractHoleRefreshCompleted = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_contract_hole_refresh_completed_seconds",
	Help: "Last complete contract-hole source pass; zero means no complete pass observed by this worker",
})

var contractHoleRefreshDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_contract_hole_refresh_duration_seconds",
	Help: "Elapsed time of the last complete contract-hole pass, including page scheduling delays",
})

func init() {
	prometheus.MustRegister(contractHoleRefreshCompleted, contractHoleRefreshDuration)
}

// PassStarted fixes cadence independently of page count and finalizer latency.
type RefreshContractHolesArgs struct {
	Cursor      *model.ContractHoleCursor `json:"cursor,omitempty"`
	PassStarted time.Time                 `json:"pass_started"`
}

// Nil cursor means the indexed source pass reached its end.
type RefreshContractHolesResult struct {
	Cursor *model.ContractHoleCursor `json:"cursor,omitempty"`
	Pairs  int                       `json:"pairs"`
}

// Starts the bootstrap immediately; existing run-once ownership prevents a new
// worker from creating a second refresher alongside an already running chain.
func ScheduleRefreshContractHoles(clientSession *session.ClientSession, tx server.PgTx) {
	now := server.NowUtc()
	scheduleRefreshContractHoles(clientSession, tx, &RefreshContractHolesArgs{PassStarted: now}, now)
}

// A finite task deadline keeps a broken dependency from retaining this owner.
func scheduleRefreshContractHoles(clientSession *session.ClientSession, tx server.PgTx, args *RefreshContractHolesArgs, runAt time.Time) {
	task.ScheduleTaskInTx(tx, RefreshContractHoles, args, clientSession,
		task.RunOnce("refresh_contract_holes"), task.RunAt(runAt),
		task.MaxTime(20*time.Second), task.Priority(task.TaskPriorityFastest))
}

// A page performs bounded indexed work and propagates failure for durable retry.
func RefreshContractHoles(args *RefreshContractHolesArgs, clientSession *session.ClientSession) (*RefreshContractHolesResult, error) {
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 15*time.Second)
	defer cancel()
	next, pairs, err := model.RefreshContractHolesPage(ctx, args.Cursor)
	if err != nil {
		return nil, task.WithRetryDelayAndArgs(err, time.Second, &RefreshContractHolesArgs{Cursor: next, PassStarted: args.PassStarted})
	}
	if next == nil {
		now := server.NowUtc()
		contractHoleRefreshCompleted.Set(float64(now.Unix()))
		contractHoleRefreshDuration.Set(max(0, now.Sub(args.PassStarted).Seconds()))
	}
	return &RefreshContractHolesResult{Cursor: next, Pairs: pairs}, nil
}

// Continuations run immediately; completed passes begin again at half the TTL.
// A slow pass restarts immediately and leaves expired projections fail closed.
func RefreshContractHolesPost(args *RefreshContractHolesArgs, result *RefreshContractHolesResult, clientSession *session.ClientSession, tx server.PgTx) error {
	now := server.NowUtc()
	next := &RefreshContractHolesArgs{Cursor: result.Cursor, PassStarted: args.PassStarted}
	runAt := now
	if result.Cursor == nil {
		runAt = maxContractHoleRefreshTime(now, args.PassStarted.Add(model.ContractHoleRefreshInterval))
		next.PassStarted = runAt
	}
	scheduleRefreshContractHoles(clientSession, tx, next, runAt)
	return nil
}

// Keep the cadence calculation pure for exact scheduling boundary controls.
func maxContractHoleRefreshTime(now, due time.Time) time.Time {
	if due.After(now) {
		return due
	}
	return now
}
