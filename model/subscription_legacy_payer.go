// Payer turns bound cross-account service independently of contract density.
package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

const legacySettlementPayerProbeLimit = 16
const legacySettlementPayerRegistrationLimit = 256

// A round visits each indexed payer at most once. Its upper key and due cutoff
// are fixed by the database. New payer keys beyond End wait for the next
// round; registrations within the remaining key range may join this one. The
// chronological cursor still covers every intent within a payer.
type LegacySettlementPayerCursor struct {
	After       *server.Id `json:"after,omitempty"`
	End         server.Id  `json:"end"`
	PassEndTime time.Time  `json:"pass_end_time"`
}

// Payer outcomes are subsets of the aggregate financial counts. Registration
// and discovery are scheduling work only and never assert a financial attempt.
type LegacySettlementShardResult struct {
	LegacySettlementFlushResult
	PayerCursor             *LegacySettlementPayerCursor `json:"payer_cursor,omitempty"`
	PayerProbes             int                          `json:"payer_probes"`
	PayerVisited            int                          `json:"payer_visited"`
	PayerCompleted          int                          `json:"payer_completed"`
	PayerBusyOrGone         int                          `json:"payer_busy_or_gone"`
	PayerFailed             int                          `json:"payer_failed"`
	PayerRegistered         int                          `json:"payer_registered"`
	PayerRegistrationFailed bool                         `json:"payer_registration_failed"`
	PayerRegistrationMs     int64                        `json:"payer_registration_ms"`
}

// The recurring shard task owns both traversals under one 15-second budget and
// one financial-visit ceiling. Up to sixteen indexed payer discoveries precede
// the chronological/head remainder. Even a huge healthy or held payer prefix
// therefore consumes one fairness turn, not thousands. The original ordered
// entry remains available to callers that specifically resume that traversal.
func FlushLegacySettlementShard(ctx context.Context, shard int, after *LegacySettlementCursor,
	payerAfter *LegacySettlementPayerCursor, limit int) (result LegacySettlementShardResult, returnErr error) {
	if shard < 0 || shard >= LegacySettlementShardCount || limit < 1 || limit > LegacySettlementPageLimit {
		return result, fmt.Errorf("invalid legacy settlement limit")
	}
	trace, _ := ctx.Value(legacyTargetTraceKey{}).(*legacyTargetTrace)
	if trace == nil {
		trace = legacyTargetTraceRuntimeState.begin(shard, after, "automatic_page", nil)
	}
	if trace != nil {
		ctx = context.WithValue(ctx, legacyTargetTraceKey{}, trace)
	}
	bounded, cancel := context.WithTimeoutCause(ctx, 15*time.Second, errLegacySettlementPageBudget)
	defer cancel()
	observer := &legacySettlementTimingObserver{now: time.Now}
	bounded = context.WithValue(bounded, legacySettlementTimingKey{}, observer)
	result, returnErr = flushLegacySettlementShardPage(ctx, bounded, shard, after, payerAfter, limit)
	result.Timings = observer.snapshot()
	result.Trace = trace.finish(result.LegacySettlementFlushResult, returnErr)
	return
}

// Keep the last completed prefix when this page's own deadline ends. Parent
// cancellation still returns an error; unknown transactions never advance either
// cursor. No input cursor is mutated, including a failure after a payer commit.
func flushLegacySettlementShardPage(ctx, bounded context.Context, shard int, after *LegacySettlementCursor,
	payerAfter *LegacySettlementPayerCursor, limit int) (result LegacySettlementShardResult, returnErr error) {
	bounded, finishPosts := withLegacySettlementPostBatch(bounded)
	defer finishPosts()
	result.Cursor = after
	if payerAfter != nil {
		cursor := *payerAfter
		result.PayerCursor = &cursor
	}
	server.HandleError(func() {
		if !legacySettlementPayerIndexesReady(bounded) {
			// An unfinished online migration disables only payer scheduling.
			// Reuse this page's budget and post owner; retain its dormant payer
			// cursor without manufacturing more chronological work at eof.
			ordered, err := flushLegacySettlementsPage(ctx, bounded, shard, after, limit, flushLegacySettlementWithGrantWait, flushLegacySettlementCohort)
			result.LegacySettlementFlushResult = ordered
			if err != nil && ordered.Visited == 0 {
				result.Cursor = after
			}
			server.Raise(err)
			return
		}
		registeredAt := time.Now()
		var registrationErr error
		server.HandleError(func() {
			result.PayerRegistered = registerLegacySettlementPayers(bounded, shard)
		}, func(err error) { registrationErr = err })
		if registrationErr != nil {
			if bounded.Err() != nil {
				server.Raise(registrationErr)
			}
			// Optional registration cannot suppress existing financial work.
			// Its own transaction has rolled back before this fallback.
			result.PayerRegistrationFailed = true
		}
		result.PayerRegistrationMs = time.Since(registeredAt).Milliseconds()
		probeLimit := min(legacySettlementPayerProbeLimit, limit/4)
		if probeLimit > 0 {
			if result.PayerCursor == nil {
				result.PayerCursor = beginLegacySettlementPayerRound(bounded, shard)
			}
			for probe := 0; probe < probeLimit && result.PayerCursor != nil; probe++ {
				payer, next := nextLegacySettlementPayer(bounded, shard, result.PayerCursor)
				if payer == nil {
					result.PayerCursor = nil
					break
				}
				result.PayerProbes++
				if next != nil {
					// Payer service is an authoritative opportunity. A stale
					// grant hint cannot suppress it, and no grant wait is added.
					attemptCtx := context.WithValue(bounded, legacySettlementAdmissionHeadKey{}, true)
					if trace, _ := bounded.Value(legacyTargetTraceKey{}).(*legacyTargetTrace); trace != nil {
						attemptCtx = trace.selectTarget(attemptCtx, next.ContractId, false)
					}
					completed, busy, gate, err := flushLegacySettlementWithGrantWait(attemptCtx, next.ContractId, nil)
					if err != nil && bounded.Err() != nil {
						server.Raise(err)
					}
					result.Visited++
					result.PayerVisited++
					if completed && err == nil {
						result.Completed++
						result.PayerCompleted++
					}
					if busy {
						result.BusyOrGone++
						result.PayerBusyOrGone++
						switch gate {
						case legacySettlementBusyIntent:
							result.BusyIntentUnavailable++
						case legacySettlementBusyContract:
							result.BusyContractUnavailable++
						case legacySettlementBusyGrantSet:
							result.BusyGrantSetMismatch++
						case legacySettlementBusyAdmission:
							result.BusyAdmissionDeferred++
						}
					}
					if err != nil {
						result.Failed++
						result.PayerFailed++
						code, delay := "operational", 30*time.Second
						if errors.Is(err, errContractInsufficientEscrow) {
							code, delay = "accounting", 15*time.Minute
						}
						traceLegacySettlement(attemptCtx, "retry_state", "entered")
						server.Tx(bounded, func(tx server.PgTx) {
							server.RaisePgResult(tx.Exec(bounded, `UPDATE legacy_settlement_intent SET
							 failure_code=$2,next_attempt_time=clock_timestamp() AT TIME ZONE 'UTC'+$3::interval
							 WHERE contract_id=$1`, next.ContractId, code, delay.String()))
						}, server.TxReadCommitted, server.OptNoRetry())
						traceLegacySettlement(attemptCtx, "retry_state", "committed")
						result.PayerCursor.After = payer
						result.More = true
						return
					}
				}
				result.PayerCursor.After = payer
			}
		}
		fair := result.LegacySettlementFlushResult
		ordered, err := flushLegacySettlementsPage(ctx, bounded, shard, after, limit-fair.Visited, flushLegacySettlementWithGrantWait, flushLegacySettlementCohort)
		result.LegacySettlementFlushResult = ordered
		result.Visited += fair.Visited
		result.Completed += fair.Completed
		result.BusyOrGone += fair.BusyOrGone
		result.Failed += fair.Failed
		result.BusyIntentUnavailable += fair.BusyIntentUnavailable
		result.BusyContractUnavailable += fair.BusyContractUnavailable
		result.BusyGrantSetMismatch += fair.BusyGrantSetMismatch
		result.BusyAdmissionDeferred += fair.BusyAdmissionDeferred
		if err != nil && ordered.Visited == 0 {
			result.Cursor = after
		}
		result.More = result.More || result.PayerCursor != nil
		server.Raise(err)
	}, func(err error) {
		if result.Failed == 0 && (result.Visited > 0 || result.PayerProbes > 0) && ctx.Err() == nil &&
			bounded.Err() != nil && context.Cause(bounded) == errLegacySettlementPageBudget && isSettlementPageCancellation(err) {
			result.More = true
			return
		}
		returnErr = err
	})
	return
}

// The missing-key partial index fences discovery to 256 intents per shard.
// SKIP LOCKED avoids waiting for any financial owner. The contract lookup is a
// primary-key lateral point; registration changes no outcome, retry or money.
// Express the integer shard as a half-open range and retain both ordering keys.
// Equality would let the planner drop shard from ORDER BY and scan the global
// primary key while filtering registered rows instead of using the missing index.
const legacySettlementPayerRegistrationSql = `SELECT picked.contract_id,contract.payer
 FROM (SELECT contract_id FROM legacy_settlement_intent
 WHERE shard>=$1::smallint AND shard<($1::smallint+1) AND payer_network_id IS NULL
 ORDER BY shard,contract_id LIMIT 256 FOR UPDATE SKIP LOCKED) AS picked
 CROSS JOIN LATERAL (
 SELECT COALESCE(payer_network_id,
   CASE WHEN companion_contract_id IS NULL THEN source_network_id ELSE destination_network_id END) AS payer
 FROM transfer_contract WHERE contract_id=picked.contract_id OFFSET 0
 ) AS contract`

func registerLegacySettlementPayers(ctx context.Context, shard int) (registered int) {
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	server.Tx(bounded, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(bounded, `SET LOCAL statement_timeout='2s'`))
		type registration struct{ contractId, payerNetworkId server.Id }
		registrations := make([]registration, 0, legacySettlementPayerRegistrationLimit)
		rows, err := tx.Query(bounded, legacySettlementPayerRegistrationSql, shard)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var value registration
				server.Raise(rows.Scan(&value.contractId, &value.payerNetworkId))
				registrations = append(registrations, value)
			}
		})
		// One protocol batch retains point updates. A bulk UPDATE FROM may
		// choose a whole-table hash join despite bounded picked rows.
		if len(registrations) > 0 {
			server.BatchInTx(bounded, tx, func(batch server.PgBatch) {
				for _, value := range registrations {
					batch.Queue(`UPDATE legacy_settlement_intent SET payer_network_id=$2 WHERE contract_id=$1`, value.contractId, value.payerNetworkId)
				}
			})
		}
		registered = len(registrations)
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

const legacySettlementPayerBeginSql = `SELECT payer_network_id,statement_timestamp() AT TIME ZONE 'UTC'
 FROM legacy_settlement_intent WHERE shard=$1 AND payer_network_id IS NOT NULL
 ORDER BY payer_network_id DESC LIMIT 1`
const legacySettlementPayerNextSql = `SELECT payer_network_id FROM legacy_settlement_intent
 WHERE shard=$1 AND payer_network_id IS NOT NULL AND payer_network_id<=$2`

// Fence the single payer's oldest row before checking time eligibility. If its
// first row is in the future, every later row is too. The singleton UUID range
// retains payer ordering so the planner cannot substitute a global due-index
// walk that filters other payers. The limit prevents pushing the due predicate
// back into that choice, including when statistics predict no due rows.
const legacySettlementPayerIntentSql = `SELECT next_attempt_time,contract_id FROM (
 SELECT payer_network_id,next_attempt_time,contract_id FROM legacy_settlement_intent
 WHERE shard=$1 AND payer_network_id>=$2::uuid AND payer_network_id<=$2::uuid
 ORDER BY payer_network_id,next_attempt_time,contract_id LIMIT 1
 ) AS payer_head
 WHERE next_attempt_time<=$3
 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'`

func beginLegacySettlementPayerRound(ctx context.Context, shard int) (cursor *LegacySettlementPayerCursor) {
	defer enterLegacySettlementTiming(ctx, legacySettlementSelection)()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, legacySettlementPayerBeginSql, shard)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				cursor = &LegacySettlementPayerCursor{}
				server.Raise(rows.Scan(&cursor.End, &cursor.PassEndTime))
			}
		})
	})
	return
}

// Discovery deliberately has no due predicate: it seeks past the entire prior
// payer in the index. A separate exact-payer range then finds its oldest due
// intent. Thousands of future intents consume one empty probe, not a scan.
func nextLegacySettlementPayer(ctx context.Context, shard int, cursor *LegacySettlementPayerCursor) (payer *server.Id, next *LegacySettlementPosition) {
	defer enterLegacySettlementTiming(ctx, legacySettlementSelection)()
	server.Db(ctx, func(conn server.PgConn) {
		query := legacySettlementPayerNextSql
		args := []any{shard, cursor.End}
		if cursor.After != nil {
			query += ` AND payer_network_id>$3`
			args = append(args, *cursor.After)
		}
		query += ` ORDER BY payer_network_id LIMIT 1`
		rows, err := conn.Query(ctx, query, args...)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				payer = &server.Id{}
				server.Raise(rows.Scan(payer))
			}
		})
		if payer == nil {
			return
		}
		rows, err = conn.Query(ctx, legacySettlementPayerIntentSql, shard, *payer, cursor.PassEndTime)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				next = &LegacySettlementPosition{}
				server.Raise(rows.Scan(&next.NextAttemptTime, &next.ContractId))
			}
		})
	})
	return
}
