// A due scheduled close also retires due siblings that share its financial
// owner. Each sibling's deadline comes from its own queued close or, for a
// contract issued with one, its stored deadline; a queued sibling's task later
// finishes through the terminal fast path without any owner.
//
// The queue runs in deadline order and then in publication order, which is
// contract creation order. One payer grant owns runs of thousands of
// consecutive queued closes, so at the queue head every worker slot claims the
// same owner, one closes a batch and the rest refuse. A refused or terminal
// close retires nothing of its own. It instead closes one other owner's due
// contracts: first the owner of a contract whose stored deadline passed within
// the last hour, read from a random slice of that hour, then within the rest
// of the last day and the rest of the deadline lookback, and otherwise the
// owner of a queued close sampled from random queue pages. The queue holds only
// legacy contracts behind days of older deadlines, so contracts that carry
// their own deadline are otherwise reached by no queued close at all. The
// newest come first: most hold escrow on a grant that is still active.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	mathrand "math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
)

// A held owner reschedules its close after this delay instead of waiting for
// admission. The deadline in the arguments is unchanged.
const scheduledCloseOwnerBusyDeferral = 5 * time.Second

// A deferral wakes at a uniform point of [deferral, 2 deferral), so the closes
// one held owner refused together do not all wake and refuse together again.
// A test fixes the offset.
var scheduledCloseDeferralJitter = func(span time.Duration) time.Duration {
	return time.Duration(mathrand.Int64N(int64(span)))
}

// When a refused or slotless close wakes again.
func scheduledCloseDeferredUntil(now time.Time) time.Time {
	return now.Add(scheduledCloseOwnerBusyDeferral + scheduledCloseDeferralJitter(scheduledCloseOwnerBusyDeferral))
}

// Random queue pages read per steal. A queue of at most
// scheduledCloseStealFullScanPages pages is read completely instead.
const scheduledCloseStealPages = 8
const scheduledCloseStealFullScanPages = 64

// Line pointers probed per sampled page. A pending row is far larger than a
// 64th of a page, so this visits every row on it.
const scheduledCloseStealPageTuples = 64

// Sampled candidates whose custody one read covers.
const scheduledCloseStealCandidateLimit = 64

// Concurrent steals in one process, an upper limit that contract_close.yml can
// lower. Each one holds at most one owner turn, on one direct maintenance
// connection, and closes at most one owner batch.
const scheduledCloseStealConcurrency = 4

// Distinct owners one steal tries when another closer holds the first.
const scheduledCloseStealOwnerAttempts = 2

// A queued close runs under the default two-minute task ceiling. Its steal
// ends at this bound, so a slow owner batch rolls back and the refused or
// terminal close still finishes with its own ordinary result.
const scheduledCloseStealTimeout = time.Minute

// A steal looks for contracts whose stored deadline passed within each window
// in turn: the last hour, the rest of the last day, the rest of the lookback.
// It reads one random slice of a window, so a window already closed costs at
// most a slice of index entries. Main's contracts carry a stored deadline from
// 2026-10-07, so once that date is beyond the lookback no slice reads only
// legacy contracts.
const scheduledCloseRecentWindow = time.Hour
const scheduledCloseDayWindow = 24 * time.Hour
const scheduledCloseDeadlineLookback = 5 * 24 * time.Hour
const scheduledCloseRecentSlice = 2 * time.Second

var scheduledCloseStealSlots = make(chan struct{}, scheduledCloseStealConcurrency)

// Chooses a uniform slice start between windowStart and span later. A test
// fixes it on its own contracts.
var scheduledCloseRecentSliceStart = func(windowStart time.Time, span time.Duration) time.Time {
	return windowStart.Add(time.Duration(mathrand.Int64N(int64(span))))
}

// The canonical target name of a queued close, set once the function exists.
var scheduledCloseFunctionName string

var scheduledCloseFunctionVersion = regexp.MustCompile(`/v\d+`)

// Runs after package variables, so the name can refer to its own target.
func init() {
	scheduledCloseFunctionName = task.NewTaskTarget(CloseScheduledContract).TargetFunctionName()
}

// The deadline authority of one queued close row: its arguments' deadline,
// lowered to an earlier merged wake as its target would. Rows of another
// function or with malformed or non-private arguments have none.
func scheduledCloseDeadline(functionName string, argsJson string, runAt time.Time) (model.ContractDeadline, bool) {
	if scheduledCloseFunctionVersion.ReplaceAllString(functionName, "") != scheduledCloseFunctionName {
		return model.ContractDeadline{}, false
	}
	var args CloseScheduledContractArgs
	if json.Unmarshal([]byte(argsJson), &args) != nil || !args.Private || args.ContractId == (server.Id{}) || args.Deadline.IsZero() {
		return model.ContractDeadline{}, false
	}
	deadline := args.Deadline
	if !runAt.IsZero() && runAt.Before(deadline) {
		deadline = runAt
	}
	return model.ContractDeadline{ContractId: args.ContractId, Deadline: deadline}, true
}

// Selects the candidates that are due through their own queued close and uses
// that task's deadline, lowered to an earlier merged wake as its target would.
// A candidate without a queued close has no scheduled deadline and is skipped.
func dueScheduledCloses(now time.Time) model.ContractDeadlineDueFunc {
	return func(ctx context.Context, query server.PgCanQuery, contractIds []server.Id) (deadlines []model.ContractDeadline) {
		candidates := make(map[server.Id]bool, len(contractIds))
		keys := make([]string, 0, len(contractIds))
		for _, contractId := range contractIds {
			candidates[contractId] = true
			keys = append(keys, model.ScheduledContractCloseRunOnce(contractId).String())
		}
		rows, err := query.Query(ctx, `SELECT function_name,args_json,run_at FROM pending_task WHERE run_once_key=ANY($1::text[])`, keys)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var functionName, argsJson string
				var runAt time.Time
				server.Raise(rows.Scan(&functionName, &argsJson, &runAt))
				deadline, ok := scheduledCloseDeadline(functionName, argsJson, runAt)
				if ok && candidates[deadline.ContractId] && !now.Before(deadline.Deadline) {
					deadlines = append(deadlines, deadline)
				}
			}
		})
		return
	}
}

// A contract issued with an immutable deadline carries on its row the deadline
// its queued close would carry, so once that deadline passes the contract is
// due without a queued close. A due queued close keeps its own, possibly
// earlier, deadline. A legacy contract without a stored deadline is due only
// through its queued close.
func dueContractDeadlines(now time.Time) model.ContractDeadlineDueFunc {
	scheduled := dueScheduledCloses(now)
	return func(ctx context.Context, query server.PgCanQuery, contractIds []server.Id) []model.ContractDeadline {
		deadlines := scheduled(ctx, query, contractIds)
		queued := make(map[server.Id]bool, len(deadlines))
		for _, deadline := range deadlines {
			queued[deadline.ContractId] = true
		}
		unqueuedIds := make([]server.Id, 0, len(contractIds))
		for _, contractId := range contractIds {
			if !queued[contractId] {
				unqueuedIds = append(unqueuedIds, contractId)
			}
		}
		return append(deadlines, model.ReadDueContractExpirationsInConn(ctx, query, unqueuedIds, now)...)
	}
}

// Reads open contracts whose stored deadline passed at now, from one random
// slice of those whose creation-based deadline passed within the recent window
// and, while a slice holds none, from one random slice of each older window,
// ordered by contract. The caller's own contract is skipped. Each read stops at
// one candidate batch, and its connection is released before returning.
func sampleRecentDueContracts(ctx context.Context, now time.Time, excludeContractId server.Id) (candidates []model.ContractDeadline) {
	windowEnd := now.Add(-model.DefaultContractExpiration)
	windows := []time.Duration{scheduledCloseRecentWindow, scheduledCloseDayWindow - scheduledCloseRecentWindow,
		scheduledCloseDeadlineLookback - scheduledCloseDayWindow}
	for _, window := range windows {
		windowStart := windowEnd.Add(-window)
		sliceStart := scheduledCloseRecentSliceStart(windowStart, window-scheduledCloseRecentSlice)
		contracts, err := model.ReadOpenContractDeadlines(ctx, model.ContractIdTimeBound(sliceStart),
			model.ContractIdTimeBound(sliceStart.Add(scheduledCloseRecentSlice)), scheduledCloseStealCandidateLimit, false)
		if err != nil {
			glog.Infof("[close-scheduled]recent sample failed: %v\n", err)
			return nil
		}
		for _, contract := range contracts {
			if deadline, ok := contract.Due(now); ok && contract.ContractId != excludeContractId {
				candidates = append(candidates, deadline)
			}
		}
		if len(candidates) != 0 {
			return
		}
		windowEnd = windowStart
	}
	return
}

// Reads due, unclaimed queued closes from random pages of the queue, ordered by
// contract. A claimed close belongs to its running owner and is skipped, as is
// the caller's own contract. The connection is released before returning.
func sampleDueScheduledCloses(ctx context.Context, now time.Time, excludeContractId server.Id) (candidates []model.ContractDeadline) {
	server.Db(ctx, func(conn server.PgConn) {
		var pageCount int64
		server.Raise(conn.QueryRow(ctx, `SELECT pg_relation_size('pending_task') / current_setting('block_size')::bigint`).Scan(&pageCount))
		if pageCount <= 0 {
			return
		}
		pages := []int64{}
		if pageCount <= scheduledCloseStealFullScanPages {
			for page := range pageCount {
				pages = append(pages, page)
			}
		} else {
			seen := map[int64]bool{}
			for range scheduledCloseStealPages {
				page := mathrand.Int64N(pageCount)
				if !seen[page] {
					seen[page] = true
					pages = append(pages, page)
				}
			}
		}
		tids := make([]string, 0, len(pages)*scheduledCloseStealPageTuples)
		for _, page := range pages {
			for offset := 1; offset <= scheduledCloseStealPageTuples; offset++ {
				tids = append(tids, fmt.Sprintf("(%d,%d)", page, offset))
			}
		}
		// A TID scan reads exactly the sampled pages, whatever the queue order.
		rows, err := conn.Query(ctx, `SELECT function_name,args_json,run_at,release_time
			FROM pending_task WHERE ctid=ANY($1::text[]::tid[])`, tids)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var functionName, argsJson string
				var runAt, releaseTime time.Time
				server.Raise(rows.Scan(&functionName, &argsJson, &runAt, &releaseTime))
				deadline, ok := scheduledCloseDeadline(functionName, argsJson, runAt)
				if ok && deadline.ContractId != excludeContractId && !now.Before(deadline.Deadline) && !now.Before(releaseTime) {
					candidates = append(candidates, deadline)
				}
			}
		})
	}, server.OptNoRetry())
	slices.SortFunc(candidates, func(a, b model.ContractDeadline) int { return a.ContractId.Cmp(b.ContractId) })
	return slices.CompactFunc(candidates, func(a, b model.ContractDeadline) bool { return a.ContractId == b.ContractId })
}

// Spends a claim whose own close retired nothing on another owner: closes one
// due contract of an owner disjoint from heldBalanceIds, with up to a batch of
// that owner's due siblings, through the ordinary owner batch. A contract due
// through its stored deadline, recently due first, is preferred to a sampled
// queued close. Each closed contract keeps the deadline of its own queued close
// or its stored deadline. Returns the contracts newly closed. Steals are best
// effort: a busy process, the steal bound or any failure closes nothing and
// never fails the caller's own close.
func stealScheduledCloseOwner(ctx context.Context, now time.Time, excludeContractId server.Id, heldBalanceIds []server.Id) (stolenClosed int) {
	// contract_close.yml can lower the concurrent steals below the slots
	_, stealConcurrency := contractCloseConcurrencyLimits()
	release, admitted := admitScheduledCloseSteal(scheduledCloseStealSlots, stealConcurrency)
	if !admitted {
		return 0
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, scheduledCloseStealTimeout)
	defer cancel()
	server.HandleError(func() {
		candidates := sampleRecentDueContracts(ctx, now, excludeContractId)
		if len(candidates) == 0 {
			candidates = sampleDueScheduledCloses(ctx, now, excludeContractId)
		}
		attempted := map[string]bool{}
		// Queued closes outlive their contracts, so read custody a chunk at a
		// time until an open contract of an untried owner turns up.
		for offset := 0; offset < len(candidates); offset += scheduledCloseStealCandidateLimit {
			chunk := candidates[offset:min(offset+scheduledCloseStealCandidateLimit, len(candidates))]
			ids := make([]server.Id, 0, len(chunk))
			for _, candidate := range chunk {
				ids = append(ids, candidate.ContractId)
			}
			custody, err := model.ReadContractDeadlineCustody(ctx, ids)
			if err != nil {
				glog.Infof("[close-scheduled]steal custody read failed: %v\n", err)
				return
			}
			for _, candidate := range chunk {
				state := custody[candidate.ContractId]
				if state == nil || !state.Found || state.Outcome != nil || len(state.BalanceIds) == 0 ||
					model.ContractDeadlineOwnerBatchLimit < len(state.BalanceIds) {
					continue
				}
				held := false
				ownerParts := make([]string, 0, len(state.BalanceIds))
				for _, balanceId := range state.BalanceIds {
					held = held || slices.Contains(heldBalanceIds, balanceId)
					ownerParts = append(ownerParts, balanceId.String())
				}
				owner := strings.Join(ownerParts, ",")
				if held || attempted[owner] {
					continue
				}
				attempted[owner] = true
				closed, err := model.ReconcileContractOwnerAtDeadline(ctx, state.BalanceIds, candidate, dueContractDeadlines(now))
				if err != nil {
					glog.Infof("[close-scheduled]steal of %s failed: %v\n", candidate.ContractId, err)
					return
				}
				if !closed.Busy {
					if primary := closed.Reconciliation; primary != nil && !primary.AlreadyClosed && !primary.Missing {
						stolenClosed++
					}
					stolenClosed += closed.SiblingsClosed
					return
				}
				if scheduledCloseStealOwnerAttempts <= len(attempted) {
					return
				}
			}
		}
	}, func(err error) {
		glog.Infof("[close-scheduled]steal failed: %v\n", err)
	})
	return
}
