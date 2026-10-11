// A due scheduled close also retires due siblings that share its financial
// owner. Each sibling's deadline comes from its own queued close; the sibling's
// task later finishes through the terminal fast path without any owner.
//
// The queue runs in deadline order and then in publication order, which is
// contract creation order. One payer grant owns runs of thousands of
// consecutive queued closes, so at the queue head every worker slot claims the
// same owner, one closes a batch and the rest refuse. A refused or terminal
// close retires nothing of its own. It instead closes one other owner's due
// queued closes, sampled from random queue pages, so the fleet works many
// owners at once instead of the one at the head.
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

// Random queue pages read per steal. A queue of at most
// scheduledCloseStealFullScanPages pages is read completely instead.
const scheduledCloseStealPages = 8
const scheduledCloseStealFullScanPages = 64

// Line pointers probed per sampled page. A pending row is far larger than a
// 64th of a page, so this visits every row on it.
const scheduledCloseStealPageTuples = 64

// Sampled candidates whose custody one read covers.
const scheduledCloseStealCandidateLimit = 64

// Concurrent steals in one process. Each one holds at most one owner turn,
// on one direct maintenance connection, and closes at most one owner batch.
const scheduledCloseStealConcurrency = 8

// Distinct owners one steal tries when another closer holds the first.
const scheduledCloseStealOwnerAttempts = 2

// A queued close runs under the default two-minute task ceiling. Its steal
// ends at this bound, so a slow owner batch rolls back and the refused or
// terminal close still finishes with its own ordinary result.
const scheduledCloseStealTimeout = time.Minute

var scheduledCloseStealSlots = make(chan struct{}, scheduledCloseStealConcurrency)

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
			keys = append(keys, task.RunOnce("close_scheduled_contract", contractId).String())
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
// sampled due queued close of an owner disjoint from heldBalanceIds, with up to
// a batch of that owner's due siblings, through the ordinary owner batch.
// Each closed contract keeps the deadline of its own queued close. Returns the
// contracts newly closed. Steals are best effort: a busy process, the steal
// bound or any failure closes nothing and never fails the caller's own close.
func stealScheduledCloseOwner(ctx context.Context, now time.Time, excludeContractId server.Id, heldBalanceIds []server.Id) (stolenClosed int) {
	select {
	case scheduledCloseStealSlots <- struct{}{}:
		defer func() { <-scheduledCloseStealSlots }()
	default:
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, scheduledCloseStealTimeout)
	defer cancel()
	server.HandleError(func() {
		candidates := sampleDueScheduledCloses(ctx, now, excludeContractId)
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
				closed, err := model.ReconcileContractOwnerAtDeadline(ctx, state.BalanceIds, candidate, dueScheduledCloses(now))
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
