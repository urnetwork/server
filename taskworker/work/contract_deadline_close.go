// Contracts issued with an immutable deadline get no queued close. The row
// already carries the deadline a queued close would copy from it, and a queued
// close would wait behind days of legacy deadlines at the queue head. One
// continuous pass retires them at that deadline through the ordinary owner
// batch instead, a bounded number of owners at a time.
//
// Each run first visits the contracts whose deadline passed since the previous
// run, newest first. It then continues a retry pass over contracts earlier
// runs left open or never reached, and finally the backlog pass: the first run
// starts it at the deadline edge and it walks down to the lookback. A contract a
// retry or the backlog pass leaves open is left to the queued closes, their
// steals and the expiry sweep.
//
// The backlog also closes the legacy contracts it passes, rows without a stored
// deadline. The startup pass queues closes only for the legacy contracts it
// reaches before a long run of pages without one, so a legacy contract left
// among contracts that carry a deadline has no other closer than the expiry
// sweep, and a walk that ended at a page of them stranded every contract below.
// A legacy contract below the backlog's start is due at its creation plus the
// default lifetime, the deadline the expiry sweep gives a missing one. It
// closes in its owner's turn, and one the backlog leaves open gets one retry.
// The pass leaves a legacy contract minted after the backlog's start alone.
//
// A run stops starting pages once its budget has passed. A page whose owners
// did not all start is read again by the next run, so a retained position never
// skips an unvisited contract. A page holds many owners, so it stays small.
package work

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Contracts read per page, the owner batches run at once (an upper limit that
// contract_close.yml can lower), and how long a run keeps starting pages and
// owners.
const contractDeadlineClosePageSize = 128
const contractDeadlineCloseParallel = 4
const contractDeadlineCloseBudget = time.Minute

// Started owners finish after the budget, under load within a few batches.
const contractDeadlineCloseMaxTime = 5 * time.Minute

// Admission waits can start a contract's database clock after the moment its
// id was minted. Contracts this close to the edge may not be due yet; the ones
// that are not wait for a retry.
const contractDeadlineCloseEdgeMargin = time.Minute

// The backlog pass never reads before this.
const contractDeadlineCloseLookback = 14 * 24 * time.Hour

// The position format. Positions without a version come from a release whose
// backlog skipped legacy contracts and ended its walk at the first full page
// of them. On Main a page of them minted at 2026-10-09 22:41 ended that walk
// 2.7 days above the oldest open contract, and no later run could resume it. A
// run that reads such a position walks the backlog again from the new-deadline
// position down to the lookback, or to the earlier walk's lower bound if lower.
const contractDeadlineCloseVersion = 1

// Until vacuum, the open-contract index still lists contracts other closers
// retired, and a page read checks each one. A read that takes longer stops the
// run with its positions kept, instead of holding the run to its ceiling. A
// test shortens it.
var contractDeadlineCloseReadTimeout = 30 * time.Second

// A run that finished its work waits this long for new deadlines.
const contractDeadlineCloseIdleDelay = 15 * time.Second

// Contracts strictly between the bounds.
type ContractDeadlineSpan struct {
	Lower server.Id `json:"lower"`
	Upper server.Id `json:"upper"`
}

// Every contract below Fresh was reached by a run's new deadlines or belongs
// to the backlog pass. Retry is the pass over contracts that earlier runs left
// open or never reached, and Pending collects the next one. A pass span's upper
// bound moves down as it proceeds. BacklogStart is where the backlog pass
// started; the legacy contracts below it are the backlog's.
type CloseContractDeadlinesArgs struct {
	Version      int                   `json:"version,omitempty"`
	Fresh        server.Id             `json:"fresh"`
	Retry        *ContractDeadlineSpan `json:"retry,omitempty"`
	Pending      *ContractDeadlineSpan `json:"pending,omitempty"`
	Backlog      *ContractDeadlineSpan `json:"backlog,omitempty"`
	BacklogStart server.Id             `json:"backlog_start"`
}

// Next is the successor's position. Closed counts contracts newly closed by
// this run's owner turns, including siblings outside its pages, and
// LegacyClosed the legacy contracts among them closed at their maximum
// lifetime.
type CloseContractDeadlinesResult struct {
	Next         CloseContractDeadlinesArgs `json:"next"`
	Visited      int                        `json:"visited"`
	Closed       int                        `json:"closed"`
	LegacyClosed int                        `json:"legacy_closed,omitempty"`
	Dropped      int                        `json:"dropped,omitempty"`
	// The budget ended a segment with work remaining.
	Exhausted bool `json:"exhausted,omitempty"`
}

// The passes of one run. Every pass keeps the contracts it finds not yet due
// for a retry. The new-deadline pass also keeps the due contracts it leaves
// open, and the others drop them, except that the backlog keeps the legacy
// ones for one retry. The retry and the backlog close the backlog's legacy
// contracts; the new-deadline pass leaves every legacy contract alone.
type contractDeadlinePass int

const (
	contractDeadlinePassFresh contractDeadlinePass = iota
	contractDeadlinePassRetry
	contractDeadlinePassBacklog
)

// Startup arms the pass at its first position; a pending run keeps its own.
func ScheduleCloseContractDeadlines(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleCloseContractDeadlines(clientSession, tx, &CloseContractDeadlinesArgs{}, server.NowUtc())
}

// A merge with a pending run keeps that run's position.
func scheduleCloseContractDeadlines(clientSession *session.ClientSession, tx server.PgTx, args *CloseContractDeadlinesArgs, runAt time.Time) {
	task.ScheduleTaskInTx(tx, CloseContractDeadlines, args, clientSession,
		task.RunOnce("close_contract_deadlines"), task.RunAt(runAt),
		task.MaxTime(contractDeadlineCloseMaxTime), task.Priority(task.TaskPriorityFastest))
}

// One budgeted run from the retained position.
func CloseContractDeadlines(args *CloseContractDeadlinesArgs, clientSession *session.ClientSession) (*CloseContractDeadlinesResult, error) {
	started := time.Now()
	return closeContractDeadlines(clientSession.Ctx, args, server.NowUtc(), func() bool {
		return time.Since(started) < contractDeadlineCloseBudget
	})
}

// The successor runs at once while work remains and otherwise after the idle
// delay, from the position this run reached.
func CloseContractDeadlinesPost(_ *CloseContractDeadlinesArgs, result *CloseContractDeadlinesResult,
	clientSession *session.ClientSession, tx server.PgTx) error {
	runAt := server.NowUtc()
	if !result.Exhausted {
		runAt = runAt.Add(contractDeadlineCloseIdleDelay)
	}
	scheduleCloseContractDeadlines(clientSession, tx, &result.Next, runAt)
	return nil
}

// A failed run retries within a minute from its unchanged position.
func NewContractDeadlineCloseTaskTarget() task.Target {
	return task.WithErrorRetryCap(task.NewTaskTargetWithPost(CloseContractDeadlines, CloseContractDeadlinesPost), time.Minute)
}

// One run at now. more reports whether another page may start, and is also
// checked before each owner starts. Cancellation returns its error, so the
// successor repeats from the unchanged position.
func closeContractDeadlines(ctx context.Context, args *CloseContractDeadlinesArgs, now time.Time, more func() bool) (*CloseContractDeadlinesResult, error) {
	result := &CloseContractDeadlinesResult{Next: CloseContractDeadlinesArgs{
		Version: contractDeadlineCloseVersion, Fresh: args.Fresh, BacklogStart: args.BacklogStart}}
	next := &result.Next
	copySpan := func(span *ContractDeadlineSpan) *ContractDeadlineSpan {
		if span == nil {
			return nil
		}
		copied := *span
		return &copied
	}
	next.Retry, next.Pending, next.Backlog = copySpan(args.Retry), copySpan(args.Pending), copySpan(args.Backlog)
	edge := model.ContractIdTimeBound(now.Add(-model.DefaultContractExpiration - contractDeadlineCloseEdgeMargin))
	lookback := model.ContractIdTimeBound(now.Add(-contractDeadlineCloseLookback))
	if next.Fresh == (server.Id{}) {
		next.Fresh, next.BacklogStart = edge, edge
		next.Backlog = &ContractDeadlineSpan{Lower: lookback, Upper: edge}
	} else {
		if args.Version < contractDeadlineCloseVersion {
			// Every contract below the new-deadline position was reached by
			// an earlier run or belongs to the backlog, so a walk from there
			// reaches every contract the earlier release left open.
			lower := lookback
			if next.Backlog != nil && next.Backlog.Lower.Cmp(lower) < 0 {
				lower = next.Backlog.Lower
			}
			next.Backlog = &ContractDeadlineSpan{Lower: lower, Upper: next.Fresh}
			next.BacklogStart = next.Fresh
		}
		if next.Retry == nil && next.Pending != nil {
			// Contracts left open by earlier runs get one more visit.
			next.Retry, next.Pending = next.Pending, nil
		}
	}

	// Exclusive bounds that include the id: one below it and one above it.
	idStep := func(id server.Id, up bool) server.Id {
		for index := len(id) - 1; 0 <= index; index-- {
			if up {
				id[index]++
				if id[index] != 0 {
					break
				}
			} else {
				id[index]--
				if id[index] != 0xff {
					break
				}
			}
		}
		return id
	}
	// Widens the next retry to the contracts strictly between the bounds.
	leaveSpan := func(lower server.Id, upper server.Id) {
		if upper.Cmp(idStep(lower, true)) <= 0 {
			return
		}
		if next.Pending == nil {
			next.Pending = &ContractDeadlineSpan{Lower: lower, Upper: upper}
			return
		}
		if lower.Cmp(next.Pending.Lower) < 0 {
			next.Pending.Lower = lower
		}
		if next.Pending.Upper.Cmp(upper) < 0 {
			next.Pending.Upper = upper
		}
	}
	leave := func(contractIds []server.Id) {
		for _, contractId := range contractIds {
			leaveSpan(idStep(contractId, false), idStep(contractId, true))
		}
	}

	// Closes the page's due contracts, each owner's in one owner batch. It is
	// complete when every owner started. A complete page leaves its not yet
	// due contracts for a retry and the due ones still open as its pass
	// directs; an incomplete page is read again.
	visit := func(page []model.OpenContractDeadline, pass contractDeadlinePass) (complete bool, returnErr error) {
		dueDeadlines := []model.ContractDeadline{}
		legacyDueIds := map[server.Id]bool{}
		notDueIds := []server.Id{}
		for _, contract := range page {
			deadline, due := contract.Due(now)
			if contract.ExpirationTime == nil {
				if pass == contractDeadlinePassFresh || next.BacklogStart.Cmp(contract.ContractId) <= 0 {
					continue
				}
				deadline, due = contract.LegacyDue(now)
				if due {
					legacyDueIds[contract.ContractId] = true
				}
			}
			if due {
				dueDeadlines = append(dueDeadlines, deadline)
			} else {
				notDueIds = append(notDueIds, contract.ContractId)
			}
		}
		dueIds := make([]server.Id, 0, len(dueDeadlines))
		for _, deadline := range dueDeadlines {
			dueIds = append(dueIds, deadline.ContractId)
		}
		custody, err := model.ReadContractDeadlineCustody(ctx, dueIds)
		if err != nil {
			return false, err
		}
		// Owners in page order. A contract without a grant closes alone. An
		// owner's legacy contracts are kept apart from the others.
		type contractOwner struct {
			balanceIds    []server.Id
			members       []model.ContractDeadline
			legacyMembers []model.ContractDeadline
		}
		owners := []*contractOwner{}
		keyOwners := map[string]*contractOwner{}
		for _, deadline := range dueDeadlines {
			state := custody[deadline.ContractId]
			if state == nil || !state.Found || state.Outcome != nil {
				continue
			}
			key := "contract:" + deadline.ContractId.String()
			if len(state.BalanceIds) != 0 {
				parts := make([]string, 0, len(state.BalanceIds))
				for _, balanceId := range state.BalanceIds {
					parts = append(parts, balanceId.String())
				}
				key = strings.Join(parts, ",")
			}
			owner := keyOwners[key]
			if owner == nil {
				owner = &contractOwner{balanceIds: state.BalanceIds}
				keyOwners[key] = owner
				owners = append(owners, owner)
			}
			if legacyDueIds[deadline.ContractId] {
				owner.legacyMembers = append(owner.legacyMembers, deadline)
			} else {
				owner.members = append(owner.members, deadline)
			}
		}
		newlyClosed := func(reconciliation *model.ContractDeadlineReconciliation) bool {
			return reconciliation != nil && !reconciliation.AlreadyClosed && !reconciliation.Missing
		}
		var stateLock sync.Mutex
		parallel, _ := contractCloseConcurrencyLimits()
		complete = runContractOwnerBatches(owners, parallel, more, func(owner *contractOwner) {
			closed, legacyClosed := 0, 0
			// A legacy contract leads the batch only when the page holds no
			// other contract of its owner.
			legacyPrimary := len(owner.members) == 0
			primaries := owner.members
			if legacyPrimary {
				primaries = owner.legacyMembers
			}
			primary := primaries[0]
			primaryClosed := false
			server.HandleError(func() {
				if len(owner.balanceIds) == 0 {
					reconciliation, err := model.ReconcileContractAtDeadline(ctx, primary.ContractId, primary.Deadline)
					server.Raise(err)
					primaryClosed = newlyClosed(reconciliation)
					return
				}
				batch, err := model.ReconcileContractOwnerAtDeadline(ctx, owner.balanceIds, primary, dueContractDeadlines(now))
				server.Raise(err)
				primaryClosed = newlyClosed(batch.Reconciliation)
				closed += batch.SiblingsClosed
			}, func(err error) {
				glog.Infof("[close-deadline]owner of %s failed: %v\n", primary.ContractId, err)
			})
			if primaryClosed {
				closed++
				if legacyPrimary {
					legacyClosed++
				}
			}
			// The batch's siblings are due through a queued close or a stored
			// deadline, so it leaves the owner's other legacy contracts open.
			// Each one, and a legacy primary a held owner refused, closes
			// alone and waits for its owner's admission. The first failure
			// leaves the rest open. A contract without a grant had its turn.
			legacyMembers := owner.legacyMembers
			if legacyPrimary && (primaryClosed || len(owner.balanceIds) == 0) {
				legacyMembers = legacyMembers[1:]
			}
			if len(legacyMembers) != 0 {
				server.HandleError(func() {
					legacyIds := make([]server.Id, 0, len(legacyMembers))
					for _, member := range legacyMembers {
						legacyIds = append(legacyIds, member.ContractId)
					}
					openIds, err := model.ReadOpenContractIds(ctx, legacyIds)
					server.Raise(err)
					openIdSet := make(map[server.Id]bool, len(openIds))
					for _, openId := range openIds {
						openIdSet[openId] = true
					}
					for _, member := range legacyMembers {
						if !openIdSet[member.ContractId] {
							continue
						}
						reconciliation, err := model.ReconcileContractAtDeadline(ctx, member.ContractId, member.Deadline)
						server.Raise(err)
						if newlyClosed(reconciliation) {
							closed++
							legacyClosed++
						}
					}
				}, func(err error) {
					glog.Infof("[close-deadline]legacy contracts of the owner of %s failed: %v\n", primary.ContractId, err)
				})
			}
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				result.Closed += closed
				result.LegacyClosed += legacyClosed
			}()
		})
		if !complete {
			return false, nil
		}
		openIds, err := model.ReadOpenContractIds(ctx, dueIds)
		if err != nil {
			return false, err
		}
		result.Visited += len(dueIds)
		keptIds := []server.Id{}
		for _, openId := range openIds {
			if pass == contractDeadlinePassFresh || (pass == contractDeadlinePassBacklog && legacyDueIds[openId]) {
				keptIds = append(keptIds, openId)
			} else {
				result.Dropped++
			}
		}
		leave(keptIds)
		leave(notDueIds)
		return true, nil
	}

	// Visits the span newest first. Returns the remaining upper bound and
	// whether the span is done.
	walk := func(span ContractDeadlineSpan, pass contractDeadlinePass) (upper server.Id, done bool, returnErr error) {
		upper = span.Upper
		for more() {
			readCtx, readCancel := context.WithTimeout(ctx, contractDeadlineCloseReadTimeout)
			page, err := model.ReadOpenContractDeadlines(readCtx, span.Lower, upper, contractDeadlineClosePageSize, true)
			readCancel()
			if err != nil {
				return upper, false, err
			}
			if len(page) == 0 {
				return upper, true, nil
			}
			complete, err := visit(page, pass)
			if err != nil || !complete {
				return upper, false, err
			}
			upper = page[len(page)-1].ContractId
			if len(page) < contractDeadlineClosePageSize {
				return upper, true, nil
			}
		}
		return upper, false, nil
	}

	// Positions only cover complete pages, so a failed read stops the run and
	// the successor retries the rest after the idle delay.
	runErr := func() error {
		if next.Fresh.Cmp(edge) < 0 {
			upper, done, err := walk(ContractDeadlineSpan{Lower: next.Fresh, Upper: edge}, contractDeadlinePassFresh)
			if !done {
				// The unvisited part of the new deadlines waits for a retry.
				leaveSpan(next.Fresh, upper)
				result.Exhausted = err == nil
			}
			next.Fresh = edge
			if err != nil {
				return err
			}
		}
		passes := []struct {
			span **ContractDeadlineSpan
			kind contractDeadlinePass
		}{{span: &next.Retry, kind: contractDeadlinePassRetry}, {span: &next.Backlog, kind: contractDeadlinePassBacklog}}
		for _, pass := range passes {
			if *pass.span == nil {
				continue
			}
			upper, done, err := walk(**pass.span, pass.kind)
			if done {
				*pass.span = nil
			} else {
				(*pass.span).Upper = upper
				result.Exhausted = err == nil
			}
			if err != nil {
				return err
			}
		}
		return nil
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runErr != nil {
		glog.Infof("[close-deadline]run stopped: %v\n", runErr)
	}
	glog.Infof("[close-deadline]visited=%d closed=%d legacy_closed=%d dropped=%d exhausted=%t\n",
		result.Visited, result.Closed, result.LegacyClosed, result.Dropped, result.Exhausted)
	return result, nil
}
