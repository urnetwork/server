// A contract that outlives its deadline keeps its payer's escrow, the Pending
// the apps show, until a closer retires it. The deadline pass and the expiry
// sweep serve every payer from one contract-ordered queue. Under a backlog of
// other payers' contracts, millions on Main in October 2026, a payer's few
// contracts waited there for days: the deadline pass reached a contract only
// as an owner sibling of a later contract on the same grant, so a contract on
// a grant that ended or got no later contract was never reached, and the sweep
// walked the whole backlog oldest first. Their escrow stayed reserved, and a
// payer whose grant they filled was refused new contracts.
//
// The payer drain retires one payer's due contracts directly, a page at a time,
// through the deadline reconciliation the deadline pass uses: each owner's
// members together in one owned turn. The payer's own activity requests it:
// a contract admission its balance refused, once the admission's recovery and
// rerun did not admit it, and a balance read whose Pending exceeds what its
// current grants reserve. A gate in Redis passes one request per payer per
// interval across processes, and the task coalesces by payer, so repeated
// refusals and polls cost one Redis write each. A drain that leaves due
// contracts schedules its successor, so one request retires all of them in
// bounded runs.
//
// Retirement is idempotent and settles once. Each member is reconciled under
// its owner's admission and row locks, a terminal contract is only read, the
// outcome claim is the single terminal transition, and a payout never repeats
// prior consumption (see ReconcileContractAtDeadline). Reconciliation pays
// what the reports, the escrow and the grant's remaining balance support, so
// a short escrow, legacy or Redis-reserved, never leaves a contract open. A
// held owner keeps its members for the next run. Disputed contracts are not
// Pending and stay with the deadline pass, which resolves them.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Tunes the payer drain, its requests and its task.
type PayerDueContractCloseSettings struct {
	// Due contracts one read returns. A read examines about a page of the
	// payer's open contracts, however many the payer holds.
	PageSize int
	// A contract is due this long after its deadline, so a drain does not
	// race a party's final close or the deadline pass at the deadline itself.
	DueMargin time.Duration
	// How long one run keeps starting owner turns; started turns finish.
	RunBudget time.Duration
	// Bounds one read of a page of the payer's open contracts.
	ReadTimeout time.Duration
	// The gate passes one request per payer per interval across processes.
	RequestInterval time.Duration
	// Bounds the gate and the task publication of one request, so neither a
	// refused admission nor a balance read waits longer on them.
	RequestTimeout time.Duration
	// A run that left only members of held owners runs again after this delay
	// instead of at once, so it does not spin against their holders.
	BusyRetryDelay time.Duration
	// The task's maximum run time: the budget, the turns started within it and
	// the read.
	MaxTime time.Duration
}

// The page reads a few thousand rows of one payer's open-contract index; on
// Main the read took 42 ms for a payer the planner put at 13 million
// unresolved contracts (2026-10-28). A minute of turns retires about 4,000
// contracts at the owner batch's rate, and a request every two minutes per
// payer is at most one Redis write per refusal or poll and one queue write
// per interval.
func DefaultPayerDueContractCloseSettings() *PayerDueContractCloseSettings {
	return &PayerDueContractCloseSettings{
		PageSize:        4096,
		DueMargin:       time.Minute,
		RunBudget:       time.Minute,
		ReadTimeout:     20 * time.Second,
		RequestInterval: 2 * time.Minute,
		RequestTimeout:  2 * time.Second,
		BusyRetryDelay:  30 * time.Second,
		MaxTime:         5 * time.Minute,
	}
}

// One drain run. Closed counts the contracts this run retired and
// AlreadyClosed those another closer retired first. Busy counts members a held
// owner kept and Failed members whose turn failed; both stay open. More
// reports due contracts this run did not reach or a held owner kept.
type PayerDueContractClose struct {
	Due           int  `json:"due"`
	Closed        int  `json:"closed"`
	AlreadyClosed int  `json:"already_closed,omitempty"`
	Busy          int  `json:"busy,omitempty"`
	Failed        int  `json:"failed,omitempty"`
	More          bool `json:"more,omitempty"`
}

// The read of ReadPayerDueContracts, apart so its plan can be tested:
// $1 the payer, $2 the due bound, $3 the due bound less
// DefaultContractExpiration for legacy rows, $4 the page. It asks for no
// order, so it stops after a page of the payer's open-contract index. Ordered
// by contract_id, PostgreSQL had to sort every due contract of the payer, or
// walk the primary key from the oldest contract of every payer until it met a
// page of the payer's. On Main, with payers holding millions of unresolved
// contracts, it walked, and the read outlived ReadTimeout on every run
// (2026-10-28).
const payerDueContractsSql = `SELECT contract_id,create_time,expiration_time FROM transfer_contract
	WHERE payer_network_id=$1 AND (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
		AND (expiration_time<=$2 OR (expiration_time IS NULL AND create_time<=$3))
	LIMIT $4`

// A page of the payer's undisputed open contracts whose deadline passed at
// dueBefore, at most limit, in contract order, each with its deadline: the
// stored one, or for a legacy row without one its creation plus
// DefaultContractExpiration, the deadline the closers give it. The page is
// any limit of the due contracts, not the oldest; the drain reads again until
// none is due. The read uses the payer's open-contract index, as the Pending
// sum does, and checks each deadline in the heap.
func ReadPayerDueContracts(ctx context.Context, payerNetworkId server.Id, dueBefore time.Time, limit int) (deadlines []ContractDeadline, returnErr error) {
	if limit < 1 {
		return nil, errors.New("invalid payer due contract page")
	}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, payerDueContractsSql,
				payerNetworkId, dueBefore.UTC(), dueBefore.UTC().Add(-DefaultContractExpiration), limit)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contract OpenContractDeadline
					server.Raise(rows.Scan(&contract.ContractId, &contract.CreateTime, &contract.ExpirationTime))
					deadline, due := contract.Due(dueBefore)
					if !due {
						deadline, due = contract.LegacyDue(dueBefore)
					}
					if due {
						deadlines = append(deadlines, deadline)
					}
				}
			})
		}, server.OptNoRetry())
		slices.SortFunc(deadlines, func(a ContractDeadline, b ContractDeadline) int {
			return a.ContractId.Cmp(b.ContractId)
		})
	}, func(err error) { deadlines, returnErr = nil, err })
	return
}

// What a dry run reports for one payer: its due contracts, the bytes they hold
// and the deadlines of the oldest and the newest.
type PayerDueContractSummary struct {
	Count          int
	ByteCount      ByteCount
	OldestDeadline *time.Time
	NewestDeadline *time.Time
}

// Summarizes the contracts ReadPayerDueContracts would return without a limit.
func SummarizePayerDueContracts(ctx context.Context, payerNetworkId server.Id, dueBefore time.Time) (summary *PayerDueContractSummary, returnErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			summary = &PayerDueContractSummary{}
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(sum(transfer_byte_count),0),
					min(COALESCE(expiration_time,create_time+make_interval(secs => $4))),max(COALESCE(expiration_time,create_time+make_interval(secs => $4)))
				FROM transfer_contract
				WHERE payer_network_id=$1 AND (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
					AND (expiration_time<=$2 OR (expiration_time IS NULL AND create_time<=$3))`,
				payerNetworkId, dueBefore.UTC(), dueBefore.UTC().Add(-DefaultContractExpiration), DefaultContractExpiration.Seconds()).Scan(
				&summary.Count, &summary.ByteCount, &summary.OldestDeadline, &summary.NewestDeadline))
		}, server.OptNoRetry())
	}, func(err error) { summary, returnErr = nil, err })
	return
}

// Names the set of contracts ReadPayerDueContracts would return without a
// limit: the payer, the bound and every contract id. bringyourctl contracts
// close-due commits only the set its dry run reported. Contracts due by
// dueBefore can only leave the set, so a digest that no longer matches means
// another closer retired some meanwhile.
func DigestPayerDueContracts(ctx context.Context, payerNetworkId server.Id, dueBefore time.Time) (count int, digest string, returnErr error) {
	var idsDigest *string
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*), md5(string_agg(contract_id::text, ',' ORDER BY contract_id))
				FROM transfer_contract
				WHERE payer_network_id=$1 AND (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
					AND (expiration_time<=$2 OR (expiration_time IS NULL AND create_time<=$3))`,
				payerNetworkId, dueBefore.UTC(), dueBefore.UTC().Add(-DefaultContractExpiration)).Scan(&count, &idsDigest))
		}, server.OptNoRetry())
	}, func(err error) { count, returnErr = 0, err })
	if returnErr != nil {
		return 0, "", returnErr
	}
	ids := ""
	if idsDigest != nil {
		ids = *idsDigest
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("payer-due-contracts-v1|%s|%s|%d|%s",
		payerNetworkId, dueBefore.UTC().Format(time.RFC3339Nano), count, ids)))
	return count, hex.EncodeToString(sum[:8]), nil
}

// Retires the payer's contracts that were due DueMargin before now, from one
// page. Members of one owner, the escrow balances they reserve from, close
// together in owned turns of at most ContractDeadlineOwnerBatchLimit; a
// contract without escrow closes alone.
// Owners take their turns in the order of their oldest member. more reports
// whether another turn may start. A held owner keeps its members, and a turn
// that failed inside its members retries each one alone, as the deadline
// pass's sibling batch does. Committed turns stay committed when a later one
// fails. The returned error joins the failures of a run that closed nothing.
func ClosePayerDueContracts(ctx context.Context, payerNetworkId server.Id, now time.Time, more func() bool,
	settings *PayerDueContractCloseSettings) (*PayerDueContractClose, error) {
	result := &PayerDueContractClose{}
	readCtx, readCancel := context.WithTimeout(ctx, settings.ReadTimeout)
	due, err := ReadPayerDueContracts(readCtx, payerNetworkId, now.Add(-settings.DueMargin), settings.PageSize)
	readCancel()
	if err != nil {
		return nil, err
	}
	result.Due = len(due)
	result.More = len(due) == settings.PageSize
	if len(due) == 0 {
		return result, nil
	}
	contractIds := make([]server.Id, 0, len(due))
	for _, deadline := range due {
		contractIds = append(contractIds, deadline.ContractId)
	}
	custody, err := ReadContractDeadlineCustody(ctx, contractIds)
	if err != nil {
		return nil, err
	}

	// Owners in the order of their oldest member, since the page is in contract order.
	type payerDueOwner struct {
		balanceIds []server.Id
		members    []ContractDeadline
	}
	owners := []*payerDueOwner{}
	keyOwners := map[string]*payerDueOwner{}
	for _, deadline := range due {
		state := custody[deadline.ContractId]
		if state == nil || !state.Found || state.Outcome != nil {
			result.AlreadyClosed++
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
			owner = &payerDueOwner{balanceIds: state.BalanceIds}
			keyOwners[key] = owner
			owners = append(owners, owner)
		}
		owner.members = append(owner.members, deadline)
	}

	var failures error
	count := func(reconciliation *ContractDeadlineReconciliation) {
		if reconciliation != nil && !reconciliation.AlreadyClosed && !reconciliation.Missing {
			result.Closed++
		} else {
			result.AlreadyClosed++
		}
	}
	fail := func(members int, err error) {
		result.Failed += members
		failures = errors.Join(failures, err)
	}
	// Closes the owner's members from its first, and reports whether another
	// owner may take a turn.
	closeOwner := func(balanceIds []server.Id, members []ContractDeadline) bool {
		if len(balanceIds) == 0 {
			// No grant reserves for it, so no owner can be held.
			if !more() {
				result.More = true
				return false
			}
			member := members[0]
			reconciliation, err := ReconcileContractAtDeadline(ctx, member.ContractId, member.Deadline)
			if err != nil {
				fail(1, fmt.Errorf("contract %s: %w", member.ContractId, err))
				return true
			}
			count(reconciliation)
			return true
		}
		for offset := 0; offset < len(members); offset += ContractDeadlineOwnerBatchLimit {
			if !more() {
				result.More = true
				return false
			}
			batch := members[offset:min(offset+ContractDeadlineOwnerBatchLimit, len(members))]
			results, busy, entered, err := reconcileContractOwnerMembersAtDeadline(ctx, balanceIds, batch)
			if busy {
				// The holder keeps the rest of the owner for the next run.
				result.Busy += len(members) - offset
				result.More = true
				return true
			}
			if err != nil && !entered {
				// The route or the admission failed before any member, as
				// each member alone would: the owner's members stay open.
				fail(len(members)-offset, fmt.Errorf("owner of %s: %w", batch[0].ContractId, err))
				return true
			}
			if err != nil {
				// A failed shared turn acknowledged no member. Each one closes
				// alone and finds closed any member a lost reply did retire.
				for index, member := range batch {
					results, busy, _, err := reconcileContractOwnerMembersAtDeadline(ctx, balanceIds, []ContractDeadline{member})
					if busy {
						result.Busy += len(members) - offset - index
						result.More = true
						return true
					}
					if err != nil {
						fail(1, fmt.Errorf("contract %s: %w", member.ContractId, err))
						continue
					}
					count(results[member.ContractId])
				}
				continue
			}
			for _, member := range batch {
				count(results[member.ContractId])
			}
		}
		return true
	}
	for _, owner := range owners {
		if !closeOwner(owner.balanceIds, owner.members) {
			break
		}
	}
	if failures != nil && result.Closed == 0 {
		return result, failures
	}
	if failures != nil {
		glog.Infof("[payer-due-close]%s left %d failed contracts open: %v\n", payerNetworkId, result.Failed, failures)
	}
	return result, nil
}

// The Redis gate of one payer's requests.
func payerDueContractCloseGateKey(payerNetworkId server.Id) string {
	return fmt.Sprintf("payer_due_contract_close_%s", payerNetworkId)
}

// One pending drain per payer; a request during a run owns its successor.
func payerDueContractCloseRunOnce(payerNetworkId server.Id) *task.RunOnceOption {
	return task.RunOnce("close_payer_due_contracts", payerNetworkId)
}

// Requests a drain of the payer's due contracts. The gate passes one request
// per payer per RequestInterval across processes; a request it holds returns
// false, as does one the gate or the queue could not take within
// RequestTimeout. A failed publication reopens the gate, and the next request
// retries. The caller never sees an error, and must hold no PostgreSQL or
// Redis connection: the request takes one of each in turn.
func RequestPayerDueContractClose(ctx context.Context, payerNetworkId server.Id) bool {
	return requestPayerDueContractClose(ctx, payerNetworkId, DefaultPayerDueContractCloseSettings())
}

// Requests the payer's drain after an admission refused it for its balance.
// Only the admission's final verdict counts: a refusal its recovery and rerun
// turned into a contract asks for nothing. Contracts of the payer that
// outlived their deadline can hold the credit the request lacked until a
// closer reaches them. The admission has released its connections by now, and
// the request does not end with a caller that gave up.
func requestPayerDueContractCloseAfterRefusal(ctx context.Context, payerNetworkId server.Id, err error) {
	if errors.Is(err, ErrContractBalanceExhausted) {
		RequestPayerDueContractClose(context.WithoutCancel(ctx), payerNetworkId)
	}
}

func requestPayerDueContractClose(ctx context.Context, payerNetworkId server.Id, settings *PayerDueContractCloseSettings) (requested bool) {
	if payerNetworkId == (server.Id{}) {
		return false
	}
	requestCtx, cancel := context.WithTimeout(ctx, settings.RequestTimeout)
	defer cancel()
	admitted := false
	server.HandleError(func() {
		server.Redis(requestCtx, func(r server.RedisClient) {
			var err error
			admitted, err = r.SetNX(requestCtx, payerDueContractCloseGateKey(payerNetworkId), "1", settings.RequestInterval).Result()
			server.Raise(err)
		})
		if !admitted {
			return
		}
		owner := session.NewLocalClientSession(requestCtx, "", nil)
		defer owner.Cancel()
		server.Tx(requestCtx, func(tx server.PgTx) {
			schedulePayerDueContractCloseInTx(tx, owner, payerNetworkId, server.NowUtc(), settings)
		}, server.OptNoRetry())
		requested = true
	}, func(err error) {
		glog.Infof("[payer-due-close]request for %s dropped: %v\n", payerNetworkId, err)
	})
	if admitted && !requested {
		server.HandleError(func() {
			reopenCtx, reopenCancel := context.WithTimeout(context.WithoutCancel(ctx), settings.RequestTimeout)
			defer reopenCancel()
			server.Redis(reopenCtx, func(r server.RedisClient) {
				r.Del(reopenCtx, payerDueContractCloseGateKey(payerNetworkId))
			})
		})
	}
	return
}

func schedulePayerDueContractCloseInTx(tx server.PgTx, clientSession *session.ClientSession, payerNetworkId server.Id,
	runAt time.Time, settings *PayerDueContractCloseSettings) {
	task.ScheduleTaskInTx(
		tx,
		ClosePayerDueContractsTask,
		&ClosePayerDueContractsArgs{PayerNetworkId: payerNetworkId},
		clientSession,
		payerDueContractCloseRunOnce(payerNetworkId),
		task.RunAt(runAt),
		task.MaxTime(settings.MaxTime),
	)
}

type ClosePayerDueContractsArgs struct {
	PayerNetworkId server.Id `json:"payer_network_id"`
}

// Both workload profiles register this target; the production owner limits
// each worker to one drain at a time. A failed run retries within a minute.
func NewClosePayerDueContractsTaskTarget() task.Target {
	return task.WithErrorRetryCap(task.NewTaskTargetWithPost(ClosePayerDueContractsTask, ClosePayerDueContractsTaskPost), time.Minute)
}

// One budgeted drain run of the payer. A failed read retries the run. A run
// whose every turn failed ends the request instead of retrying contracts that
// would fail again; the payer's next request tries them again.
func ClosePayerDueContractsTask(args *ClosePayerDueContractsArgs, clientSession *session.ClientSession) (*PayerDueContractClose, error) {
	settings := DefaultPayerDueContractCloseSettings()
	started := time.Now()
	result, err := ClosePayerDueContracts(clientSession.Ctx, args.PayerNetworkId, server.NowUtc(), func() bool {
		return time.Since(started) < settings.RunBudget
	}, settings)
	if result == nil {
		return nil, err
	}
	if err != nil {
		glog.Infof("[payer-due-close]%s closed nothing, %d failed: %v\n", args.PayerNetworkId, result.Failed, err)
		result.More = false
	}
	glog.Infof("[payer-due-close]%s due=%d closed=%d already_closed=%d busy=%d failed=%d more=%t\n",
		args.PayerNetworkId, result.Due, result.Closed, result.AlreadyClosed, result.Busy, result.Failed, result.More)
	return result, nil
}

// A run that left due contracts schedules its successor: at once after
// progress, after BusyRetryDelay when only held owners kept them.
func ClosePayerDueContractsTaskPost(args *ClosePayerDueContractsArgs, result *PayerDueContractClose,
	clientSession *session.ClientSession, tx server.PgTx) error {
	if !result.More {
		return nil
	}
	settings := DefaultPayerDueContractCloseSettings()
	runAt := server.NowUtc()
	if result.Closed == 0 && 0 < result.Busy {
		runAt = runAt.Add(settings.BusyRetryDelay)
	}
	schedulePayerDueContractCloseInTx(tx, clientSession, args.PayerNetworkId, runAt, settings)
	return nil
}
