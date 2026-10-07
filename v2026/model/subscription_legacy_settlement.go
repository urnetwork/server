package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

const LegacySettlementShardCount = 16

// A larger row ceiling amortizes dispatch while the existing fifteen-second
// context still bounds every page. Each contract retains its own transaction.
const LegacySettlementPageLimit = 256

// Preserve one bounded grant-wait opportunity per sixteen head visits, including
// when a page continues beyond the former sixty-four-visit ceiling.
const legacySettlementHeadGrantWaitStride = 16

var errLegacySettlementPending = errors.New("legacy settlement is durably pending")
var errLegacySettlementPageBudget = errors.New("legacy settlement page budget elapsed")
var errLegacySettlementGrantWaitBusy = errors.New("legacy settlement bounded grant wait elapsed")

// Local query-stage evidence; it is recorded only after the attempt returns.
type legacySettlementGrantWait struct {
	attempted bool
	timedOut  bool
}

// A skipped owner identifies a gate, not a live lock census. Grant membership
// can also change between the count and lock statements' ReadCommitted snapshots.
type legacySettlementBusyGate int

const (
	legacySettlementBusyNone legacySettlementBusyGate = iota
	legacySettlementBusyIntent
	legacySettlementBusyContract
	legacySettlementBusyGrantSet
)

// The fixed pass cutoff lets skipped owners return after a finite cohort.
// It bounds traversal growth, not elapsed time or the existing cohort size.
type LegacySettlementCursor struct {
	NextAttemptTime time.Time                 `json:"next_attempt_time"`
	ContractId      server.Id                 `json:"contract_id"`
	PassEndTime     time.Time                 `json:"pass_end_time,omitzero"`
	HeadAfter       *LegacySettlementPosition `json:"head_after,omitempty"`
}

// The head revisit position is independent of the forward pass. Persisting it
// prevents a permanently busy prefix from consuming every page's head share.
type LegacySettlementPosition struct {
	NextAttemptTime time.Time `json:"next_attempt_time"`
	ContractId      server.Id `json:"contract_id"`
}

type LegacySettlementFlushResult struct {
	Cursor     *LegacySettlementCursor  `json:"cursor,omitempty"`
	Visited    int                      `json:"visited"`
	Completed  int                      `json:"completed"`
	BusyOrGone int                      `json:"busy_or_gone"`
	Failed     int                      `json:"failed"`
	More       bool                     `json:"more"`
	Timings    *LegacySettlementTimings `json:"timings,omitempty"`
	// Head outcomes are included in the total counts, not extra visits.
	HeadVisited    int `json:"head_visited"`
	HeadCompleted  int `json:"head_completed"`
	HeadBusyOrGone int `json:"head_busy_or_gone"`
	HeadFailed     int `json:"head_failed"`
	// The three gates partition busy visits; head counts remain total subsets.
	BusyIntentUnavailable       int `json:"busy_intent_unavailable"`
	BusyContractUnavailable     int `json:"busy_contract_unavailable"`
	BusyGrantSetMismatch        int `json:"busy_grant_set_mismatch"`
	HeadBusyIntentUnavailable   int `json:"head_busy_intent_unavailable"`
	HeadBusyContractUnavailable int `json:"head_busy_contract_unavailable"`
	HeadBusyGrantSetMismatch    int `json:"head_busy_grant_set_mismatch"`
	// One of each sixteen head visits may join the grant queue. These are head
	// subsets, not additional visits; absent fields in earlier results are unknown.
	HeadGrantWaitAttempted int `json:"head_grant_wait_attempted"`
	HeadGrantWaitCompleted int `json:"head_grant_wait_completed"`
	HeadGrantWaitTimedOut  int `json:"head_grant_wait_timed_out"`
}

// The caller owns the contract row. Neither admission debt nor outcome changes
// here: an acknowledgement means durable work is queued, not that usage settled.
// A different requested outcome cannot replace an already accepted intent.
func queueLegacySettlementInTx(ctx context.Context, tx server.PgTx, contractId server.Id, outcome ContractOutcome, clearDispute bool) error {
	switch outcome {
	case ContractOutcomeSettled, ContractOutcomeDisputeResolvedToSource, ContractOutcomeDisputeResolvedToDestination:
	default:
		return fmt.Errorf("unknown legacy settlement outcome")
	}
	var open bool
	server.Raise(tx.QueryRow(ctx, `SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&open))
	if !open {
		return nil
	}
	tag := server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute)
      VALUES($1,$2,$3,$4) ON CONFLICT (contract_id) DO UPDATE
      SET clear_dispute=legacy_settlement_intent.clear_dispute OR EXCLUDED.clear_dispute
      WHERE legacy_settlement_intent.outcome=EXCLUDED.outcome`, contractId, int(contractId[15])%LegacySettlementShardCount, outcome, clearDispute))
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("legacy settlement intent outcome conflicts")
	}
	return nil
}

// Count only this contract's joined grants without a parallel startup driven
// by distorted historical cardinality. Missing grants keep the original inner-
// join semantics. The subsequent sorted SKIP LOCKED ownership check is unchanged.
const legacySettlementExpectedGrantCountSQL = `SELECT count(*)
 FROM unnest(ARRAY[$1::uuid]) AS requested_contract(contract_id)
 CROSS JOIN LATERAL (SELECT balance_id FROM transfer_escrow
   WHERE contract_id=requested_contract.contract_id OFFSET 0) AS escrow
 CROSS JOIN LATERAL (SELECT 1 FROM transfer_balance
   WHERE balance_id=escrow.balance_id OFFSET 0) AS balance`

// No ownership is inferred from a statement snapshot. Both the queue item and
// contract, then every grant, are acquired with SKIP LOCKED. Busy owners leave
// the intent untouched. Exact debit, payout, outcome, metadata, total-projection ownership and intent
// deletion share one commit; a lost commit acknowledgement can never repeat consumption.
func flushLegacySettlementInTx(ctx context.Context, tx server.PgTx, contractId server.Id) (posts []func() any, completed, busy bool, busyGate legacySettlementBusyGate, returnErr error) {
	return flushLegacySettlementWithGrantWaitInTx(ctx, tx, contractId, nil)
}

// A nonnil wait is reserved for an allocated head grant preflight. All other
// ownership gates and the financial transaction retain their existing behavior.
func flushLegacySettlementWithGrantWaitInTx(ctx context.Context, tx server.PgTx, contractId server.Id, wait *legacySettlementGrantWait) (posts []func() any, completed, busy bool, busyGate legacySettlementBusyGate, returnErr error) {
	var outcome ContractOutcome
	var clearDispute bool
	var found bool
	rows, err := tx.Query(ctx, `SELECT outcome,clear_dispute FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE SKIP LOCKED`, contractId)
	server.WithPgResult(rows, err, func() {
		if rows.Next() {
			server.Raise(rows.Scan(&outcome, &clearDispute))
			found = true
		}
	})
	if !found {
		return nil, false, true, legacySettlementBusyIntent, nil
	}
	var terminal bool
	found = false
	rows, err = tx.Query(ctx, `SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1 FOR UPDATE SKIP LOCKED`, contractId)
	server.WithPgResult(rows, err, func() {
		if rows.Next() {
			server.Raise(rows.Scan(&terminal))
			found = true
		}
	})
	if !found {
		return nil, false, true, legacySettlementBusyContract, nil
	}
	if terminal {
		// The schema guard makes this impossible for ordinary writers. Do
		// not guess whether an external repair completed required payouts.
		return nil, false, false, legacySettlementBusyNone, fmt.Errorf("legacy settlement intent has a terminal contract")
	}
	{
		var expected int
		server.Raise(tx.QueryRow(ctx, legacySettlementExpectedGrantCountSQL, contractId).Scan(&expected))
		locked, err := lockLegacySettlementGrantsInTx(ctx, tx, contractId, wait)
		if err != nil {
			return nil, false, false, legacySettlementBusyNone, err
		}
		if locked != expected {
			// Only joined grants can be locked. The original settlement core
			// decides whether their actual amounts fund usage; an absent,
			// unused legacy grant must not become a new accounting refusal.
			return nil, false, true, legacySettlementBusyGrantSet, nil
		}

		if clearDispute {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=false,close_time=clock_timestamp() AT TIME ZONE 'UTC' WHERE contract_id=$1`, contractId))
		}
		// Deleting the locked intent admits only this transaction through the
		// outcome guard. Every failure restores the intent with the finances.
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, contractId))
		posts, completed, returnErr = settleEscrowWithOptionsInTx(ctx, tx, contractId, outcome, false, true)
		if returnErr != nil {
			return
		}
		if !completed {
			return nil, false, false, legacySettlementBusyNone, fmt.Errorf("legacy worker did not claim locked open contract")
		}
	}
	// Only best-effort projections with separate recovery/expiry remain after commit.
	posts = append(posts, observeLegacySettlementPost(ctx, legacySettlementStream,
		func() any { RemoveFromStream(ctx, contractId); return nil }))
	return posts, true, false, legacySettlementBusyNone, nil
}

// Choose the ownership mode before locking any grant. Retrying a partial
// SKIP LOCKED set in place could invert the sorted grant order. An allocated
// head instead gets one whole-statement wait budget, not a budget per grant row.
func lockLegacySettlementGrantsInTx(ctx context.Context, tx server.PgTx, contractId server.Id, wait *legacySettlementGrantWait) (locked int, returnErr error) {
	query := `SELECT balance.balance_id FROM transfer_balance AS balance
          INNER JOIN transfer_escrow AS escrow USING(balance_id) WHERE escrow.contract_id=$1
	          ORDER BY balance.balance_id FOR UPDATE OF balance`
	if wait == nil {
		query += ` SKIP LOCKED`
	} else {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='250ms'`))
		wait.attempted = true
	}
	rows, err := tx.Query(ctx, query, contractId)
	if err == nil {
		for rows.Next() {
			locked++
		}
		err = rows.Err()
		rows.Close()
	}
	if err != nil {
		if wait != nil && ctx.Err() == nil && legacySettlementGrantWaitExpired(err) {
			wait.timedOut = true
			// The transaction is aborted. The caller must unwind through Tx's
			// rollback before treating this expected ownership refusal as busy.
			return 0, errLegacySettlementGrantWaitBusy
		}
		return 0, err
	}
	if wait != nil {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'`))
	}
	return locked, nil
}

// Classify only errors returned by the bounded grant query, never later DML.
func legacySettlementGrantWaitExpired(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	// Operator cancellation shares 57014 but is not this bounded wait.
	return pgErr.Code == "55P03" || (pgErr.Code == "57014" && pgErr.Message == "canceling statement due to statement timeout")
}

func flushLegacySettlement(ctx context.Context, contractId server.Id) (completed, busy bool, busyGate legacySettlementBusyGate, returnErr error) {
	return flushLegacySettlementWithGrantWait(ctx, contractId, nil)
}

// Expected grant-wait exhaustion crosses the normal rollback boundary before
// becoming a busy result. Cancellation and every other error stay visible.
func flushLegacySettlementWithGrantWait(ctx context.Context, contractId server.Id, wait *legacySettlementGrantWait) (completed, busy bool, busyGate legacySettlementBusyGate, returnErr error) {
	var posts []func() any
	server.HandleError(func() {
		func() {
			defer enterLegacySettlementTiming(ctx, legacySettlementFinancial)()
			defer func() {
				if r := recover(); r != nil {
					if r == errLegacySettlementGrantWaitBusy {
						if ctx.Err() != nil {
							panic(ctx.Err())
						}
						// Tx has already rolled back and released every acquired
						// owner. Do not log expected contention or run any posts.
						posts, completed, busy, busyGate = nil, false, true, legacySettlementBusyGrantSet
						return
					}
					panic(r)
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
				var err error
				posts, completed, busy, busyGate, err = flushLegacySettlementWithGrantWaitInTx(ctx, tx, contractId, wait)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
		}()
		func() {
			defer enterLegacySettlementTiming(ctx, legacySettlementJoinedPosts)()
			server.RunPosts(ctx, posts...)
		}()
	}, func(err error) { returnErr = err })
	return
}

// The first selection fixes a database-clock cutoff for this traversal. New due
// arrivals cannot extend its tail forever and starve earlier busy intents. Each
// statement retains an indexable due bound; a volatile clock would scan future
// rows even with LIMIT 1. Failed accounting stays visible, reserved and deferred.
// Each task owns a bounded page and persists the cursor and fixed pass cutoff.
// A continued page gives up to one quarter of its slots to distinct older heads,
// with one early retry and three forward visits between later retries. Its
// durable head cursor skips busy owners without rewinding the forward cursor.
// Exhausting this page's own budget after progress yields its completed prefix;
// it must not turn durable per-contract progress into a task-wide error backoff.
func FlushLegacySettlements(ctx context.Context, shard int, after *LegacySettlementCursor, limit int) (result LegacySettlementFlushResult, returnErr error) {
	if shard < 0 || shard >= LegacySettlementShardCount || limit < 1 || limit > LegacySettlementPageLimit {
		return result, fmt.Errorf("invalid legacy settlement limit")
	}
	bounded, cancel := context.WithTimeoutCause(ctx, 15*time.Second, errLegacySettlementPageBudget)
	defer cancel()
	observer := &legacySettlementTimingObserver{now: time.Now}
	bounded = context.WithValue(bounded, legacySettlementTimingKey{}, observer)
	result, returnErr = flushLegacySettlementsPage(ctx, bounded, shard, after, limit, flushLegacySettlementWithGrantWait)
	result.Timings = observer.snapshot()
	return
}

// One traversal owns its settlement operation and bounded head grant waits.
// Tests can interrupt after a real committed prefix.
func flushLegacySettlementsPage(ctx, bounded context.Context, shard int, after *LegacySettlementCursor, limit int,
	settle func(context.Context, server.Id, *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error)) (result LegacySettlementFlushResult, returnErr error) {
	pageBudgetExceeded := func(err error) bool {
		return ctx.Err() == nil && bounded.Err() != nil && context.Cause(bounded) == errLegacySettlementPageBudget &&
			isSettlementPageCancellation(err)
	}
	server.HandleError(func() {
		headCursor := after
		var headAfter *LegacySettlementPosition
		if after != nil {
			headAfter = after.HeadAfter
		}
		headCycleBefore := headAfter
		headWrapped := false
		headRemaining := 0
		if headCursor != nil && limit > 1 {
			headRemaining = max(1, limit/4)
		}
		forwardUntilHead := 1
		for remaining := limit; remaining > 0; {
			visitHead := headRemaining > 0 && forwardUntilHead == 0
			var next *LegacySettlementCursor
			func() {
				defer enterLegacySettlementTiming(bounded, legacySettlementSelection)()
				server.Db(bounded, func(conn server.PgConn) {
					query := `SELECT next_attempt_time,contract_id,statement_timestamp() AT TIME ZONE 'UTC' FROM legacy_settlement_intent
                  WHERE shard=$1 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'
                  ORDER BY next_attempt_time,contract_id LIMIT 1`
					args := []any{shard}
					if after != nil {
						// Older task cursors have no cutoff. Establish it once on
						// their first continuation, then preserve it across pages.
						cursor := after
						if visitHead {
							cursor = headCursor
						}
						var passEndTime any
						if !cursor.PassEndTime.IsZero() {
							passEndTime = cursor.PassEndTime
						}
						query = `SELECT next_attempt_time,contract_id,COALESCE($4::timestamp,statement_timestamp() AT TIME ZONE 'UTC') FROM legacy_settlement_intent
                      WHERE shard=$1 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'
                      AND next_attempt_time<=COALESCE($4::timestamp,statement_timestamp() AT TIME ZONE 'UTC')
                      AND (next_attempt_time,contract_id)>($2,$3) ORDER BY next_attempt_time,contract_id LIMIT 1`
						args = append(args, cursor.NextAttemptTime, cursor.ContractId, passEndTime)
						if visitHead {
							query = `SELECT next_attempt_time,contract_id,COALESCE($4::timestamp,statement_timestamp() AT TIME ZONE 'UTC') FROM legacy_settlement_intent
                      WHERE shard=$1 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'
                      AND next_attempt_time<=COALESCE($4::timestamp,statement_timestamp() AT TIME ZONE 'UTC')
                      AND (next_attempt_time,contract_id)<=($2,$3)`
							if headAfter != nil {
								query += ` AND (next_attempt_time,contract_id)>($5,$6)`
								args = append(args, headAfter.NextAttemptTime, headAfter.ContractId)
							}
							if headWrapped {
								// A page can wrap once. Its second segment ends at the
								// original lower bound so no head is visited twice.
								query += fmt.Sprintf(` AND (next_attempt_time,contract_id)<=($%d,$%d)`, len(args)+1, len(args)+2)
								args = append(args, headCycleBefore.NextAttemptTime, headCycleBefore.ContractId)
							}
							query += ` ORDER BY next_attempt_time,contract_id LIMIT 1`
						}
					}
					rows, err := conn.Query(bounded, query, args...)
					server.WithPgResult(rows, err, func() {
						if rows.Next() {
							value := LegacySettlementCursor{}
							server.Raise(rows.Scan(&value.NextAttemptTime, &value.ContractId, &value.PassEndTime))
							next = &value
						}
					})
				})
			}()
			if next == nil {
				if visitHead {
					if headCycleBefore != nil && !headWrapped {
						// Resume after the previous page's head position first,
						// then revisit its earlier busy owners in this same bounded
						// allocation. Empty probes do not consume financial slots.
						headAfter = nil
						headWrapped = true
						result.Cursor.HeadAfter = nil
						continue
					}
					headRemaining = 0
					continue
				}
				result.Cursor = nil
				return
			}
			var grantWait *legacySettlementGrantWait
			if visitHead && result.HeadVisited%legacySettlementHeadGrantWaitStride == 0 {
				grantWait = &legacySettlementGrantWait{}
			}
			completed, busy, busyGate, err := settle(bounded, next.ContractId, grantWait)
			if err != nil && bounded.Err() != nil {
				// The current transaction may have rolled back or its commit
				// acknowledgement may be unknown. Retain the previous cursor;
				// replay still takes the existing intent/outcome ownership guard.
				server.Raise(err)
			}
			result.Visited++
			remaining--
			if visitHead {
				result.HeadVisited++
				if grantWait != nil && grantWait.attempted {
					result.HeadGrantWaitAttempted++
					if completed && err == nil {
						result.HeadGrantWaitCompleted++
					}
					if grantWait.timedOut && err == nil {
						result.HeadGrantWaitTimedOut++
					}
				}
				headRemaining--
				headAfter = &LegacySettlementPosition{NextAttemptTime: next.NextAttemptTime, ContractId: next.ContractId}
				forwardUntilHead = 3
			} else if forwardUntilHead > 0 {
				forwardUntilHead--
			}
			if completed && err == nil {
				result.Completed++
				if visitHead {
					result.HeadCompleted++
				}
			}
			if busy {
				result.BusyOrGone++
				if visitHead {
					result.HeadBusyOrGone++
				}
				switch busyGate {
				case legacySettlementBusyIntent:
					result.BusyIntentUnavailable++
					if visitHead {
						result.HeadBusyIntentUnavailable++
					}
				case legacySettlementBusyContract:
					result.BusyContractUnavailable++
					if visitHead {
						result.HeadBusyContractUnavailable++
					}
				case legacySettlementBusyGrantSet:
					result.BusyGrantSetMismatch++
					if visitHead {
						result.HeadBusyGrantSetMismatch++
					}
				}
			}
			if err != nil {
				result.Failed++
				if visitHead {
					result.HeadFailed++
				}
				code, delay := "operational", 30*time.Second
				if errors.Is(err, errContractInsufficientEscrow) {
					code, delay = "accounting", 15*time.Minute
				}
				server.Tx(bounded, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(bounded, `UPDATE legacy_settlement_intent SET
                      failure_code=$2,next_attempt_time=clock_timestamp() AT TIME ZONE 'UTC'+$3::interval
                      WHERE contract_id=$1`, next.ContractId, code, delay.String()))
				}, server.TxReadCommitted, server.OptNoRetry())
				if !visitHead {
					next.HeadAfter = headAfter
					result.Cursor = next
				} else {
					result.Cursor.HeadAfter = headAfter
				}
				return
			}
			if !visitHead {
				next.HeadAfter = headAfter
				result.Cursor = next
				after = next
			} else {
				result.Cursor.HeadAfter = headAfter
			}
		}
		result.More = true
	}, func(err error) {
		// A failed retry-state write is not a completed visit. Keep that
		// failure visible even if it coincides with the page budget.
		if result.Failed == 0 && result.Visited > 0 && pageBudgetExceeded(err) {
			result.More = true
			return
		}
		returnErr = err
	})
	return
}
