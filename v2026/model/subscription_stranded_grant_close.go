// A contract whose grant ended before the contract settled is stranded: no
// current grant reserves it, the apps count its bytes as Pending, and its
// provider waits for payment until a closer retires it. The payer drain
// (subscription_payer_due_close.go) retires a payer's due contracts, but only
// the payer's own activity requests it: a balance read in the app or a contract
// its balance refused. A payer with neither waited for the contract-ordered
// passes, which a backlog can hold back for weeks. On Main on 2026-10-29 the
// proxy network of support inbox 7830 held 5,334 contracts a day or more past
// their deadline, on daily grants that ended from 10-20 to 10-29. Its
// contracts came from the hosted proxy, its app's clients last authenticated
// on 10-20, and its current grants had room, so no drain was ever requested.
// Meanwhile the deadline pass's retry span walked down through contracts
// minted on 10-22 at about a sixth of real time, and the expiry sweep read
// contracts minted on 10-18 at about a fifth.
//
// The stranded-grant pass finds those payers from their grants instead. It
// walks grant end times and, for each grant that ended Settle ago and still
// holds unsettled escrow, runs its payer's drain. Every contract on such a grant
// is past its deadline by then, since a contract is admitted only while its
// grant runs and is due DefaultContractExpiration after. New end times go
// first, as they settle; the backfill walks back from the first run's edge to
// the lookback, newest first, so payers still in use come before ones long
// idle. One run visits payers in network order within a window of end times
// and keeps its position, and a payer's turn is one drain page: a payer with
// more due contracts than a page waits for its next ended grant or its own
// requests, so one large payer cannot hold the pass.
//
// The pass is one task, so it adds one drain stream to the primary however many
// payers wait, and it never fills the task queue: the queue's claim scan skips
// a limited target's rows one by one, so payer drains must not crowd its head.
package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Tunes the stranded-grant pass.
type StrandedGrantCloseSettings struct {
	// The backfill's reach on the first run, as the deadline pass's lookback.
	Lookback time.Duration
	// The span of grant end times one read covers.
	Window time.Duration
	// A grant is examined this long after it ended, once every contract on it
	// is past its deadline and the drain's margin.
	Settle time.Duration
	// Payers one read returns.
	PageSize int
	// Bounds one read of a window.
	ReadTimeout time.Duration
	// How long one run keeps starting payers; a started drain finishes its turn.
	RunBudget time.Duration
	// A run that found nothing left waits this long for new end times.
	IdleDelay time.Duration
	// A run whose read failed waits this long before its successor.
	ErrorDelay time.Duration
	// The task's maximum run time.
	MaxTime time.Duration
}

// On Main on 2026-10-29 the read of the window holding the 01:00 UTC end of
// the daily free grants, 1.1 million grants, found the 16,973 that still held
// escrow in about 5 s; other hours hold far fewer grants. A payer's drain page
// takes a read and its owners' turns, under a second for the few contracts most
// payers strand.
func DefaultStrandedGrantCloseSettings() *StrandedGrantCloseSettings {
	return &StrandedGrantCloseSettings{
		Lookback:    14 * 24 * time.Hour,
		Window:      time.Hour,
		Settle:      DefaultContractExpiration + 5*time.Minute,
		PageSize:    4096,
		ReadTimeout: 30 * time.Second,
		RunBudget:   time.Minute,
		IdleDelay:   time.Minute,
		ErrorDelay:  time.Minute,
		MaxTime:     5 * time.Minute,
	}
}

// Grants that ended strictly after Lower up to Upper. The payers up to After in
// network order were visited.
type StrandedGrantWindow struct {
	Lower time.Time `json:"lower"`
	Upper time.Time `json:"upper"`
	After server.Id `json:"after"`
}

// The pass's position. Grants that ended up to Fresh were examined, or belong to
// the backfill, which examines those that ended after BackfillFloor up to
// BackfillUpper, newest first. A window in progress keeps its bounds, so the
// end times it covers do not move under its payer position.
type CloseStrandedGrantContractsArgs struct {
	Fresh          time.Time            `json:"fresh"`
	FreshWindow    *StrandedGrantWindow `json:"fresh_window,omitempty"`
	BackfillFloor  time.Time            `json:"backfill_floor,omitempty"`
	BackfillUpper  time.Time            `json:"backfill_upper,omitempty"`
	BackfillWindow *StrandedGrantWindow `json:"backfill_window,omitempty"`
}

// One run. Payers counts the payers whose drain ran, Closed the contracts they
// retired and Failed the drains that failed; a failed payer is passed. Exhausted
// reports that the budget ended with work remaining, and Error a read that
// stopped the run.
type CloseStrandedGrantContractsResult struct {
	Next      CloseStrandedGrantContractsArgs `json:"next"`
	Payers    int                             `json:"payers"`
	Closed    int                             `json:"closed"`
	Failed    int                             `json:"failed,omitempty"`
	Exhausted bool                            `json:"exhausted,omitempty"`
	Error     string                          `json:"error,omitempty"`
}

// The read of ReadStrandedGrantPayers, apart so its plan can be tested: $1 and
// $2 bound the end times, $3 the payer position, $4 the page. The window's
// grants are read through the end-time index and each is checked for an
// unsettled escrow through the unsettled-escrow index, before the payers are
// ordered: ordered by network_id alone, PostgreSQL could walk a network index
// across every grant until a page of payers matched.
const strandedGrantPayersSql = `WITH stranded AS MATERIALIZED (
		SELECT b.network_id FROM transfer_balance b
		WHERE b.end_time > $1 AND b.end_time <= $2
			AND EXISTS (SELECT 1 FROM transfer_escrow e WHERE e.balance_id = b.balance_id AND e.settled = false)
	)
	SELECT DISTINCT network_id FROM stranded WHERE network_id > $3 ORDER BY network_id LIMIT $4`

// The payers, after afterNetworkId in network order and at most limit, of the
// grants that ended after endedAfter up to endedBefore and still hold an
// unsettled escrow.
func ReadStrandedGrantPayers(ctx context.Context, endedAfter time.Time, endedBefore time.Time, afterNetworkId server.Id,
	limit int) (payerNetworkIds []server.Id, returnErr error) {
	if limit < 1 {
		return nil, errors.New("invalid stranded grant payer page")
	}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, strandedGrantPayersSql, endedAfter.UTC(), endedBefore.UTC(), afterNetworkId, limit)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var payerNetworkId server.Id
					server.Raise(rows.Scan(&payerNetworkId))
					payerNetworkIds = append(payerNetworkIds, payerNetworkId)
				}
			})
		}, server.OptNoRetry())
	}, func(err error) { payerNetworkIds, returnErr = nil, err })
	return
}

// The pass's reads and drains; tests replace them.
type strandedGrantCloseOps struct {
	readPayers func(ctx context.Context, endedAfter time.Time, endedBefore time.Time, afterNetworkId server.Id,
		limit int) ([]server.Id, error)
	drain func(ctx context.Context, payerNetworkId server.Id, now time.Time, more func() bool,
		settings *PayerDueContractCloseSettings) (*PayerDueContractClose, error)
}

func defaultStrandedGrantCloseOps() strandedGrantCloseOps {
	return strandedGrantCloseOps{readPayers: ReadStrandedGrantPayers, drain: ClosePayerDueContracts}
}

// One run at now from args. more reports whether another payer may start, and
// each drain checks it before its owners' turns.
func CloseStrandedGrantContracts(ctx context.Context, args *CloseStrandedGrantContractsArgs, now time.Time, more func() bool,
	settings *StrandedGrantCloseSettings) *CloseStrandedGrantContractsResult {
	return closeStrandedGrantContracts(ctx, args, now, more, settings, defaultStrandedGrantCloseOps())
}

func closeStrandedGrantContracts(ctx context.Context, args *CloseStrandedGrantContractsArgs, now time.Time, more func() bool,
	settings *StrandedGrantCloseSettings, ops strandedGrantCloseOps) *CloseStrandedGrantContractsResult {
	result := &CloseStrandedGrantContractsResult{Next: *args}
	next := &result.Next
	copyWindow := func(window *StrandedGrantWindow) *StrandedGrantWindow {
		if window == nil {
			return nil
		}
		copied := *window
		return &copied
	}
	next.FreshWindow, next.BackfillWindow = copyWindow(args.FreshWindow), copyWindow(args.BackfillWindow)
	edge := now.Add(-settings.Settle)
	if next.Fresh.IsZero() {
		next.Fresh = edge
		next.BackfillFloor = now.Add(-settings.Lookback)
		next.BackfillUpper = edge
	}
	drainSettings := DefaultPayerDueContractCloseSettings()

	// Visits the window's payers from its position. done reports that every
	// payer of the window was visited; a false done with a nil error means the
	// budget ended.
	visit := func(window *StrandedGrantWindow) (done bool, err error) {
		for {
			if !more() {
				return false, nil
			}
			readCtx, readCancel := context.WithTimeout(ctx, settings.ReadTimeout)
			payerNetworkIds, err := ops.readPayers(readCtx, window.Lower, window.Upper, window.After, settings.PageSize)
			readCancel()
			if err != nil {
				return false, err
			}
			for _, payerNetworkId := range payerNetworkIds {
				if !more() {
					return false, nil
				}
				drained, err := ops.drain(ctx, payerNetworkId, now, more, drainSettings)
				if err != nil {
					// A failed drain leaves the payer to its next ended grant
					// and its own requests; it does not hold the pass.
					result.Failed++
					glog.Infof("[stranded-grant-close]%s drain failed: %v\n", payerNetworkId, err)
				} else {
					if drained.Closed == 0 && drained.More && !more() {
						// The budget ended before the payer's first turn:
						// the next run starts with it.
						return false, nil
					}
					result.Payers++
					result.Closed += drained.Closed
				}
				window.After = payerNetworkId
			}
			if len(payerNetworkIds) < settings.PageSize {
				return true, nil
			}
		}
	}

	for {
		if next.FreshWindow == nil && next.Fresh.Before(edge) {
			next.FreshWindow = &StrandedGrantWindow{Lower: next.Fresh, Upper: server.MinTime(next.Fresh.Add(settings.Window), edge)}
		}
		window := next.FreshWindow
		fresh := window != nil
		if !fresh {
			if next.BackfillWindow == nil && next.BackfillFloor.Before(next.BackfillUpper) {
				next.BackfillWindow = &StrandedGrantWindow{
					Lower: server.MaxTime(next.BackfillUpper.Add(-settings.Window), next.BackfillFloor),
					Upper: next.BackfillUpper,
				}
			}
			window = next.BackfillWindow
		}
		if window == nil {
			// caught up
			return result
		}
		done, err := visit(window)
		if err != nil {
			result.Error = err.Error()
			glog.Infof("[stranded-grant-close]read of grants that ended after %s up to %s failed: %v\n",
				window.Lower, window.Upper, err)
			return result
		}
		if !done {
			result.Exhausted = true
			return result
		}
		if fresh {
			next.Fresh = window.Upper
			next.FreshWindow = nil
		} else {
			next.BackfillUpper = window.Lower
			next.BackfillWindow = nil
		}
	}
}

func scheduleCloseStrandedGrantContractsInTx(clientSession *session.ClientSession, tx server.PgTx,
	args *CloseStrandedGrantContractsArgs, runAt time.Time) {
	task.ScheduleTaskInTx(tx, CloseStrandedGrantContractsTask, args, clientSession,
		task.RunOnce("close_stranded_grant_contracts"), task.RunAt(runAt),
		task.MaxTime(DefaultStrandedGrantCloseSettings().MaxTime))
}

// Startup arms the pass at its first position; a pending run keeps its own.
func ScheduleCloseStrandedGrantContracts(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleCloseStrandedGrantContractsInTx(clientSession, tx, &CloseStrandedGrantContractsArgs{}, server.NowUtc())
}

// One budgeted run from the retained position.
func CloseStrandedGrantContractsTask(args *CloseStrandedGrantContractsArgs, clientSession *session.ClientSession) (*CloseStrandedGrantContractsResult, error) {
	settings := DefaultStrandedGrantCloseSettings()
	started := time.Now()
	result := CloseStrandedGrantContracts(clientSession.Ctx, args, server.NowUtc(), func() bool {
		return clientSession.Ctx.Err() == nil && time.Since(started) < settings.RunBudget
	}, settings)
	if err := clientSession.Ctx.Err(); err != nil {
		// The successor repeats from the unchanged position.
		return nil, err
	}
	glog.Infof("[stranded-grant-close]payers=%d closed=%d failed=%d exhausted=%t fresh=%s backfill=%s%s\n",
		result.Payers, result.Closed, result.Failed, result.Exhausted, result.Next.Fresh, result.Next.BackfillUpper,
		func() string {
			if result.Error != "" {
				return fmt.Sprintf(" error=%s", result.Error)
			}
			return ""
		}())
	return result, nil
}

// The successor runs at once while work remains, after the error delay when a
// read failed, and otherwise after the idle delay, from the position this run
// reached.
func CloseStrandedGrantContractsTaskPost(args *CloseStrandedGrantContractsArgs, result *CloseStrandedGrantContractsResult,
	clientSession *session.ClientSession, tx server.PgTx) error {
	settings := DefaultStrandedGrantCloseSettings()
	runAt := server.NowUtc()
	switch {
	case result.Error != "":
		runAt = runAt.Add(settings.ErrorDelay)
	case !result.Exhausted:
		runAt = runAt.Add(settings.IdleDelay)
	}
	scheduleCloseStrandedGrantContractsInTx(clientSession, tx, &result.Next, runAt)
	return nil
}

// A failed run retries within a minute from its unchanged position.
func NewCloseStrandedGrantContractsTaskTarget() task.Target {
	return task.WithErrorRetryCap(task.NewTaskTargetWithPost(CloseStrandedGrantContractsTask, CloseStrandedGrantContractsTaskPost), time.Minute)
}
