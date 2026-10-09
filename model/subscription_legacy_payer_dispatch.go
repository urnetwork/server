// Recurring shard tasks discover durable intents and wake bounded payer tasks.
// Discovery never debits grants and does not require a foreground shared row.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

var errLegacySettlementDispatchBudget = errors.New("legacy settlement dispatch budget")

// Identity is private task data. Cursor is a bounded compatibility-registration
// traversal; PayerCursor controls fair service independently of contract density.
type LegacySettlementDispatchResult struct {
	Private            bool                         `json:"_private_task_arguments"`
	PayerNetworkIds    []server.Id                  `json:"payer_network_ids"`
	Cursor             *LegacySettlementCursor      `json:"cursor,omitempty"`
	PayerCursor        *LegacySettlementPayerCursor `json:"payer_cursor,omitempty"`
	Probes             int                          `json:"probes"`
	Registered         int                          `json:"registered"`
	RegistrationFailed bool                         `json:"registration_failed"`
	More               bool                         `json:"more"`
}

// A fixed payer round and at most sixteen probes prevent a dense account from
// concealing a later one. Accepted results and scheduled owners commit together
// in the task Post; a crash before that handoff simply repeats discovery.
func DispatchLegacySettlementPayers(ctx context.Context, shard int, after *LegacySettlementCursor,
	payerAfter *LegacySettlementPayerCursor) (result LegacySettlementDispatchResult, returnErr error) {
	result, _, returnErr = DispatchLegacySettlementPayersWithReadiness(ctx, shard, after, payerAfter)
	return
}

// Carry readiness through the task result, including the financial fallback.
// Its existing 250ms budget includes the cache check or one catalog probe.
func DispatchLegacySettlementPayersWithReadiness(ctx context.Context, shard int, after *LegacySettlementCursor,
	payerAfter *LegacySettlementPayerCursor,
) (result LegacySettlementDispatchResult, readiness *LegacySettlementPayerIndexReadiness, returnErr error) {
	result.Private = true
	if shard < 0 || shard >= LegacySettlementShardCount {
		return result, nil, fmt.Errorf("invalid legacy settlement dispatch shard")
	}
	observation := legacySettlementPayerDueIndexObservation(ctx)
	readiness = &observation
	if observation.Outcome != "ready" {
		return result, readiness, ErrLegacySettlementPayerIndexUnavailable
	}
	bounded, cancel := context.WithTimeoutCause(ctx, 5*time.Second, errLegacySettlementDispatchBudget)
	defer cancel()
	result, returnErr = dispatchLegacySettlementPayersPage(ctx, bounded, shard, after, payerAfter,
		nextLegacySettlementPayer, registerLegacySettlementPayerDispatchPage)
	return
}

// A valid missing-key index gives registration its own finite progress lane.
// Rewalking a registered chronological prefix must not hold newer unregistered
// work behind that pass's fixed cutoff. An unavailable index keeps the original
// chronological fallback; both paths share the caller's registration budget.
func registerLegacySettlementPayerDispatchPage(ctx context.Context, shard int, after *LegacySettlementCursor) (*LegacySettlementCursor, int) {
	return registerLegacySettlementPayerDispatchPageWithReadiness(ctx, shard, after,
		cachedLegacySettlementPayerIndexesReady, readLegacySettlementPayerIndexesWithCache)
}

// Registration reuses the same full-index proof as dispatch; a cache miss keeps
// its fresh bounded catalog check. Neither path publishes or extends the proof.
// Invocation-local readers let controls force a transient refusal exactly.
func registerLegacySettlementPayerDispatchPageWithReadiness(ctx context.Context, shard int, after *LegacySettlementCursor,
	cached func(context.Context) bool, read func(context.Context) (bool, error),
) (*LegacySettlementCursor, int) {
	readiness := observeLegacySettlementPayerIndexWithCache(ctx, cached, read, time.Now)
	if readiness.Outcome == "ready" {
		return after, registerLegacySettlementPayers(ctx, shard)
	}
	return registerLegacySettlementPayerPage(ctx, shard, after)
}

// Only this page owns its time budget. A completed discovery prefix can be
// handed back when that budget ends; parent cancellation remains an error.
// Invocation-local operations permit deterministic database barriers.
func dispatchLegacySettlementPayersPage(ctx, bounded context.Context, shard int, after *LegacySettlementCursor,
	payerAfter *LegacySettlementPayerCursor,
	next func(context.Context, int, *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition),
	register func(context.Context, int, *LegacySettlementCursor) (*LegacySettlementCursor, int),
) (result LegacySettlementDispatchResult, returnErr error) {
	result.Private = true
	result.Cursor = after
	if payerAfter != nil {
		cursor := *payerAfter
		result.PayerCursor = &cursor
	}
	server.HandleError(func() {
		if result.PayerCursor == nil {
			result.PayerCursor = beginLegacySettlementPayerRound(bounded, shard)
		}
		for probe := 0; probe < legacySettlementPayerProbeLimit && result.PayerCursor != nil; probe++ {
			payer, ready := next(bounded, shard, result.PayerCursor)
			if payer == nil {
				result.PayerCursor = nil
				break
			}
			result.Probes++
			if ready != nil {
				result.PayerNetworkIds = append(result.PayerNetworkIds, *payer)
			}
			result.PayerCursor.After = payer
		}
		// Discover registered work before optional compatibility registration:
		// transaction cleanup can outlive its own query context. This prefix
		// retains scheduling custody if our page deadline expires in cleanup.
		// The chronological index also works while the NULL-payer index is
		// being repaired. Newly registered keys enter the next bounded turn.
		registrationCtx, registrationCancel := context.WithTimeout(bounded, 2*time.Second)
		server.HandleError(func() {
			defer registrationCancel()
			result.Cursor, result.Registered = register(registrationCtx, shard, after)
		}, func(error) {
			result.Cursor, result.Registered = after, 0
			result.RegistrationFailed = true
		})
		// HandleError contains rescue-handler panics. Propagate cancellation
		// from the normal path, including a cancellation during joined cleanup.
		// A preceding optional SQL refusal does not invalidate ready discovery.
		if bounded.Err() != nil {
			server.Raise(bounded.Err())
		}
		// Multiple dispatchers acquire payer task keys in the same order.
		slices.SortFunc(result.PayerNetworkIds, server.Id.Cmp)
		result.More = result.PayerCursor != nil || result.Registered > 0
	}, func(err error) {
		if ctx.Err() == nil && bounded.Err() != nil && context.Cause(bounded) == errLegacySettlementDispatchBudget &&
			result.Probes > 0 && isSettlementPageCancellation(err) {
			// The interrupted probe has not moved After. Publication of this
			// prefix and its successor belongs to the task's ordinary Post.
			slices.SortFunc(result.PayerNetworkIds, server.Id.Cmp)
			result.More = true
			return
		}
		returnErr = err
	})
	return
}

// At most 256 chronological due candidates are read, irrespective of NULL
// density. Only missing keys are updated, by primary key and without financial
// locks. The fixed cutoff/cursor survives task continuation. Future retries
// need no owner until they become due, and normal recurring discovery revisits.
func registerLegacySettlementPayerPage(ctx context.Context, shard int, after *LegacySettlementCursor) (next *LegacySettlementCursor, registered int) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
		query := `SELECT next_attempt_time,contract_id,payer_network_id,
 statement_timestamp() AT TIME ZONE 'UTC' FROM legacy_settlement_intent
 WHERE shard=$1 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'
 ORDER BY next_attempt_time,contract_id LIMIT 256`
		args := []any{shard}
		if after != nil {
			var passEnd any
			if !after.PassEndTime.IsZero() {
				passEnd = after.PassEndTime
			}
			query = `SELECT next_attempt_time,contract_id,payer_network_id,
 COALESCE($4::timestamp,statement_timestamp() AT TIME ZONE 'UTC') FROM legacy_settlement_intent
 WHERE shard=$1 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'
 AND next_attempt_time<=COALESCE($4::timestamp,statement_timestamp() AT TIME ZONE 'UTC')
 AND (next_attempt_time,contract_id)>($2,$3)
 ORDER BY next_attempt_time,contract_id LIMIT 256`
			args = append(args, after.NextAttemptTime, after.ContractId, passEnd)
		}
		var missing []server.Id
		count := 0
		rows, err := tx.Query(ctx, query, args...)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				position := &LegacySettlementCursor{}
				var payer *server.Id
				server.Raise(rows.Scan(&position.NextAttemptTime, &position.ContractId, &payer, &position.PassEndTime))
				next = position
				count++
				if payer == nil {
					missing = append(missing, position.ContractId)
				}
			}
		})
		slices.SortFunc(missing, server.Id.Cmp)
		if len(missing) > 0 {
			// Preserve indexed point updates in one protocol batch. Separate
			// round trips can exhaust the whole registration budget on a full
			// NULL prefix even when each individual statement is inexpensive.
			server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
				for _, id := range missing {
					batch.Queue(`UPDATE legacy_settlement_intent AS intent
 SET payer_network_id=(SELECT COALESCE(payer_network_id,
 CASE WHEN companion_contract_id IS NULL THEN source_network_id ELSE destination_network_id END)
 FROM transfer_contract WHERE contract_id=$1)
 WHERE intent.contract_id=$1 AND intent.payer_network_id IS NULL`, id).Exec(func(tag pgconn.CommandTag) error {
						registered += int(tag.RowsAffected())
						return nil
					})
				}
			})
		}
		if count < legacySettlementPayerRegistrationLimit {
			next = nil
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}
