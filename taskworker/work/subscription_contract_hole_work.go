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
	Help: "Elapsed time of the last traversed contract-hole pass, including page scheduling delays",
})

var contractHoleRefreshAvailable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_contract_hole_refresh_available",
	Help: "One only after a complete timely contract-hole pass published a surviving earliest-pair witness",
})

var contractHoleRefreshFailedPairs = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_contract_hole_refresh_failed_pairs",
	Help: "Failed pair observations in the last traversed contract-hole source pass",
})

func init() {
	prometheus.MustRegister(contractHoleRefreshCompleted, contractHoleRefreshDuration,
		contractHoleRefreshAvailable, contractHoleRefreshFailedPairs)
}

// PassStarted fixes cadence independently of page count and finalizer latency.
type RefreshContractHolesArgs struct {
	Cursor           *model.ContractHoleCursor  `json:"cursor,omitempty"`
	PassStarted      time.Time                  `json:"pass_started"`
	FailedPairs      int                        `json:"failed_pairs,omitempty"`
	PairVisits       int                        `json:"pair_visits,omitempty"`
	SuccessfulPairs  int                        `json:"successful_pairs,omitempty"`
	PositivePairs    int                        `json:"positive_pairs,omitempty"`
	Pages            int                        `json:"pages,omitempty"`
	EarliestPositive *model.ContractHoleWitness `json:"earliest_positive,omitempty"`
}

// Nil cursor means the indexed source pass reached its end.
type RefreshContractHolesResult struct {
	Cursor           *model.ContractHoleCursor  `json:"cursor,omitempty"`
	Pairs            int                        `json:"pairs"`
	FailedPairs      int                        `json:"failed_pairs"`
	PairVisits       int                        `json:"pair_visits"`
	SuccessfulPairs  int                        `json:"successful_pairs"`
	PositivePairs    int                        `json:"positive_pairs"`
	Pages            int                        `json:"pages"`
	EarliestPositive *model.ContractHoleWitness `json:"earliest_positive,omitempty"`
	WarmReady        bool                       `json:"warm_ready"`
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
	return refreshContractHolesWithSource(args, clientSession, model.RefreshContractHolesPage)
}

// The source boundary permits a deterministic dependency failure through the
// real task evaluator; scheduling, cursor custody and readiness remain owned here.
func refreshContractHolesWithSource(args *RefreshContractHolesArgs, clientSession *session.ClientSession,
	readSource func(context.Context, *model.ContractHoleCursor) (*model.ContractHoleRefreshPageResult, error),
) (*RefreshContractHolesResult, error) {
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 15*time.Second)
	defer cancel()
	page, err := readSource(ctx, args.Cursor)
	if err != nil {
		contractHoleRefreshAvailable.Set(0)
		_ = model.InvalidateContractHoleReadiness(clientSession.Ctx)
		return nil, task.WithRetryDelayAndArgs(err, task.RescheduleTimeout, args)
	}
	now := server.NowUtc()
	result := observeContractHoleRefreshPage(args, page, now)
	if result.FailedPairs > 0 {
		_ = model.InvalidateContractHoleReadiness(clientSession.Ctx)
	} else if result.Cursor == nil {
		result.WarmReady, err = model.PublishContractHoleReadiness(clientSession.Ctx, &model.ContractHoleReadiness{
			Version: 1, PassStarted: args.PassStarted, PassCompleted: now, PairVisits: result.PairVisits,
			SuccessfulPairs: result.SuccessfulPairs, UnknownPairs: result.FailedPairs, PositivePairs: result.PositivePairs,
			Pages: result.Pages, EarliestPositive: result.EarliestPositive,
		})
		if err != nil || !result.WarmReady {
			contractHoleRefreshAvailable.Set(0)
		} else {
			contractHoleRefreshAvailable.Set(1)
		}
	}
	return result, nil
}

// A traversed source pass is not complete coverage if any pair failed, was
// superseded, or exceeded its bound. Only a later entire healthy pass recovers it.
func observeContractHoleRefreshPage(args *RefreshContractHolesArgs, page *model.ContractHoleRefreshPageResult, now time.Time) *RefreshContractHolesResult {
	failedPairs := args.FailedPairs + page.FailedPairs
	if failedPairs > 0 {
		contractHoleRefreshAvailable.Set(0)
	}
	if page.Cursor == nil {
		contractHoleRefreshDuration.Set(max(0, now.Sub(args.PassStarted).Seconds()))
		contractHoleRefreshFailedPairs.Set(float64(failedPairs))
		if failedPairs == 0 {
			contractHoleRefreshCompleted.Set(float64(now.Unix()))
		}
	}
	earliest := args.EarliestPositive
	if page.EarliestPositive != nil && (earliest == nil || page.EarliestPositive.SourceStarted.Before(earliest.SourceStarted)) {
		earliest = page.EarliestPositive
	}
	return &RefreshContractHolesResult{Cursor: page.Cursor, Pairs: page.Pairs, FailedPairs: failedPairs,
		PairVisits: args.PairVisits + page.Pairs, SuccessfulPairs: args.SuccessfulPairs + page.Pairs - page.FailedPairs,
		PositivePairs: args.PositivePairs + page.PositivePairs, Pages: args.Pages + 1, EarliestPositive: earliest,
	}
}

// Continuations run immediately; completed passes begin again at half the TTL.
// A slow pass restarts immediately and leaves expired projections fail closed.
func RefreshContractHolesPost(args *RefreshContractHolesArgs, result *RefreshContractHolesResult, clientSession *session.ClientSession, tx server.PgTx) error {
	now := server.NowUtc()
	next := &RefreshContractHolesArgs{Cursor: result.Cursor, PassStarted: args.PassStarted, FailedPairs: result.FailedPairs,
		PairVisits: result.PairVisits, SuccessfulPairs: result.SuccessfulPairs, PositivePairs: result.PositivePairs,
		Pages: result.Pages, EarliestPositive: result.EarliestPositive}
	runAt := now
	if result.Cursor == nil {
		runAt = maxContractHoleRefreshTime(now, args.PassStarted.Add(model.ContractHoleRefreshInterval))
		next = &RefreshContractHolesArgs{PassStarted: runAt}
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
