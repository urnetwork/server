package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

const LegacySettlementShardCount = 16

var errLegacySettlementPending = errors.New("legacy settlement is durably pending")
var errLegacySettlementPageBudget = errors.New("legacy settlement page budget elapsed")

// The fixed pass cutoff lets skipped owners return after a finite cohort.
// It bounds traversal growth, not elapsed time or the existing cohort size.
type LegacySettlementCursor struct {
	NextAttemptTime time.Time `json:"next_attempt_time"`
	ContractId      server.Id `json:"contract_id"`
	PassEndTime     time.Time `json:"pass_end_time,omitzero"`
}

type LegacySettlementFlushResult struct {
	Cursor     *LegacySettlementCursor `json:"cursor,omitempty"`
	Visited    int                     `json:"visited"`
	Completed  int                     `json:"completed"`
	BusyOrGone int                     `json:"busy_or_gone"`
	Failed     int                     `json:"failed"`
	More       bool                    `json:"more"`
	// Head outcomes are included in the total counts, not extra visits.
	HeadVisited    int `json:"head_visited"`
	HeadCompleted  int `json:"head_completed"`
	HeadBusyOrGone int `json:"head_busy_or_gone"`
	HeadFailed     int `json:"head_failed"`
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
// the intent untouched. Exact debit, payout, outcome, metadata and intent deletion share
// one commit; a lost commit acknowledgement can never repeat consumption.
func flushLegacySettlementInTx(ctx context.Context, tx server.PgTx, contractId server.Id) (posts []func() any, completed, busy bool, returnErr error) {
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
		return nil, false, true, nil
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
		return nil, false, true, nil
	}
	if terminal {
		// The schema guard makes this impossible for ordinary writers. Do
		// not guess whether an external repair completed required payouts.
		return nil, false, false, fmt.Errorf("legacy settlement intent has a terminal contract")
	}
	{
		var expected int
		server.Raise(tx.QueryRow(ctx, legacySettlementExpectedGrantCountSQL, contractId).Scan(&expected))
		locked := 0
		rows, err = tx.Query(ctx, `SELECT balance.balance_id FROM transfer_balance AS balance
          INNER JOIN transfer_escrow AS escrow USING(balance_id) WHERE escrow.contract_id=$1
          ORDER BY balance.balance_id FOR UPDATE OF balance SKIP LOCKED`, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				locked++
			}
		})
		if locked != expected {
			// Only joined grants can be locked. The original settlement core
			// decides whether their actual amounts fund usage; an absent,
			// unused legacy grant must not become a new accounting refusal.
			return nil, false, true, nil
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
			return nil, false, false, fmt.Errorf("legacy worker did not claim locked open contract")
		}
	}
	// Only best-effort projections with separate recovery/expiry remain after commit.
	posts = append(posts, func() any { RemoveFromStream(ctx, contractId); return nil })
	return posts, true, false, nil
}

func flushLegacySettlement(ctx context.Context, contractId server.Id) (completed, busy bool, returnErr error) {
	var posts []func() any
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
			var err error
			posts, completed, busy, err = flushLegacySettlementInTx(ctx, tx, contractId)
			server.Raise(err)
		}, server.TxReadCommitted, server.OptNoRetry())
		server.RunPosts(ctx, posts...)
	}, func(err error) { returnErr = err })
	return
}

// The first selection fixes a database-clock cutoff for this traversal. New due
// arrivals cannot extend its tail forever and starve earlier busy intents. Each
// statement retains an indexable due bound; a volatile clock would scan future
// rows even with LIMIT 1. Failed accounting stays visible, reserved and deferred.
// Each task owns a bounded page and persists the cursor and fixed pass cutoff.
// A continued page gives up to one quarter of its slots to distinct older heads,
// with one early retry and three forward visits between later retries. Its local
// head cursor skips busy owners without rewinding the persisted forward cursor.
// Exhausting this page's own budget after progress yields its completed prefix;
// it must not turn durable per-contract progress into a task-wide error backoff.
func FlushLegacySettlements(ctx context.Context, shard int, after *LegacySettlementCursor, limit int) (result LegacySettlementFlushResult, returnErr error) {
	if shard < 0 || shard >= LegacySettlementShardCount || limit < 1 || limit > 64 {
		return result, fmt.Errorf("invalid legacy settlement limit")
	}
	bounded, cancel := context.WithTimeoutCause(ctx, 15*time.Second, errLegacySettlementPageBudget)
	defer cancel()
	contextDone := func(err error) bool {
		// The database owner deliberately replaces interrupted connection
		// failures with this exact sentinel after joining its cleanup.
		return bounded.Err() != nil && (err == server.DbContextDoneError || errors.Is(err, bounded.Err()))
	}
	pageBudgetExceeded := func(err error) bool {
		return ctx.Err() == nil && context.Cause(bounded) == errLegacySettlementPageBudget &&
			contextDone(err)
	}
	server.HandleError(func() {
		headCursor := after
		var headAfter *LegacySettlementCursor
		headRemaining := 0
		if headCursor != nil && limit > 1 {
			headRemaining = max(1, limit/4)
		}
		forwardUntilHead := 1
		for remaining := limit; remaining > 0; {
			visitHead := headRemaining > 0 && forwardUntilHead == 0
			var next *LegacySettlementCursor
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
                      AND (next_attempt_time,contract_id)<=($2,$3) ORDER BY next_attempt_time,contract_id LIMIT 1`
						if headAfter != nil {
							query = `SELECT next_attempt_time,contract_id,COALESCE($4::timestamp,statement_timestamp() AT TIME ZONE 'UTC') FROM legacy_settlement_intent
                      WHERE shard=$1 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'
                      AND next_attempt_time<=COALESCE($4::timestamp,statement_timestamp() AT TIME ZONE 'UTC')
                      AND (next_attempt_time,contract_id)<=($2,$3) AND (next_attempt_time,contract_id)>($5,$6)
                      ORDER BY next_attempt_time,contract_id LIMIT 1`
							args = append(args, headAfter.NextAttemptTime, headAfter.ContractId)
						}
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
			if next == nil {
				if visitHead {
					headRemaining = 0
					continue
				}
				result.Cursor = nil
				return
			}
			completed, busy, err := flushLegacySettlement(bounded, next.ContractId)
			if contextDone(err) {
				// The current transaction may have rolled back or its commit
				// acknowledgement may be unknown. Retain the previous cursor;
				// replay still takes the existing intent/outcome ownership guard.
				server.Raise(err)
			}
			result.Visited++
			remaining--
			if visitHead {
				result.HeadVisited++
				headRemaining--
				headAfter = next
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
					result.Cursor = next
				}
				return
			}
			if !visitHead {
				result.Cursor = next
				after = next
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
